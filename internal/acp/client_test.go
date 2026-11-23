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

	"github.com/ArcheMind/agentx/internal/runtime"
)

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

func TestV1StartNegotiatesResumeSystemPromptConfigAndSteer(t *testing.T) {
	var setConfigs []string
	var trusted bool
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
		case "_codex/hooks/list":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"hooks": []map[string]any{{"key": "k1", "currentHash": "h1", "trustStatus": "untrusted"}}}})
		case "_codex/hooks/trust":
			trusted = true
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/resume":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"sessionId": "s1",
				"configOptions": []map[string]any{
					{"id": "model", "category": "model"},
					{"id": "reasoning_effort", "category": "thought_level"},
					{"id": "mode", "category": "mode"},
				},
			}})
		case "session/set_config_option":
			p := req["params"].(map[string]any)
			if _, hasType := p["type"]; hasType {
				t.Errorf("v1 set_config_option must not send type field: %v", p)
			}
			setConfigs = append(setConfigs, p["configId"].(string)+"="+p["value"].(string))
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "_session/steering":
			p := req["params"].(map[string]any)
			txt := p["prompt"].([]any)[0].(map[string]any)["text"].(string)
			if strings.Contains(txt, "IDLE") {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": "promptRequired"}})
			} else {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": "injected"}})
			}
		case "session/prompt":
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "OK"}}}})
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
		}
	})

	ctx := context.Background()
	info, err := c.Start(ctx, StartOptions{
		ClientVersion: "1.0", SessionID: "s1", Params: map[string]any{"cwd": "/ws"},
		SystemPrompt: "role", Model: "m1", Effort: "high", Mode: "agent-full-access",
		TrustCodexHooks: true, Cwd: "/ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trusted || info.ProtocolVersion != 1 || info.SessionID != "s1" || info.PromptPrefix != "" || !info.Capabilities.Steering {
		t.Fatalf("info=%+v trusted=%v", info, trusted)
	}
	if got := strings.Join(setConfigs, ","); got != "mode=agent-full-access,model=m1,reasoning_effort=high" {
		t.Fatalf("configs=%s", got)
	}
	if disp, _, err := c.Steer(ctx, "s1", "adjust"); err != nil || disp != "steered" {
		t.Fatalf("steer=%s err=%v", disp, err)
	}
	if disp, _, err := c.Steer(ctx, "s1", "IDLE"); err != nil || disp != "queued" {
		t.Fatalf("idle steer=%s err=%v", disp, err)
	}
	res, err := c.PromptSync(ctx, "s1", "hi")
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(strings.Join(*live, "\n"), "OK") {
		t.Fatalf("missing live update: %v", *live)
	}
}

func TestV2StartConfigOptionsAndStateUpdateBackgroundLifecycle(t *testing.T) {
	var setConfigs []string
	turnDone := make(chan PromptResult, 1)
	c, live := pipeACPWithHandlers(t, func(h *Handlers) {
		h.Agent = "claude"
		h.OnTurnEnd = func(res PromptResult) {
			turnDone <- res
		}
	}, func(req map[string]any, send func(any)) {
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": 2,
				"capabilities": map[string]any{
					"session": map[string]any{
						"systemPrompt": map[string]any{"mode": map[string]any{"override": map[string]any{}}},
					},
				},
			}})
		case "session/resume":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"sessionId": "s2",
				"configOptions": []map[string]any{
					{"configId": "model", "type": "select", "category": "model"},
					{"configId": "effort", "type": "select", "category": "thought_level"},
					{"configId": "mode", "type": "select", "category": "mode"},
				},
			}})
		case "session/set_config_option":
			p := req["params"].(map[string]any)
			if p["type"] != "id" {
				t.Errorf("v2 set_config_option must send type=id: %v", p)
			}
			setConfigs = append(setConfigs, p["configId"].(string)+"="+p["value"].(string))
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "session/prompt":
			// v2 session/prompt returns messageId immediately.
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"messageId": "m1"}})
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s2",
				"update":    map[string]any{"sessionUpdate": "state_update", "state": "running"},
			}})
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s2",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "WAIT;"}},
			}})
			// Foreground goes idle while background task is still running -> must NOT settle yet.
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "s2",
				"update": map[string]any{
					"sessionUpdate": "state_update",
					"state":         "idle",
					"stopReason":    "end_turn",
					"background":    []map[string]any{{"type": "tool_call", "toolCallId": "bg1"}},
				},
			}})
			go func() {
				time.Sleep(30 * time.Millisecond)
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "s2",
					"update":    map[string]any{"sessionUpdate": "state_update", "state": "running", "background": []map[string]any{}},
				}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "s2",
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "DONE"}},
				}})
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "s2",
					"update":    map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn", "background": []any{}},
				}})
			}()
		}
	})

	ctx := context.Background()
	info, err := c.Start(ctx, StartOptions{
		ClientVersion: "1.0", SessionID: "s2", Params: map[string]any{"cwd": "/ws"},
		SystemPrompt: "role", Model: "claude-sonnet", Effort: "high", Mode: "bypassPermissions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != 2 || info.PromptPrefix != "" || !info.Capabilities.Resume {
		t.Fatalf("unexpected v2 info=%+v", info)
	}
	if got := strings.Join(setConfigs, ","); got != "mode=bypassPermissions,model=claude-sonnet,effort=high" {
		t.Fatalf("configs=%s", got)
	}

	res, err := c.PromptSync(ctx, "s2", "run")
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	joined := strings.Join(*live, "\n")
	if !strings.Contains(joined, "DONE") {
		t.Fatalf("PromptSync returned before background work completed: %s", joined)
	}

	// Also verify SendPrompt + OnTurnEnd works on v2.
	c.ResetTurn("s2")
	if _, err := c.SendPrompt("s2", "run2"); err != nil {
		t.Fatal(err)
	}
	select {
	case endRes := <-turnDone:
		if endRes.StopReason != "end_turn" {
			t.Fatalf("unexpected OnTurnEnd res=%+v", endRes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for v2 OnTurnEnd")
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

	res, err := c.PromptSync(context.Background(), "s1", "run tests")
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
	res, err := c.PromptSync(ctx, "s1", "start server")
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("expected immediate return for daemon task, got res=%+v err=%v", res, err)
	}
}

func TestV1SendPromptOnTurnEndOrderedAfterWakeupChunks(t *testing.T) {
	endCh := make(chan PromptResult, 1)
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
		h.OnTurnEnd = func(res PromptResult) {
			endCh <- res
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

	c.ResetTurn("s1")
	if _, err := c.SendPrompt("s1", "go"); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-endCh:
		if res.StopReason != "end_turn" {
			t.Fatalf("unexpected res=%+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnTurnEnd")
	}
	mu.Lock()
	got := text.String()
	mu.Unlock()
	if got != "WAIT;WOKE" {
		t.Fatalf("expected WAIT;WOKE before OnTurnEnd, got %q", got)
	}
}
