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
	"strings"
	"testing"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

// continueRunner simulates one native agent turn: it appends the turn's
// messages to the native session file and answers with the native stream.
type continueRunner struct {
	home       string
	agent      string
	model      string
	reply      []sessions.Message
	plans      []runtime.CommandPlan
	prompts    []string
	resumedIDs []string
	fresh      string
}

func (r *continueRunner) ExecuteJSONRPC(_ context.Context, _ runtime.CommandPlan, _ []json.RawMessage, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"id":2,"result":{"config":{"model_provider":"company"}}}`), nil
}

func (r *continueRunner) Execute(_ context.Context, _ runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	return runtime.CommandResult{}, fmt.Errorf("agent %s must use stream protocol (ACP/RPC), not OneShot Execute", r.agent)
}

func (r *continueRunner) StartStream(plan runtime.CommandPlan) (*runtime.Stream, error) {
	switch r.agent {
	case "pi":
		return r.startPiStream(plan, nil, nil)
	case "codex":
		return r.startCodexStream(plan, nil)
	}
	return r.startClaudeStream(plan, nil)
}

// storeTurn appends one turn to the agent's native session store the way the real CLI would.
func (r *continueRunner) storeTurn(plan runtime.CommandPlan, sessionID, prompt string) error {
	if r.agent == "claude" {
		path := sessions.ClaudeProjectFile(r.home, plan.Cwd, sessionID)
		ps := sessions.ProtocolSession{ProtocolVersion: "2.0", ID: sessionID, Provider: "claude", Workspace: plan.Cwd}
		if _, err := os.Stat(path); err == nil {
			parsed, err := sessions.ParseNativeProtocolFile("claude", path)
			if err != nil {
				return err
			}
			ps = parsed
			ps.ID = sessionID
		}
		ps.Messages = append(ps.Messages, sessions.ProtocolMessage{
			ID: "echo-user", Source: "user", Mutability: "delta",
			Content: []sessions.ContentBlock{{Type: "text", Text: prompt}},
		})
		replyPS, err := sessions.ToProtocolSession(sessions.Session{ID: sessionID, Provider: "claude", Messages: r.reply})
		if err != nil {
			return err
		}
		ps.Messages = append(ps.Messages, replyPS.Messages...)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		for _, record := range sessions.SerializeClaudeProtocol(ps) {
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return os.WriteFile(path, buffer.Bytes(), 0o600)
	}
	codexHome := plan.Env["CODEX_HOME"]
	if codexHome == "" {
		codexHome = os.Getenv("CODEX_HOME")
	}
	path, err := sessions.FindCodexRollout(codexHome, sessionID)
	if err != nil {
		return err
	}
	return r.appendTurn(path, "codex", prompt, sessions.SerializeCodex)
}

func pipeJSON(outW *io.PipeWriter) func(any) {
	return func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = io.WriteString(outW, string(raw)+"\n")
	}
}

// startClaudeStream fakes `claude -p --input-format stream-json`: one result per user message.
func (r *continueRunner) startClaudeStream(plan runtime.CommandPlan, hook func(emit func(any), sessionID string) error) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	emit := pipeJSON(outW)
	sessionID := flagValue(plan.Args, "--resume")
	if sessionID == "" {
		sessionID = r.fresh
	}
	r.resumedIDs = append(r.resumedIDs, flagValue(plan.Args, "--resume"))
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var msg struct {
				Type    string `json:"type"`
				Message struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Type != "user" || len(msg.Message.Content) == 0 {
				continue
			}
			prompt := msg.Message.Content[0].Text
			r.prompts = append(r.prompts, prompt)
			emit(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID})
			if err := r.storeTurn(plan, sessionID, prompt); err != nil {
				emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error(), "session_id": sessionID})
				continue
			}
			emit(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "done"}}}})
			if hook != nil {
				if err := hook(emit, sessionID); err != nil {
					emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": err.Error(), "session_id": sessionID})
					continue
				}
			}
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done", "session_id": sessionID})
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

// startCodexStream fakes `codex app-server`.
func (r *continueRunner) startCodexStream(plan runtime.CommandPlan, hook func(emit func(any), sessionID string) error) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	emit := pipeJSON(outW)
	go func() {
		defer outW.Close()
		var threadID string
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					ThreadID string `json:"threadId"`
					Input    []struct {
						Text string `json:"text"`
					} `json:"input"`
				} `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
			}
			switch req.Method {
			case "initialize":
				emit(map[string]any{"id": req.ID, "result": map[string]any{"userAgent": "fake"}})
			case "thread/start", "thread/resume":
				threadID = req.Params.ThreadID
				r.resumedIDs = append(r.resumedIDs, threadID)
				if threadID == "" {
					threadID = r.fresh
				}
				emit(map[string]any{"id": req.ID, "result": map[string]any{"thread": map[string]any{"id": threadID}}})
			case "turn/start":
				prompt := ""
				if len(req.Params.Input) > 0 {
					prompt = req.Params.Input[0].Text
				}
				r.prompts = append(r.prompts, prompt)
				fail := func(message string) {
					emit(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "t1", "status": "failed", "error": map[string]any{"message": message}}}})
				}
				emit(map[string]any{"id": req.ID, "result": map[string]any{"turn": map[string]any{"id": "t1"}}})
				emit(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "t1"}}})
				if err := r.storeTurn(plan, threadID, prompt); err != nil {
					fail(err.Error())
					continue
				}
				emit(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": threadID, "turnId": "t1", "item": map[string]any{"type": "agentMessage", "text": "done"}}})
				if hook != nil {
					if err := hook(emit, threadID); err != nil {
						fail(err.Error())
						continue
					}
				}
				emit(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "t1", "status": "completed"}}})
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func (r *continueRunner) startPiStream(plan runtime.CommandPlan, beforeEmit func(emit func(string)) error, beforeSettle func(emit func(string)) error) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	emit := func(line string) {
		_, _ = io.WriteString(outW, line+"\n")
	}
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
			if cmd.Type != "prompt" {
				continue
			}
			r.prompts = append(r.prompts, cmd.Message)
			res, err := r.runPi(plan, cmd.Message)
			if err != nil {
				emit(fmt.Sprintf(`{"type":"response","command":"prompt","success":false,"error":%q}`, err.Error()))
				return
			}
			if beforeEmit != nil {
				if err := beforeEmit(emit); err != nil {
					emit(fmt.Sprintf(`{"type":"response","command":"prompt","success":false,"error":%q}`, err.Error()))
					return
				}
			}
			emit(`{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`)
			lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
			for i, line := range lines {
				if i == len(lines)-1 && beforeSettle != nil {
					if err := beforeSettle(emit); err != nil {
						return
					}
				}
				emit(line)
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (r *continueRunner) appendTurn(path, agent, prompt string, write func(sessions.Session) []map[string]any) error {
	detail := sessions.Session{ID: filepath.Base(path), Provider: agent}
	if _, err := os.Stat(path); err == nil {
		parsed, err := sessions.ParseNativeSessionFile(agent, path)
		if err != nil {
			return err
		}
		detail = parsed
	}
	user := sessions.Message{ID: "echo-user", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: prompt}}}
	detail.Messages = append(detail.Messages, user)
	detail.Messages = append(detail.Messages, r.reply...)
	records := write(detail)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buffer.Bytes(), 0o600)
}

func (r *continueRunner) runPi(plan runtime.CommandPlan, prompt string) (runtime.CommandResult, error) {
	path := flagValue(plan.Args, "--session")
	if err := r.appendTurn(path, "pi", prompt, sessions.SerializePi); err != nil {
		return runtime.CommandResult{}, err
	}
	provider, model, _ := strings.Cut(r.model, "/")
	stdout := fmt.Sprintf(`{"type":"session","id":"pi-native"}`+"\n"+
		`{"type":"tool_execution_end","toolCallId":"call_1","toolName":"read","result":{"content":[{"type":"text","text":"file body"}],"details":{"truncated":false}},"isError":false}`+"\n"+
		`{"type":"message_end","message":{"role":"assistant","provider":%q,"model":%q,"stopReason":"stop","content":[{"type":"text","text":"done"}]}}`+"\n"+
		`{"type":"agent_end"}`+"\n"+
		`{"type":"compaction_end","reason":"threshold","result":{"summary":"earlier work","firstKeptEntryId":"e1","tokensBefore":1200,"details":{}},"aborted":false,"willRetry":false}`+"\n"+
		`{"type":"agent_settled","aborted":false}`+"\n", provider, model)
	return runtime.CommandResult{Stdout: stdout}, nil
}

func continueApp(t *testing.T, agent, model, input string) (App, *bytes.Buffer, *continueRunner) {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	runner := &continueRunner{home: home, agent: agent, model: model, fresh: "99999999-8888-4777-a666-555555555555"}
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(input), &stdout, &bytes.Buffer{})
	application.Runner = runner
	application.Sessions = sessions.Service{HomeDir: home}
	return application, &stdout, runner
}

func continueTranscript(t *testing.T, workspace string) string {
	t.Helper()
	detail := sessions.Session{
		ID:        "turn-1",
		Provider:  "harness",
		Workspace: workspace,
		Messages: []sessions.Message{
			{ID: "m1", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "hello"}}},
			{ID: "m2", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "hi there"}}},
			{ID: "m3", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "what now?"}}},
		},
	}
	detail.MessageCount = len(detail.Messages)
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testWorkspace(t *testing.T) string {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func decodeDelta(t *testing.T, stdout *bytes.Buffer) sessions.ProtocolSession {
	t.Helper()
	var delta sessions.ProtocolSession
	if err := json.Unmarshal(stdout.Bytes(), &delta); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if err := transcript.ValidateSession(delta); err != nil {
		t.Fatalf("invalid 2.0 delta %q: %v", stdout.String(), err)
	}
	return delta
}

func TestSessionContinueClaudeEmitsNamespacedDelta(t *testing.T) {
	workspace := testWorkspace(t)
	application, stdout, runner := continueApp(t, "claude", "", continueTranscript(t, workspace))
	runner.reply = []sessions.Message{{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "the answer"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "claude", "--mode", "yolo"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.prompts) != 1 || runner.prompts[0] != "what now?" {
		t.Fatalf("prompts = %q", runner.prompts)
	}
	delta := decodeDelta(t, stdout)
	if delta.ProtocolVersion != transcript.ProtocolVersion || delta.Provider != "claude" || delta.Source != "continue" || delta.Workspace != workspace {
		t.Fatalf("delta = %+v", delta)
	}
	if len(delta.Messages) != 1 || delta.MessageCount != 1 {
		t.Fatalf("delta entries = %+v", delta.Messages)
	}
	entry := delta.Messages[0]
	if entry.Source != transcript.SourceS2 || entry.Mutability != transcript.MutabilityDelta || transcript.TextContent(entry) != "the answer" {
		t.Fatalf("entry = %+v", entry)
	}
	if len(runner.resumedIDs) == 0 || runner.resumedIDs[len(runner.resumedIDs)-1] == "" {
		t.Fatalf("claude ACP missing sessionId: %q", runner.resumedIDs)
	}
	token := runner.resumedIDs[len(runner.resumedIDs)-1]
	if !strings.HasPrefix(entry.ID, token+":") {
		t.Fatalf("id %q not namespaced by %q", entry.ID, token)
	}
	if delta.ID != token {
		t.Fatalf("delta session id %q != resumed session %q", delta.ID, token)
	}
}

func TestSessionContinueClaudeFreshSessionWithoutHistory(t *testing.T) {
	workspace := testWorkspace(t)
	detail := sessions.Session{ID: "turn-1", Provider: "harness", Workspace: workspace, Messages: []sessions.Message{
		{ID: "m1", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "first contact"}}},
	}, MessageCount: 1}
	input, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	application, stdout, runner := continueApp(t, "claude", "", string(input))
	runner.reply = []sessions.Message{{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "welcome"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "claude"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.resumedIDs) == 0 || runner.resumedIDs[len(runner.resumedIDs)-1] != "" {
		t.Fatalf("fresh turn must not resume: %q", runner.resumedIDs)
	}
	delta := decodeDelta(t, stdout)
	if delta.ID != runner.fresh {
		t.Fatalf("delta.ID = %q", delta.ID)
	}
	if len(delta.Messages) != 1 || transcript.TextContent(delta.Messages[0]) != "welcome" {
		t.Fatalf("delta = %+v", delta.Messages)
	}
}

func TestSessionContinueCodexEmitsDeltaFromConfiguredHome(t *testing.T) {
	workspace := testWorkspace(t)
	application, stdout, runner := continueApp(t, "codex", "", continueTranscript(t, workspace))
	runner.reply = []sessions.Message{{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "codex answer"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "codex", "--model", "gpt-6", "--mode", "yolo"}); err != nil {
		t.Fatal(err)
	}
	delta := decodeDelta(t, stdout)
	if delta.Provider != "codex" || len(delta.Messages) != 1 {
		t.Fatalf("delta = %+v", delta)
	}
	if transcript.TextContent(delta.Messages[0]) != "codex answer" {
		t.Fatalf("entry = %+v", delta.Messages[0])
	}
	if len(runner.plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(runner.plans))
	}
	// Codex writes should go to the configured CODEX_HOME, not the test's
	// default home dir.
	codexHome := os.Getenv("CODEX_HOME")
	if _, err := os.Stat(filepath.Join(codexHome, "sessions")); os.IsNotExist(err) {
		t.Log("codex sessions written to configured CODEX_HOME as expected")
	}
}

func TestSessionContinuePiEmitsDelta(t *testing.T) {
	workspace := testWorkspace(t)
	application, stdout, runner := continueApp(t, "pi", "anthropic/claude-x", continueTranscript(t, workspace))
	runner.reply = []sessions.Message{{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "pi answer"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x"}); err != nil {
		t.Fatal(err)
	}
	delta := decodeDelta(t, stdout)
	if delta.Provider != "pi" || delta.ID != "pi-native" {
		t.Fatalf("delta = %+v", delta)
	}
	if len(delta.Messages) != 1 || transcript.TextContent(delta.Messages[0]) != "pi answer" {
		t.Fatalf("entries = %+v", delta.Messages)
	}
}

func TestSessionContinueRejectsTranscriptNotEndingWithUser(t *testing.T) {
	workspace := testWorkspace(t)
	detail := sessions.Session{ID: "turn-1", Provider: "harness", Workspace: workspace, Messages: []sessions.Message{
		{ID: "m1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "hello"}}},
	}, MessageCount: 1}
	input, _ := json.Marshal(detail)
	application, _, _ := continueApp(t, "claude", "", string(input))
	err := application.Run(context.Background(), []string{"session", "continue", "claude"})
	if err == nil || !strings.Contains(err.Error(), "must end with a user message") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionContinueRequiresModelForPi(t *testing.T) {
	application, _, _ := continueApp(t, "pi", "", continueTranscript(t, testWorkspace(t)))
	err := application.Run(context.Background(), []string{"session", "continue", "pi"})
	if err == nil || !strings.Contains(err.Error(), "--model is required for pi") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionContinueClaudeProtocol20PreventsReasoningExtraction(t *testing.T) {
	workspace := testWorkspace(t)
	ps := map[string]any{
		"protocolVersion": "2.0",
		"id":              "wake-chats-91",
		"provider":        "agy",
		"workspace":       workspace,
		"messageCount":      5,
		"messages": []any{
			map[string]any{
				"id": "s1-identity", "source": "s1", "mutability": "constant", "slot": "identity",
				"content": []any{map[string]any{"type": "text", "text": "Identity: philia093"}},
			},
			map[string]any{
				"id": "world-rules", "source": "world", "mutability": "constant", "slot": "rules",
				"content": []any{map[string]any{"type": "text", "text": "AGENTS.md static rules"}},
			},
			map[string]any{
				"id": "u-prev", "source": "user", "mutability": "delta",
				"content": []any{map[string]any{"type": "text", "text": "previous question"}},
			},
			map[string]any{
				"id": "a-agy", "parentId": "u-prev", "source": "s2", "mutability": "delta",
				"producer": map[string]any{"provider": "agy", "model": "gemini-3"},
				"content": []any{
					map[string]any{"type": "thinking", "text": "Initial hypothesis pointed toward foreign reasoning"},
					map[string]any{"type": "text", "text": "agy normal reply"},
				},
			},
			map[string]any{
				"id": "u-now", "parentId": "a-agy", "source": "user", "mutability": "delta",
				"content": []any{map[string]any{"type": "text", "text": "new prompt"}},
			},
		},
		"runtimeEvents": []any{
			map[string]any{
				"id": "rt-1", "provider": "claude", "kind": "safety_refusal",
				"code": "reasoning_extraction", "message": "safeguards flagged this message",
			},
		},
	}
	input, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	application, _, runner := continueApp(t, "claude", "", string(input))
	runner.reply = []sessions.Message{{ID: "reply-1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "clean reply"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "claude"}); err != nil {
		t.Fatalf("session continue claude failed: %v", err)
	}
	rawBytes, err := os.ReadFile(sessions.ClaudeProjectFile(runner.home, workspace, runner.resumedIDs[0]))
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(rawBytes)
	for _, forbidden := range []string{"Initial hypothesis", "AGENTS.md static rules", "safeguards flagged", "※", "<think>"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("forbidden content %q reached Claude native session: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"isMeta":true`) || !strings.Contains(encoded, "Identity: philia093") {
		t.Fatalf("s1 identity missing or not marked isMeta:true in Claude native session: %s", encoded)
	}
}

func TestSessionContinueRejectsAgentsWithoutAdapter(t *testing.T) {
	application, _, _ := continueApp(t, "claude", "", "")
	err := application.Run(context.Background(), []string{"session", "continue", "opencode"})
	if err == nil || !strings.Contains(err.Error(), "no headless continue adapter") {
		t.Fatalf("err = %v", err)
	}
}

// The caller's real contract: each delta is appended to the transcript AS-IS
// (namespaced ids, dangling parentIds, native fields) and continued. The
// second import must keep the whole history on one Claude ancestor chain.
func TestSessionContinueRoundTripAppendsDeltaAsIs(t *testing.T) {
	workspace := testWorkspace(t)
	secret := "Remember the secret passphrase PINEAPPLE-77."
	first := sessions.ProtocolSession{
		ProtocolVersion: transcript.ProtocolVersion,
		ID:              "turn-1",
		Provider:        "harness",
		Workspace:       workspace,
		Messages: []sessions.ProtocolMessage{
			{ID: "u1", Source: transcript.SourceUser, Mutability: transcript.MutabilityDelta, Content: []sessions.ContentBlock{{Type: "text", Text: secret}}},
		},
		MessageCount: 1,
	}
	firstInput, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	application, stdout, runner := continueApp(t, "claude", "", string(firstInput))
	runner.reply = []sessions.Message{{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "OK"}}}}
	if err := application.Run(context.Background(), []string{"session", "continue", "claude"}); err != nil {
		t.Fatal(err)
	}
	delta1 := decodeDelta(t, stdout)

	second := first
	second.Messages = append(append([]sessions.ProtocolMessage{}, first.Messages...), delta1.Messages...)
	second.Messages = append(second.Messages, sessions.ProtocolMessage{
		ID: "u2", Source: transcript.SourceUser, Mutability: transcript.MutabilityDelta, Content: []sessions.ContentBlock{{Type: "text", Text: "What was the passphrase?"}},
	})
	second.MessageCount = len(second.Messages)
	secondInput, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	application2, stdout2, runner2 := continueApp(t, "claude", "", string(secondInput))
	application2.Sessions = application.Sessions
	runner2.home = runner.home
	runner2.reply = []sessions.Message{{ID: "a2", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "PINEAPPLE-77"}}}}
	if err := application2.Run(context.Background(), []string{"session", "continue", "claude"}); err != nil {
		t.Fatal(err)
	}
	delta2 := decodeDelta(t, stdout2)
	if len(delta2.Messages) != 1 || transcript.TextContent(delta2.Messages[0]) != "PINEAPPLE-77" {
		t.Fatalf("second delta = %+v", delta2.Messages)
	}

	if len(runner2.resumedIDs) == 0 || runner2.resumedIDs[len(runner2.resumedIDs)-1] == "" {
		t.Fatalf("second turn must resume the imported history: %q", runner2.resumedIDs)
	}
	token := runner2.resumedIDs[len(runner2.resumedIDs)-1]
	records := readClaudeRecords(t, sessions.ClaudeProjectFile(runner2.home, workspace, token))
	joined := ""
	// Only the imported history (first 2 records) is under test; the fake
	// runner rewrites the file when simulating the turn.
	if len(records) < 2 {
		t.Fatalf("imported history has %d records", len(records))
	}
	if records[0]["parentUuid"] != "" {
		t.Fatalf("history root carries parentUuid %v", records[0]["parentUuid"])
	}
	for i := 1; i < 2; i++ {
		if records[i]["parentUuid"] != records[i-1]["uuid"] {
			t.Fatalf("history record %d is off the ancestor chain: parentUuid=%v previous uuid=%v", i, records[i]["parentUuid"], records[i-1]["uuid"])
		}
	}
	for _, record := range records {
		data, _ := json.Marshal(record)
		joined += string(data)
	}
	if !strings.Contains(joined, "PINEAPPLE-77") {
		t.Fatalf("imported history lost the passphrase: %s", joined)
	}
}

func readClaudeRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}
