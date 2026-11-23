package acp

import (
	"encoding/json"
	"strings"
	"sync"
)

// v2Turn implements ACP v2 turn completion using standard state_update
// notifications (`state == "idle"` with an empty `background` list).
// No vendor-specific metadata or settle timers are needed in ACP v2.
type v2Turn struct {
	mu              sync.Mutex
	sessionID       string
	active          bool
	prompts         map[int]bool
	sawStopReason   bool
	stopReason      string
	usage           json.RawMessage
	idleError       *Error
	backgroundCount int

	doneCh     chan struct{}
	doneClosed bool
	res        PromptResult
	onEnd      func(PromptResult)
}

func newV2Turn() *v2Turn {
	return &v2Turn{
		prompts: map[int]bool{},
	}
}

func (t *v2Turn) Reset(sessionID string, onEnd func(PromptResult)) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = sessionID
	t.active = true
	t.prompts = map[int]bool{}
	t.sawStopReason = false
	t.stopReason = ""
	t.usage = nil
	t.idleError = nil
	t.backgroundCount = 0
	t.doneCh = make(chan struct{})
	t.doneClosed = false
	t.res = PromptResult{}
	t.onEnd = onEnd
	return t.doneCh
}

func (t *v2Turn) OnPromptSent(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prompts[id] = true
}

func (t *v2Turn) OnPromptResponse(id int, _ json.RawMessage, rpcErr *Error) {
	t.mu.Lock()
	if !t.prompts[id] {
		t.mu.Unlock()
		return
	}
	delete(t.prompts, id)
	if rpcErr != nil && t.active {
		cb := t.settleLocked(PromptResult{ID: id, Err: rpcErr})
		t.mu.Unlock()
		if cb != nil {
			cb(t.Result())
		}
		return
	}
	t.mu.Unlock()
}

type v2StateEnvelope struct {
	Method string `json:"method"`
	Params struct {
		Session string `json:"sessionId"`
		Update  struct {
			Type       string            `json:"sessionUpdate"`
			State      string            `json:"state"`
			StopReason string            `json:"stopReason"`
			Error      *Error            `json:"error"`
			Usage      json.RawMessage   `json:"usage"`
			Background []json.RawMessage `json:"background"`
		} `json:"update"`
	} `json:"params"`
}

func (t *v2Turn) Observe(line string) {
	if !strings.Contains(line, `"state_update"`) {
		return
	}
	var ev v2StateEnvelope
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Method != "session/update" {
		return
	}
	u := ev.Params.Update
	if u.Type != "state_update" {
		return
	}

	t.mu.Lock()
	if t.sessionID != "" && ev.Params.Session != "" && ev.Params.Session != t.sessionID {
		t.mu.Unlock()
		return
	}
	t.backgroundCount = len(u.Background)
	if u.State == "idle" {
		if u.StopReason != "" {
			t.sawStopReason = true
			t.stopReason = u.StopReason
			t.usage = u.Usage
			t.idleError = u.Error
		}
		if t.active && t.sawStopReason && (t.backgroundCount == 0 || t.stopReason == "cancelled" || t.stopReason == "error") {
			res := PromptResult{
				StopReason: t.stopReason,
				Usage:      t.usage,
				Err:        t.idleError,
			}
			if res.StopReason == "error" && res.Err == nil {
				res.Err = &Error{Code: -32000, Message: "agent turn failed"}
			}
			cb := t.settleLocked(res)
			t.mu.Unlock()
			if cb != nil {
				cb(res)
			}
			return
		}
	}
	t.mu.Unlock()
}

func (t *v2Turn) settleLocked(res PromptResult) func(PromptResult) {
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

func (t *v2Turn) Result() PromptResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.res
}

func (t *v2Turn) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = false
	t.onEnd = nil
}
