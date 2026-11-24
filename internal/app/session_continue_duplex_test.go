package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArcheMind/agentx/internal/runtime"
	"github.com/ArcheMind/agentx/internal/sessions"
)

type duplexEvent struct {
	Type        string           `json:"type"`
	ID          string           `json:"id"`
	Disposition string           `json:"disposition"`
	Error       string           `json:"error"`
	Message     sessions.Message `json:"message"`
	Session     sessions.Session `json:"session"`
}

func decodeDuplexEvents(t *testing.T, output string) ([]sessions.Message, []duplexEvent, *sessions.Session) {
	t.Helper()
	var messages []sessions.Message
	var acks []duplexEvent
	var result *sessions.Session
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev duplexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		if result != nil {
			t.Fatalf("event after terminal result: %s", line)
		}
		switch ev.Type {
		case "message":
			messages = append(messages, ev.Message)
		case "steer_ack":
			acks = append(acks, ev)
		case "result":
			s := ev.Session
			result = &s
		default:
			t.Fatalf("unknown event type %q in %s", ev.Type, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return messages, acks, result
}

func TestSessionContinueDuplexFlagValidation(t *testing.T) {
	workspace := testWorkspace(t)
	transcript := continueTranscript(t, workspace)

	t.Run("requires stream", func(t *testing.T) {
		application, _, _ := continueApp(t, "claude", "", transcript)
		err := application.Run(context.Background(), []string{"session", "continue", "claude", "--duplex"})
		if err == nil || !strings.Contains(err.Error(), "--duplex requires --stream") {
			t.Fatalf("expected --duplex requires --stream error, got %v", err)
		}
	})

	t.Run("rejects file argument", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "transcript.json")
		if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		application, _, _ := continueApp(t, "claude", "", "")
		err := application.Run(context.Background(), []string{"session", "continue", "claude", path, "--stream", "--duplex"})
		if err == nil || !strings.Contains(err.Error(), "--duplex cannot be combined with a transcript file") {
			t.Fatalf("expected file conflict error, got %v", err)
		}
	})
}

// duplexACPRunner simulates an ACP v1 or v2 agent that supports mid-turn steer
// and cancel while writing canonical records to the native session store.
type duplexACPRunner struct {
	*continueRunner
	protocolVersion int
	steeringCap     bool
	cancelCh        chan struct{}
	promptStarted   chan struct{}
	releaseTurn     chan struct{}
	onSteer         func(sessionID, text string, plan runtime.CommandPlan) (outcome string)
}

func (r *duplexACPRunner) StartStream(plan runtime.CommandPlan) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var writeMu sync.Mutex
	emit := func(v any) {
		raw, _ := json.Marshal(v)
		writeMu.Lock()
		_, _ = io.WriteString(outW, string(raw)+"\n")
		writeMu.Unlock()
	}

	go func() {
		defer outW.Close()
		var sessionID string
		var promptID json.RawMessage
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					SessionID string `json:"sessionId"`
					Prompt    []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				} `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
			}
			switch req.Method {
			case "initialize":
				res := map[string]any{
					"protocolVersion": r.protocolVersion,
					"agentCapabilities": map[string]any{
						"loadSession": true,
						"sessionCapabilities": map[string]any{
							"resume":       map[string]any{},
							"systemPrompt": map[string]any{},
						},
					},
				}
				if r.steeringCap {
					res["_meta"] = map[string]any{"steering": map[string]any{"supported": true}}
				}
				emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			case "_codex/hooks/list":
				emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"hooks": []any{}}})
			case "session/new", "session/resume", "session/load":
				sessionID = req.Params.SessionID
				if sessionID == "" {
					sessionID = r.fresh
				}
				emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": sessionID}})
			case "session/set_config_option", "session/set_model":
				emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			case "session/prompt":
				promptText := ""
				if len(req.Params.Prompt) > 0 {
					promptText = req.Params.Prompt[0].Text
				}
				if len(promptID) == 0 {
					promptID = req.ID
					r.writeInitialStore(plan, sessionID, promptText)
					if r.protocolVersion >= 2 {
						emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"messageId": "m1"}})
						emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
							"sessionId": sessionID,
							"update":    map[string]any{"sessionUpdate": "state_update", "state": "running", "background": []map[string]any{{"type": "tool_call", "toolCallId": "bg1"}}},
						}})
					} else {
						emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
							"sessionId": sessionID,
							"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "working"}},
						}})
					}
					if r.promptStarted != nil {
						select {
						case r.promptStarted <- struct{}{}:
						default:
						}
					}
					go func(sid string, pid json.RawMessage) {
						select {
						case <-r.releaseTurn:
							if r.protocolVersion >= 2 {
								emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
									"sessionId": sid,
									"update":    map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn", "background": []any{}},
								}})
							} else {
								emit(map[string]any{"jsonrpc": "2.0", "id": pid, "result": map[string]any{"stopReason": "end_turn"}})
							}
						case <-r.cancelCh:
							if r.protocolVersion >= 2 {
								emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
									"sessionId": sid,
									"update":    map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "cancelled", "background": []any{}},
								}})
							} else {
								emit(map[string]any{"jsonrpc": "2.0", "id": pid, "result": map[string]any{"stopReason": "cancelled"}})
							}
						}
					}(sessionID, promptID)
				} else {
					// Concurrent session/prompt steer (no _meta.steering).
					if r.onSteer != nil {
						r.onSteer(sessionID, promptText, plan)
					}
					if r.protocolVersion >= 2 {
						emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"messageId": "m2"}})
					} else {
						emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
					}
				}
			case "_session/steering":
				txt := ""
				if len(req.Params.Prompt) > 0 {
					txt = req.Params.Prompt[0].Text
				}
				outcome := "injected"
				if r.onSteer != nil {
					outcome = r.onSteer(sessionID, txt, plan)
				}
				emit(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"outcome": outcome}})
			case "session/cancel":
				select {
				case r.cancelCh <- struct{}{}:
				default:
				}
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func (r *duplexACPRunner) writeInitialStore(plan runtime.CommandPlan, sessionID, prompt string) {
	if r.agent == "claude" {
		path := sessions.ClaudeProjectFile(r.home, plan.Cwd, sessionID)
		_ = r.appendTurn(path, "claude", prompt, func(detail sessions.Session) []map[string]any {
			detail.ID = sessionID
			return sessions.SerializeClaude(detail)
		})
	} else if r.agent == "codex" {
		codexHome := plan.Env["CODEX_HOME"]
		if codexHome == "" {
			codexHome = os.Getenv("CODEX_HOME")
		}
		if path, err := sessions.FindCodexRollout(codexHome, sessionID); err == nil {
			_ = r.appendTurn(path, "codex", prompt, sessions.SerializeCodex)
		}
	}
}

func appendClaudeMessages(t *testing.T, home, cwd, sessionID string, msgs ...sessions.Message) {
	t.Helper()
	path := sessions.ClaudeProjectFile(home, cwd, sessionID)
	parsed, err := sessions.ParseNativeSessionFile("claude", path)
	if err != nil {
		t.Fatal(err)
	}
	parsed.ID = sessionID
	parsed.Messages = append(parsed.Messages, msgs...)
	records := sessions.SerializeClaude(parsed)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionContinueDuplexStartsOnLine1AndIgnoresStdinEOF(t *testing.T) {
	workspace := testWorkspace(t)
	inR, inW := io.Pipe()
	application, _, baseRunner := continueApp(t, "claude", "", "")
	application.Stdin = inR
	capture := &continueStreamCapture{}
	application.Stdout = capture
	baseRunner.reply = []sessions.Message{
		{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "done"}}},
	}
	runner := &duplexACPRunner{
		continueRunner:  baseRunner,
		protocolVersion: 1,
		steeringCap:     true,
		cancelCh:        make(chan struct{}, 1),
		promptStarted:   make(chan struct{}, 1),
		releaseTurn:     make(chan struct{}),
	}
	application.Runner = runner

	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Run(context.Background(), []string{"session", "continue", "claude", "--stream", "--duplex"})
	}()

	// Write line 1 WITHOUT closing stdin — turn must start immediately.
	if _, err := io.WriteString(inW, continueTranscript(t, workspace)+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.promptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("duplex turn did not start on line 1 without stdin EOF")
	}

	// Closing stdin (EOF) mid-turn must NOT abort or kill the running turn.
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	select {
	case <-runner.cancelCh:
		t.Fatal("stdin EOF unexpectedly cancelled the turn")
	default:
	}

	close(runner.releaseTurn)
	if err := <-errCh; err != nil {
		t.Fatalf("duplex run failed after stdin EOF: %v", err)
	}
	msgs, _, final := decodeDuplexEvents(t, capture.snapshot())
	if final == nil || len(msgs) != 1 || sessions.MessageText(msgs[0]) != "done" {
		t.Fatalf("unexpected output: %s", capture.snapshot())
	}
}

func TestSessionContinueDuplexACPv1SteerIdleAndChronologicalDelta(t *testing.T) {
	workspace := testWorkspace(t)
	inR, inW := io.Pipe()
	defer inW.Close()
	application, _, baseRunner := continueApp(t, "claude", "", "")
	application.Stdin = inR
	capture := &continueStreamCapture{}
	application.Stdout = capture
	baseRunner.reply = []sessions.Message{
		{ID: "call", Role: "assistant", Content: []sessions.ContentBlock{{Type: "tool_use", ID: "t1", Name: "read", Input: map[string]any{"path": "a"}}}},
		{ID: "tool", ParentID: "call", Role: "tool", ToolUseID: "t1", ToolName: "read", Result: []any{map[string]any{"type": "text", "text": "ok"}}},
	}

	runner := &duplexACPRunner{
		continueRunner:  baseRunner,
		protocolVersion: 1,
		steeringCap:     true,
		cancelCh:        make(chan struct{}, 1),
		promptStarted:   make(chan struct{}, 1),
		releaseTurn:     make(chan struct{}),
		onSteer: func(sessionID, text string, plan runtime.CommandPlan) string {
			if text == "late-msg" {
				return "promptRequired"
			}
			appendClaudeMessages(t, baseRunner.home, plan.Cwd, sessionID,
				sessions.Message{ID: "steer-u", ParentID: "tool", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: text}}},
				sessions.Message{ID: "steer-a", ParentID: "steer-u", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "acknowledged: " + text}}},
			)
			return "injected"
		},
	}
	application.Runner = runner

	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Run(context.Background(), []string{"session", "continue", "claude", "--stream", "--duplex"})
	}()

	_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
	<-runner.promptStarted

	// Send mid-turn steer + idle steer.
	_, _ = io.WriteString(inW, `{"type":"steer","id":"req-1","message":"focus on tests"}`+"\n")
	_, _ = io.WriteString(inW, `{"type":"steer","id":"req-2","message":"late-msg"}`+"\n")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, acks, _ := decodeDuplexEvents(t, capture.snapshot())
		if len(acks) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(runner.releaseTurn)
	if err := <-errCh; err != nil {
		t.Fatalf("duplex run failed: %v", err)
	}

	msgs, acks, final := decodeDuplexEvents(t, capture.snapshot())
	if len(acks) != 2 {
		t.Fatalf("expected 2 steer_acks, got %+v\nraw:\n%s", acks, capture.snapshot())
	}
	if acks[0].ID != "req-1" || acks[0].Disposition != "steered" || acks[0].Error != "" {
		t.Fatalf("unexpected ack[0]: %+v", acks[0])
	}
	if acks[1].ID != "req-2" || acks[1].Disposition != "idle" || acks[1].Error != "" {
		t.Fatalf("unexpected ack[1]: %+v", acks[1])
	}
	// Verify error field is omitted on wire when empty.
	if strings.Contains(capture.snapshot(), `"error":""`) {
		t.Fatalf("empty error field must be omitted on wire: %s", capture.snapshot())
	}
	if final == nil || !reflect.DeepEqual(msgs, final.Messages) || len(msgs) != 4 {
		t.Fatalf("expected 4 chronological canonical messages (assistant, tool, user-steer, assistant-final), got %d: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != "user" || sessions.MessageText(msgs[2]) != "focus on tests" {
		t.Fatalf("expected steered user message at index 2, got %+v", msgs[2])
	}
	if msgs[3].Role != "assistant" || sessions.MessageText(msgs[3]) != "acknowledged: focus on tests" {
		t.Fatalf("expected final assistant message at index 3, got %+v", msgs[3])
	}
}

func TestSessionContinueDuplexACPv2SteerAndCancel(t *testing.T) {
	workspace := testWorkspace(t)
	inR, inW := io.Pipe()
	defer inW.Close()
	application, _, baseRunner := continueApp(t, "codex", "", "")
	application.Stdin = inR
	capture := &continueStreamCapture{}
	application.Stdout = capture
	baseRunner.reply = []sessions.Message{
		{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "working"}}},
	}

	steered := make(chan string, 1)
	runner := &duplexACPRunner{
		continueRunner:  baseRunner,
		protocolVersion: 2,
		steeringCap:     true,
		cancelCh:        make(chan struct{}, 1),
		promptStarted:   make(chan struct{}, 1),
		releaseTurn:     make(chan struct{}),
		onSteer: func(_ string, text string, _ runtime.CommandPlan) string {
			steered <- text
			return "injected"
		},
	}
	application.Runner = runner

	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Run(context.Background(), []string{"session", "continue", "codex", "--stream", "--duplex"})
	}()

	_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
	<-runner.promptStarted

	_, _ = io.WriteString(inW, `{"type":"steer","id":"v2-1","message":"pivot"}`+"\n")
	if got := <-steered; got != "pivot" {
		t.Fatalf("expected pivot, got %q", got)
	}
	// Wait for steer_ack before sending cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, acks, _ := decodeDuplexEvents(t, capture.snapshot())
		if len(acks) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _ = io.WriteString(inW, `{"type":"cancel"}`+"\n")

	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected cancelled error, got %v", err)
	}
	_, acks, final := decodeDuplexEvents(t, capture.snapshot())
	if final != nil {
		t.Fatalf("cancelled turn must not emit result: %s", capture.snapshot())
	}
	if len(acks) != 1 || acks[0].Disposition != "steered" {
		t.Fatalf("unexpected acks: %+v", acks)
	}
}

type duplexPiRunner struct {
	*continueRunner
	promptStarted chan struct{}
	releaseTurn   chan struct{}
	abortMode     bool
}

func (r *duplexPiRunner) StartStream(plan runtime.CommandPlan) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var writeMu sync.Mutex
	emit := func(line string) {
		writeMu.Lock()
		_, _ = io.WriteString(outW, line+"\n")
		writeMu.Unlock()
	}
	provider, model, _ := strings.Cut(r.model, "/")
	sessionPath := flagValue(plan.Args, "--session")

	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var cmd struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				continue
			}
			switch cmd.Type {
			case "prompt":
				_ = r.appendTurn(sessionPath, "pi", cmd.Message, sessions.SerializePi)
				emit(`{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`)
				emit(`{"type":"session","id":"pi-duplex"}`)
				select {
				case r.promptStarted <- struct{}{}:
				default:
				}
				if !r.abortMode {
					go func() {
						<-r.releaseTurn
						emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","provider":%q,"model":%q,"stopReason":"stop","content":[{"type":"text","text":"done"}]}}`, provider, model))
						emit(`{"type":"agent_end"}`)
						emit(`{"type":"agent_settled","aborted":false}`)
					}()
				}
			case "steer":
				// Append steered user + assistant messages to Pi session file.
				if parsed, err := sessions.ParseNativeSessionFile("pi", sessionPath); err == nil {
					parsed.Messages = append(parsed.Messages,
						sessions.Message{ID: "pi-u", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: cmd.Message}}},
						sessions.Message{ID: "pi-a", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "pi saw: " + cmd.Message}}},
					)
					records := sessions.SerializePi(parsed)
					var buf bytes.Buffer
					enc := json.NewEncoder(&buf)
					for _, rec := range records {
						_ = enc.Encode(rec)
					}
					_ = os.WriteFile(sessionPath, buf.Bytes(), 0o600)
				}
				emit(`{"type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}`)
			case "abort":
				emit(`{"type":"response","command":"abort","success":true}`)
				emit(`{"type":"agent_settled","aborted":true}`)
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func TestSessionContinueDuplexPiRPCSteerAndCancel(t *testing.T) {
	t.Run("steer", func(t *testing.T) {
		workspace := testWorkspace(t)
		inR, inW := io.Pipe()
		defer inW.Close()
		application, _, baseRunner := continueApp(t, "pi", "anthropic/claude-x", "")
		application.Stdin = inR
		capture := &continueStreamCapture{}
		application.Stdout = capture
		baseRunner.reply = []sessions.Message{
			{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "step1"}}},
		}
		runner := &duplexPiRunner{
			continueRunner: baseRunner,
			promptStarted:  make(chan struct{}, 1),
			releaseTurn:    make(chan struct{}),
		}
		application.Runner = runner

		errCh := make(chan error, 1)
		go func() {
			errCh <- application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x", "--stream", "--duplex"})
		}()

		_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
		<-runner.promptStarted
		_, _ = io.WriteString(inW, `{"type":"steer","id":"p1","message":"check pi"}`+"\n")

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, acks, _ := decodeDuplexEvents(t, capture.snapshot())
			if len(acks) == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(runner.releaseTurn)
		if err := <-errCh; err != nil {
			t.Fatalf("pi duplex steer failed: %v", err)
		}
		msgs, acks, final := decodeDuplexEvents(t, capture.snapshot())
		if len(acks) != 1 || acks[0].ID != "p1" || acks[0].Disposition != "steered" {
			t.Fatalf("unexpected pi acks: %+v", acks)
		}
		if final == nil || len(msgs) != 3 || !reflect.DeepEqual(msgs, final.Messages) {
			t.Fatalf("expected 3 chronological pi messages, got %d: %+v", len(msgs), msgs)
		}
		if msgs[1].Role != "user" || sessions.MessageText(msgs[1]) != "check pi" {
			t.Fatalf("missing steered user message in pi delta: %+v", msgs)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		workspace := testWorkspace(t)
		inR, inW := io.Pipe()
		defer inW.Close()
		application, _, baseRunner := continueApp(t, "pi", "anthropic/claude-x", "")
		application.Stdin = inR
		capture := &continueStreamCapture{}
		application.Stdout = capture
		baseRunner.reply = []sessions.Message{
			{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "step1"}}},
		}
		runner := &duplexPiRunner{
			continueRunner: baseRunner,
			promptStarted:  make(chan struct{}, 1),
			releaseTurn:    make(chan struct{}),
			abortMode:      true,
		}
		application.Runner = runner

		errCh := make(chan error, 1)
		go func() {
			errCh <- application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x", "--stream", "--duplex"})
		}()

		_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
		<-runner.promptStarted
		// Wait until the turn is active and streaming before sending native cancel.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			msgs, _, _ := decodeDuplexEvents(t, capture.snapshot())
			if len(msgs) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = io.WriteString(inW, `{"type":"cancel"}`+"\n")

		err := <-errCh
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("expected pi abort error, got %v", err)
		}
		_, _, final := decodeDuplexEvents(t, capture.snapshot())
		if final != nil {
			t.Fatalf("aborted pi turn must not emit result")
		}
	})
}

func TestContinueStreamWriterConcurrentLockingAndTerminalBarrier(t *testing.T) {
	var buf bytes.Buffer
	w := newContinueStreamWriter(&buf)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = w.WriteEvent(duplexSteerAck{
					Type:        "steer_ack",
					ID:          fmt.Sprintf("g%d-%d", id, i),
					Disposition: "steered",
				})
			}
		}(g)
	}
	wg.Wait()

	if err := w.WriteTerminal(struct {
		Type    string           `json:"type"`
		Session sessions.Session `json:"session"`
	}{"result", sessions.Session{ID: "done"}}); err != nil {
		t.Fatal(err)
	}
	// Post-terminal writes must be silently dropped so nothing follows result.
	_ = w.WriteEvent(duplexSteerAck{Type: "steer_ack", ID: "late", Disposition: "idle"})

	_, acks, final := decodeDuplexEvents(t, buf.String())
	if len(acks) != 200 || final == nil || final.ID != "done" {
		t.Fatalf("expected 200 acks + terminal result, got acks=%d final=%+v", len(acks), final)
	}
}

func TestSessionContinueNonDuplexMultilineStdinBackwardsCompat(t *testing.T) {
	workspace := testWorkspace(t)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(continueTranscript(t, workspace)), "", "  "); err != nil {
		t.Fatal(err)
	}
	application, _, runner := continueApp(t, "claude", "", pretty.String())
	capture := &continueStreamCapture{}
	application.Stdout = capture
	runner.reply = []sessions.Message{{ID: "r1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "compat ok"}}}}

	if err := application.Run(context.Background(), []string{"session", "continue", "claude", "--stream"}); err != nil {
		t.Fatalf("non-duplex multiline stdin failed: %v", err)
	}
	msgs, _, final := decodeDuplexEvents(t, capture.snapshot())
	if final == nil || len(msgs) != 1 || sessions.MessageText(msgs[0]) != "compat ok" {
		t.Fatalf("unexpected non-duplex output: %s", capture.snapshot())
	}
}
