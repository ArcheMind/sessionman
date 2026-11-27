package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

type immediateReplyWriter struct {
	write func([]byte) (int, error)
}

func (w immediateReplyWriter) Write(p []byte) (int, error) { return w.write(p) }
func (w immediateReplyWriter) Close() error                { return nil }

func TestPromptResponseBeforeWriteReturns(t *testing.T) {
	outR, outW := io.Pipe()
	processed := make(chan struct{})
	writer := immediateReplyWriter{write: func(p []byte) (int, error) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(p, &req); err != nil {
			return 0, err
		}
		if req.Method != "session/prompt" {
			return len(p), nil
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
		if _, err := outW.Write(append(raw, '\n')); err != nil {
			return 0, err
		}
		// The marker is handled after readLoop consumes the preceding response.
		if _, err := io.WriteString(outW, "{\"method\":\"test/response_processed\"}\n"); err != nil {
			return 0, err
		}
		<-processed
		return len(p), nil
	}}
	stream := runtime.NewPipeStream(writer, outR, nil, func() { _ = outW.Close() }, func() error { return nil })
	t.Cleanup(func() { _ = outW.Close(); _ = outR.Close() })
	c := New(stream, Handlers{OnLine: func(line string) {
		if strings.Contains(line, "test/response_processed") {
			close(processed)
		}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := c.Prompt(ctx, "s1", "hello", nil)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("fast prompt response lost: result=%+v err=%v", res, err)
	}
}

func pipeACPWithHandlers(t *testing.T, modify func(*Handlers), handler func(req map[string]any, send func(any))) (*Client, *[]string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var mu sync.Mutex
	var live []string
	send := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = io.WriteString(outW, string(raw)+"\n")
	}
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var req map[string]any
			if json.Unmarshal(scanner.Bytes(), &req) == nil {
				handler(req, send)
			}
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })
	h := Handlers{
		Agent: "codex",
		OnLine: func(line string) {
			mu.Lock()
			live = append(live, line)
			mu.Unlock()
		},
	}
	if modify != nil {
		modify(&h)
	}
	c := New(stream, h)
	t.Cleanup(func() { stream.Kill() })
	return c, &live
}

func pipeACP(t *testing.T, handler func(req map[string]any, send func(any))) (*Client, *[]string) {
	return pipeACPWithHandlers(t, nil, handler)
}

func TestStartNegotiatesResumeSystemPromptAndConfig(t *testing.T) {
	var setConfigs []string
	c, live := pipeACP(t, func(req map[string]any, send func(any)) {
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"sessionCapabilities": map[string]any{
						"resume":       map[string]any{},
						"systemPrompt": map[string]any{},
					},
				},
				"_meta": map[string]any{"steering": map[string]any{"supported": true}},
			}})
		case "session/resume":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"sessionId": "s1",
				"configOptions": []map[string]any{
					{"id": "model", "category": "model"},
					{"id": "reasoning_effort", "category": "thought_level"},
				},
			}})
		case "session/set_config_option":
			p := req["params"].(map[string]any)
			setConfigs = append(setConfigs, p["configId"].(string)+"="+p["value"].(string))
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/prompt":
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "OK"}}}})
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
		}
	})

	ctx := context.Background()
	info, err := c.Start(ctx, StartOptions{SessionID: "s1", Params: map[string]any{"cwd": "/ws"}, SystemPrompt: "role", Model: "m1", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if info.SessionID != "s1" || info.PromptPrefix != "" || !info.Capabilities.Steering {
		t.Fatalf("info=%+v", info)
	}
	if got := strings.Join(setConfigs, ","); got != "model=m1,reasoning_effort=high" {
		t.Fatalf("configs=%s", got)
	}
	res, err := c.Prompt(ctx, "s1", "hi", nil)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(strings.Join(*live, "\n"), "OK") {
		t.Fatalf("missing live update: %v", *live)
	}
}

func TestStartLoadDiscardsReplayAndRejectsSessionReplacement(t *testing.T) {
	c, live := pipeACP(t, func(req map[string]any, send func(any)) {
		id := req["id"]
		switch req["method"] {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"agentCapabilities": map[string]any{"loadSession": true}}})
		case "session/load":
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "OLD"}}}})
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": "other"}})
		}
	})
	if _, err := c.Start(context.Background(), StartOptions{SessionID: "s1", Params: map[string]any{"cwd": "/ws"}}); err == nil || !strings.Contains(err.Error(), "different native session") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	if len(*live) != 0 {
		t.Fatalf("replayed update leaked: %v", *live)
	}
}

func TestV1AgyWaitsForBackgroundToolCallAndFollowUpSteps(t *testing.T) {
	c, live := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "agy"
		h.BackgroundSettleWindow = 40 * time.Millisecond
	}, func(req map[string]any, send func(any)) {
		if req["method"] != "session/prompt" {
			return
		}
		id := req["id"]
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    "call_bg",
				"status":        "in_progress",
				"rawInput":      map[string]any{"CommandLine": "swift test", "WaitMsBeforeAsync": 10000},
			},
		}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "WAIT"}},
		}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000},
		}})
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})

		go func() {
			time.Sleep(30 * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_bg", "status": "completed"},
			}})
			time.Sleep(60 * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "usage_update", "used": 200, "size": 1000},
			}})
			time.Sleep(5 * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_step2", "status": "in_progress"},
			}})
			time.Sleep(50 * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_step2", "status": "completed"},
			}})
			time.Sleep(30 * time.Millisecond)
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ALL_DONE"}},
			}})
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "usage_update", "used": 300, "size": 1000},
			}})
		}()
	})

	res, err := c.Prompt(context.Background(), "s1", "run tests", nil)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	joined := strings.Join(*live, "\n")
	if !strings.Contains(joined, "ALL_DONE") {
		t.Fatalf("returned before background wakeup finished; live=%s", joined)
	}
}

func TestV1AgyDoesNotWaitForDaemonToolCall(t *testing.T) {
	c, _ := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "agy"
		h.BackgroundSettleWindow = 5 * time.Second
	}, func(req map[string]any, send func(any)) {
		if req["method"] != "session/prompt" {
			return
		}
		id := req["id"]
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    "call_daemon",
				"status":        "in_progress",
				"rawInput":      map[string]any{"CommandLine": "npm run dev", "IsDaemon": true},
			},
		}})
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := c.Prompt(ctx, "s1", "start server", nil)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("expected immediate return for daemon task, got res=%+v err=%v", res, err)
	}
}

func TestPromptReturnsAfterWakeupChunks(t *testing.T) {
	var mu sync.Mutex
	var text strings.Builder
	c, _ := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "agy"
		h.BackgroundSettleWindow = 30 * time.Millisecond
		h.OnLine = func(line string) {
			_, chunk, _, ok := ParseLine(line)
			if ok && chunk != "" {
				mu.Lock()
				text.WriteString(chunk)
				mu.Unlock()
			}
		}
	}, func(req map[string]any, send func(any)) {
		if req["method"] != "session/prompt" {
			return
		}
		id := req["id"]
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_1", "status": "in_progress"},
		}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "WAIT;"}},
		}})
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed"},
		}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "WOKE"}},
		}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000},
		}})
	})

	res, err := c.Prompt(context.Background(), "s1", "go", nil)
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	mu.Lock()
	got := text.String()
	mu.Unlock()
	if got != "WAIT;WOKE" {
		t.Fatalf("expected WAIT;WOKE before Prompt returns, got %q", got)
	}
}

func TestV1SteerActiveDuringBackgroundWaitAndAfterSettle(t *testing.T) {
	var promptMu sync.Mutex
	var prompts []string
	bgEntered := make(chan struct{}, 1)
	steerReceived := make(chan struct{}, 1)

	c, _ := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "agy"
		h.BackgroundSettleWindow = 40 * time.Millisecond
	}, func(req map[string]any, send func(any)) {
		if req["method"] != "session/prompt" {
			return
		}
		id := req["id"]
		txt := req["params"].(map[string]any)["prompt"].([]any)[0].(map[string]any)["text"].(string)
		promptMu.Lock()
		prompts = append(prompts, txt)
		n := len(prompts)
		promptMu.Unlock()

		if n == 1 {
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s1",
				"update":    map[string]any{"sessionUpdate": "tool_call", "toolCallId": "bg1", "status": "in_progress"},
			}})
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
			bgEntered <- struct{}{}
			return
		}
		// Second prompt is the mid-turn steer arriving during background wait.
		steerReceived <- struct{}{}
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update":    map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "bg1", "status": "completed"},
		}})
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
	})

	errCh := make(chan error, 1)
	go func() {
		res, err := c.Prompt(context.Background(), "s1", "initial", nil)
		if err != nil {
			errCh <- err
			return
		}
		if res.StopReason != "end_turn" {
			errCh <- context.DeadlineExceeded
			return
		}
		errCh <- nil
	}()

	<-bgEntered
	// Wait briefly to ensure v1Turn has processed end_turn and entered waiting state.
	time.Sleep(15 * time.Millisecond)

	disp, err := c.SteerActive(context.Background(), "s1", "mid-turn steer")
	if err != nil || disp != "steered" {
		t.Fatalf("expected steered during background wait, got disp=%q err=%v", disp, err)
	}
	<-steerReceived
	if err := <-errCh; err != nil {
		t.Fatalf("PromptSync failed: %v", err)
	}

	// After settlement, SteerActive must report idle and NOT send another prompt.
	disp, err = c.SteerActive(context.Background(), "s1", "too late")
	if err != nil || disp != "idle" {
		t.Fatalf("expected idle after settlement, got disp=%q err=%v", disp, err)
	}
	promptMu.Lock()
	defer promptMu.Unlock()
	if len(prompts) != 2 {
		t.Fatalf("expected exactly 2 prompts, got %v", prompts)
	}
}

func TestSteerActiveWithSteeringCapabilityAndCancel(t *testing.T) {
	cancelSeen := make(chan struct{}, 1)
	var steerCalls int
	var promptID any
	var mu sync.Mutex

	c, _ := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "agy"
	}, func(req map[string]any, send func(any)) {
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": 1,
				"_meta":           map[string]any{"steering": map[string]any{"supported": true}},
			}})
		case "session/new":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"sessionId": "s2"}})
		case "session/prompt":
			mu.Lock()
			promptID = id
			mu.Unlock()
		case "_session/steering":
			mu.Lock()
			steerCalls++
			mu.Unlock()
			txt := req["params"].(map[string]any)["prompt"].([]any)[0].(map[string]any)["text"].(string)
			if txt == "idle-case" {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": "promptRequired"}})
			} else {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": "injected"}})
			}
		case "session/cancel":
			cancelSeen <- struct{}{}
			mu.Lock()
			pid := promptID
			mu.Unlock()
			send(map[string]any{"jsonrpc": "2.0", "id": pid, "result": map[string]any{"stopReason": "cancelled"}})
		}
	})

	ctx := context.Background()
	info, err := c.Start(ctx, StartOptions{Params: map[string]any{"cwd": "/ws"}})
	if err != nil || !info.Capabilities.Steering {
		t.Fatalf("start info=%+v err=%v", info, err)
	}

	ready := make(chan struct{})
	resCh := make(chan PromptResult, 1)
	go func() {
		res, _ := c.Prompt(ctx, "s2", "go", func() { close(ready) })
		resCh <- res
	}()
	<-ready

	if disp, err := c.SteerActive(ctx, "s2", "adjust"); err != nil || disp != "steered" {
		t.Fatalf("steer=%q err=%v", disp, err)
	}
	if disp, err := c.SteerActive(ctx, "s2", "idle-case"); err != nil || disp != "idle" {
		t.Fatalf("idle steer=%q err=%v", disp, err)
	}
	if err := c.Cancel("s2"); err != nil {
		t.Fatalf("cancel err=%v", err)
	}
	<-cancelSeen
	res := <-resCh
	if res.StopReason != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", res)
	}

	// After settlement, SteerActive returns idle without sending another RPC.
	mu.Lock()
	before := steerCalls
	mu.Unlock()
	if disp, err := c.SteerActive(ctx, "s2", "after-settle"); err != nil || disp != "idle" {
		t.Fatalf("after-settle steer=%q err=%v", disp, err)
	}
	mu.Lock()
	after := steerCalls
	mu.Unlock()
	if after != before {
		t.Fatalf("SteerActive sent RPC after turn settled")
	}
}
