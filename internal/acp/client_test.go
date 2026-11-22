package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ArcheMind/agentx/internal/runtime"
)

func pipeACP(t *testing.T, handler func(req map[string]any, send func(any))) (*Client, *[]string) {
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
	c := New(stream, Handlers{
		Agent: "codex",
		OnLine: func(line string) {
			mu.Lock()
			live = append(live, line)
			mu.Unlock()
		},
	})
	t.Cleanup(func() { stream.Kill() })
	return c, &live
}

func TestStartNegotiatesResumeSystemPromptConfigAndSteer(t *testing.T) {
	var methods []string
	var setConfigs []string
	var trusted bool
	c, live := pipeACP(t, func(req map[string]any, send func(any)) {
		method, _ := req["method"].(string)
		if method != "" {
			methods = append(methods, method)
		}
		id := req["id"]
		switch method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
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
	if !trusted || info.SessionID != "s1" || info.PromptPrefix != "" || !info.Capabilities.Steering {
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
