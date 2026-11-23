package acp

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// DefaultBackgroundSettleWindow is how long the ACP v1 client (used for AGY)
// waits after a post-end_turn usage_update (with no pending background tasks)
// to confirm no immediate tool_call follows from the same LLM response.
const DefaultBackgroundSettleWindow = 750 * time.Millisecond

// v1Turn implements ACP v1 turn completion (synchronous session/prompt
// response) plus the AGY-specific post-end_turn background tool tracker.
type v1Turn struct {
	mu           sync.Mutex
	sessionID    string
	settleWindow time.Duration

	active      bool
	prompts     map[int]bool
	results     []PromptResult
	lastResult  PromptResult
	inFlight    map[string]bool
	daemons     map[string]bool
	waiting     bool
	settleTimer *time.Timer
	settleGen   uint64

	doneCh     chan struct{}
	doneClosed bool
	res        PromptResult
	onEnd      func(PromptResult)
}

func newV1Turn(settleWindow time.Duration) *v1Turn {
	if settleWindow <= 0 {
		settleWindow = DefaultBackgroundSettleWindow
	}
	return &v1Turn{
		settleWindow: settleWindow,
		prompts:      map[int]bool{},
		inFlight:     map[string]bool{},
		daemons:      map[string]bool{},
	}
}

func (t *v1Turn) Reset(sessionID string, onEnd func(PromptResult)) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopSettleLocked()
	t.sessionID = sessionID
	t.active = true
	t.prompts = map[int]bool{}
	t.results = nil
	t.lastResult = PromptResult{}
	t.inFlight = map[string]bool{}
	t.daemons = map[string]bool{}
	t.waiting = false
	t.doneCh = make(chan struct{})
	t.doneClosed = false
	t.res = PromptResult{}
	t.onEnd = onEnd
	return t.doneCh
}

func (t *v1Turn) OnPromptSent(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prompts[id] = true
	// A concurrent steer prompt disarms any post-end_turn wait without discarding
	// still-running background tools.
	t.stopSettleLocked()
	t.waiting = false
}

func (t *v1Turn) OnPromptResponse(id int, result json.RawMessage, rpcErr *Error) {
	t.mu.Lock()
	if !t.prompts[id] {
		t.mu.Unlock()
		return
	}
	delete(t.prompts, id)
	res := PromptResult{ID: id, Err: rpcErr}
	if rpcErr == nil {
		var body struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(result, &body); err != nil {
			res.Err = &Error{Code: -32603, Message: "invalid prompt result: " + err.Error()}
		} else {
			res.StopReason = body.StopReason
			res.Usage = body.Usage
		}
	}
	t.results = append(t.results, res)
	t.lastResult = res
	if !t.active || len(t.prompts) > 0 {
		t.mu.Unlock()
		return
	}
	finalRes := t.mergedResultLocked()
	if finalRes.Err != nil || finalRes.StopReason != "end_turn" || len(t.inFlight) == 0 {
		cb := t.settleLocked(finalRes)
		t.mu.Unlock()
		if cb != nil {
			cb(finalRes)
		}
		return
	}
	// AGY returned end_turn while non-daemon tool calls are still in_progress.
	t.lastResult = finalRes
	t.stopSettleLocked()
	t.waiting = true
	t.mu.Unlock()
}

func (t *v1Turn) mergedResultLocked() PromptResult {
	if len(t.results) <= 1 {
		return t.lastResult
	}
	merged := t.lastResult
	for _, r := range t.results {
		if r.Err != nil {
			merged.Err = r.Err
		} else if r.StopReason != "end_turn" && merged.Err == nil {
			merged.StopReason = r.StopReason
		}
	}
	usage, _ := json.Marshal(struct {
		Prompts []PromptResult `json:"prompts"`
	}{t.results})
	merged.Usage = usage
	return merged
}

type v1UpdateEnvelope struct {
	Method string `json:"method"`
	Params struct {
		Session string `json:"sessionId"`
		Update  struct {
			Type       string `json:"sessionUpdate"`
			ToolCallID string `json:"toolCallId"`
			Status     string `json:"status"`
			RawInput   struct {
				IsDaemon bool `json:"IsDaemon"`
			} `json:"rawInput"`
			Args struct {
				IsDaemon bool `json:"IsDaemon"`
			} `json:"args"`
		} `json:"update"`
	} `json:"params"`
}

func (t *v1Turn) Observe(line string) {
	if !strings.Contains(line, `"session/update"`) {
		return
	}
	var ev v1UpdateEnvelope
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Method != "session/update" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessionID != "" && ev.Params.Session != "" && ev.Params.Session != t.sessionID {
		return
	}
	u := ev.Params.Update
	switch u.Type {
	case "tool_call":
		if u.ToolCallID == "" {
			return
		}
		if u.RawInput.IsDaemon || u.Args.IsDaemon {
			t.daemons[u.ToolCallID] = true
			delete(t.inFlight, u.ToolCallID)
			return
		}
		if isTerminalToolStatus(u.Status) {
			delete(t.inFlight, u.ToolCallID)
			return
		}
		t.inFlight[u.ToolCallID] = true
		if t.waiting {
			t.stopSettleLocked()
		}

	case "tool_call_update":
		if u.ToolCallID == "" || t.daemons[u.ToolCallID] {
			return
		}
		if u.RawInput.IsDaemon || u.Args.IsDaemon {
			t.daemons[u.ToolCallID] = true
			delete(t.inFlight, u.ToolCallID)
			return
		}
		if isTerminalToolStatus(u.Status) {
			wasInFlight := t.inFlight[u.ToolCallID]
			delete(t.inFlight, u.ToolCallID)
			if t.waiting && wasInFlight && len(t.inFlight) == 0 {
				t.stopSettleLocked()
			}
		} else if u.Status == "pending" || u.Status == "in_progress" {
			t.inFlight[u.ToolCallID] = true
			if t.waiting {
				t.stopSettleLocked()
			}
		}

	case "agent_message_chunk", "agent_thought_chunk":
		if t.waiting && t.settleTimer != nil {
			t.stopSettleLocked()
		}

	case "usage_update":
		if t.waiting && len(t.inFlight) == 0 {
			t.armSettleLocked()
		}
	}
}

func (t *v1Turn) stopSettleLocked() {
	if t.settleTimer != nil {
		t.settleTimer.Stop()
		t.settleTimer = nil
	}
	t.settleGen++
}

func (t *v1Turn) armSettleLocked() {
	t.stopSettleLocked()
	gen := t.settleGen
	t.settleTimer = time.AfterFunc(t.settleWindow, func() {
		t.mu.Lock()
		if !t.active || !t.waiting || t.settleGen != gen || len(t.inFlight) > 0 {
			t.mu.Unlock()
			return
		}
		t.settleTimer = nil
		res := t.lastResult
		cb := t.settleLocked(res)
		t.mu.Unlock()
		if cb != nil {
			cb(res)
		}
	})
}

func (t *v1Turn) settleLocked(res PromptResult) func(PromptResult) {
	t.stopSettleLocked()
	t.waiting = false
	t.active = false
	t.res = res
	if t.doneCh != nil && !t.doneClosed {
		t.doneClosed = true
		close(t.doneCh)
	}
	cb := t.onEnd
	t.onEnd = nil
	return cb
}

func (t *v1Turn) Result() PromptResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.res
}

func (t *v1Turn) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopSettleLocked()
	t.waiting = false
	t.active = false
	t.onEnd = nil
}

func isTerminalToolStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}
