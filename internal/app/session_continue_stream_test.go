package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type continueStreamCapture struct {
	mu sync.Mutex
	bytes.Buffer
	message chan struct{}
}

func (c *continueStreamCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.Buffer.Write(p)
	if bytes.Contains(p, []byte(`"type":"message"`)) && c.message != nil {
		select {
		case c.message <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (c *continueStreamCapture) snapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Buffer.String()
}

type continueStreamTestRunner struct {
	*continueRunner
	during func(runtime.ExecuteOptions, *runtime.CommandResult) error
}

func (r continueStreamTestRunner) Execute(ctx context.Context, plan runtime.CommandPlan, opts runtime.ExecuteOptions) (runtime.CommandResult, error) {
	result, err := r.continueRunner.Execute(ctx, plan, opts)
	if err == nil {
		err = r.during(opts, &result)
	}
	return result, err
}

func decodeContinueEvents(t *testing.T, output string) ([]sessions.Message, *sessions.Session) {
	t.Helper()
	var messages []sessions.Message
	var result *sessions.Session
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event struct {
			Type    string           `json:"type"`
			Message sessions.Message `json:"message"`
			Session sessions.Session `json:"session"`
		}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if result != nil {
			t.Fatal("event after terminal result")
		}
		switch event.Type {
		case "message":
			messages = append(messages, event.Message)
		case "result":
			result = &event.Session
		default:
			t.Fatalf("unknown event %q", event.Type)
		}
	}
	return messages, result
}

func TestContinueStreamMessagesBeforeRunnerFinishes(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			workspace := testWorkspace(t)
			application, _, runner := continueApp(t, agent, "provider/model", continueTranscript(t, workspace))
			capture := &continueStreamCapture{}
			application.Stdout = capture
			runner.reply = []sessions.Message{
				{ID: "call", Role: "assistant", Timestamp: "2026-11-01T01:02:03Z", RecordedAt: "2026-11-01T01:02:04Z", Content: []sessions.ContentBlock{{Type: "text", Text: ""}, {Type: "tool_use", ID: "tool-id", Name: "read", Input: map[string]any{"path": "file"}}}},
				{ID: "tool", ParentID: "call", Role: "tool", ToolUseID: "tool-id", ToolName: "read", Result: []any{map[string]any{"type": "text", "text": "body"}}, Timestamp: "2026-11-01T01:02:05Z"},
				{ID: "answer", ParentID: "tool", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "done"}}},
			}
			application.Runner = continueStreamTestRunner{runner, func(opts runtime.ExecuteOptions, result *runtime.CommandResult) error {
				if opts.Stdout == nil {
					t.Fatal("missing live output writer")
				}
				// Exercise arbitrary stdout chunk boundaries, including a final line
				// without a newline. The runner has not returned at this point.
				output := strings.TrimSuffix(result.Stdout, "\n")
				for i := range len(output) {
					if _, err := opts.Stdout.Write([]byte(output[i : i+1])); err != nil {
						return err
					}
				}
				messages, final := decodeContinueEvents(t, capture.snapshot())
				if len(messages) < 2 || final != nil {
					t.Fatalf("no realtime messages before runner exit: %s", capture.snapshot())
				}
				if agent == "codex" && len(messages) != 2 {
					t.Fatal("mutable Codex tail was emitted")
				}
				return nil
			}}
			if err := application.Run(context.Background(), []string{"session", "continue", agent, "--model", "provider/model", "--stream"}); err != nil {
				t.Fatal(err)
			}
			messages, final := decodeContinueEvents(t, capture.snapshot())
			if final == nil || len(messages) != 3 || !reflect.DeepEqual(messages, final.Messages) {
				t.Fatalf("stream/final mismatch: %s", capture.snapshot())
			}
			if final.MessageCount != 3 {
				t.Fatal(final.MessageCount)
			}
			if messages[0].ID == "call" || !strings.Contains(messages[0].ID, ":") {
				t.Fatal("missing namespace")
			}
			if messages[1].ParentID != messages[0].ID || messages[1].ToolUseID != messages[0].Content[1].ID {
				t.Fatal("parent/tool identity mismatch")
			}
			if len(messages[0].Content) != 2 || messages[0].Content[0].Type != "text" || messages[0].Content[0].Text != "" {
				t.Fatal("lost empty text")
			}
			var wire map[string]any
			firstLine, _, _ := strings.Cut(capture.snapshot(), "\n")
			if err := json.Unmarshal([]byte(firstLine), &wire); err != nil {
				t.Fatal(err)
			}
			block := wire["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
			if value, present := block["text"]; !present || value != "" {
				t.Fatal("empty text omitted on wire")
			}
		})
	}
}

func TestContinueStreamFreshClaudeSkipsPrompt(t *testing.T) {
	workspace := testWorkspace(t)
	input, _ := json.Marshal(sessions.Session{Workspace: workspace, Messages: []sessions.Message{{ID: "prompt", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "fresh prompt"}}}}})
	application, _, runner := continueApp(t, "claude", "", string(input))
	capture := &continueStreamCapture{}
	application.Stdout = capture
	runner.reply = []sessions.Message{{ID: "reply", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "fresh answer"}}}}
	application.Runner = continueStreamTestRunner{runner, func(opts runtime.ExecuteOptions, result *runtime.CommandResult) error {
		_, err := io.WriteString(opts.Stdout, result.Stdout)
		messages, _ := decodeContinueEvents(t, capture.snapshot())
		if len(messages) != 1 {
			t.Fatal(capture.snapshot())
		}
		return err
	}}
	if err := application.Run(context.Background(), []string{"session", "continue", "claude", "--stream"}); err != nil {
		t.Fatal(err)
	}
	messages, final := decodeContinueEvents(t, capture.snapshot())
	if final == nil || len(messages) != 1 || messages[0].ID != runner.fresh+":reply" {
		t.Fatal(capture.snapshot())
	}
}

func TestContinueStreamFailuresHaveNoResult(t *testing.T) {
	for _, failure := range []string{"execution", "cancel", "malformed", "native"} {
		t.Run(failure, func(t *testing.T) {
			application, _, runner := continueApp(t, "claude", "", continueTranscript(t, testWorkspace(t)))
			capture := &continueStreamCapture{}
			application.Stdout = capture
			runner.reply = []sessions.Message{{ID: "reply", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "answer"}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			application.Runner = continueStreamTestRunner{runner, func(opts runtime.ExecuteOptions, result *runtime.CommandResult) error {
				if failure == "malformed" {
					_, err := io.WriteString(opts.Stdout, "{broken}\n")
					return err
				}
				if failure == "native" {
					result.Stdout = "{\"type\":\"result\",\"is_error\":true,\"result\":\"failed\"}\n"
					_, err := io.WriteString(opts.Stdout, result.Stdout)
					return err
				}
				if _, err := io.WriteString(opts.Stdout, result.Stdout); err != nil {
					return err
				}
				if failure == "cancel" {
					cancel()
					return nil
				}
				return errors.New("runner failed")
			}}
			if err := application.Run(ctx, []string{"session", "continue", "claude", "--stream"}); err == nil {
				t.Fatal("expected failure")
			}
			_, final := decodeContinueEvents(t, capture.snapshot())
			if final != nil {
				t.Fatal("successful result on failure")
			}
		})
	}
}

func TestContinueStreamAgyRejectedBeforeLaunch(t *testing.T) {
	application, stdout, runner := continueApp(t, "agy", "", "")
	err := application.Run(context.Background(), []string{"session", "continue", "agy", "--stream"})
	if err == nil || !strings.Contains(err.Error(), "does not support --stream") || len(runner.plans) != 0 || stdout.Len() != 0 {
		t.Fatalf("err=%v plans=%v output=%s", err, runner.plans, stdout)
	}
}

func TestContinueStreamStoreRaces(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint("replace=", replace), func(t *testing.T) {
			workspace, home := testWorkspace(t), t.TempDir()
			capture := &continueStreamCapture{message: make(chan struct{}, 10)}
			application := New(false, strings.NewReader(""), capture, &bytes.Buffer{})
			application.Sessions = sessions.Service{HomeDir: home}
			s := newContinueTranscriptStream(application, "claude", "", workspace, "race-session", "", 0, func() {})
			t.Cleanup(func() {
				if err := s.stop(); err != nil {
					t.Error(err)
				}
			})
			// Signal before the file exists, then flush a partial store line
			// without another native stdout signal.
			if _, err := io.WriteString(s, "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"race-session\"}\n"); err != nil {
				t.Fatal(err)
			}
			path := sessions.ClaudeProjectFile(home, workspace, "race-session")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			line := `{"type":"assistant","sessionId":"race-session","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t","name":"read","input":{"path":"file"}}]}}` + "\n"
			cut := len(line) - 10
			if err := os.WriteFile(path, []byte(line[:cut]), 0600); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			err := s.readStore()
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if capture.snapshot() != "" {
				t.Fatal("partial tool input emitted")
			}
			if replace {
				if err := os.WriteFile(path+".new", []byte(line), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(line[cut:])
				closeErr := file.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			select {
			case <-capture.message:
			case <-time.After(3 * time.Second):
				t.Fatal("store flush after stdout was never emitted")
			}
			messages, _ := decodeContinueEvents(t, capture.snapshot())
			if len(messages) != 1 || messages[0].ID != "race-session:a1" {
				t.Fatal(capture.snapshot())
			}
		})
	}
}

func TestContinueStreamCodexWithholdsGrowingAssistant(t *testing.T) {
	home := t.TempDir()
	capture := &continueStreamCapture{}
	application := New(false, strings.NewReader(""), capture, &bytes.Buffer{})
	s := newContinueTranscriptStream(application, "codex", "", "/workspace", "thread", home, 0, func() {})
	defer func() {
		if err := s.stop(); err != nil {
			t.Error(err)
		}
	}()
	path := filepath.Join(home, "sessions", "2026", "11", "14", "rollout-thread.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	appendRecord := func(record string) {
		t.Helper()
		if _, err := file.WriteString(record + "\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(s, "{}\n"); err != nil {
			t.Fatal(err)
		}
	}
	appendRecord(`{"type":"session_meta","payload":{"id":"thread"}}`)
	appendRecord(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}}`)
	appendRecord(`{"type":"response_item","payload":{"type":"message","id":"a","role":"assistant","content":[{"type":"output_text","text":""}]}}`)
	appendRecord(`{"type":"response_item","payload":{"type":"function_call","call_id":"t1","name":"read","arguments":"{\"path\":\"one\"}"}}`)
	appendRecord(`{"type":"response_item","payload":{"type":"function_call","call_id":"t2","name":"read","arguments":"{\"path\":\"two\"}"}}`)
	if capture.snapshot() != "" {
		t.Fatal("growing assistant emitted")
	}
	appendRecord(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t1","output":"one"}}`)
	messages, _ := decodeContinueEvents(t, capture.snapshot())
	if len(messages) != 2 || len(messages[0].Content) != 3 {
		t.Fatal(capture.snapshot())
	}
	appendRecord(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t2","output":"two"}}`)
	appendRecord(`{"type":"response_item","payload":{"type":"message","id":"b","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)
	full, err := sessions.ParseNativeSessionFile("codex", path)
	if err != nil {
		t.Fatal(err)
	}
	full.Messages = namespaceDelta(continueDelta(full.Messages, 0), "thread")
	// Serialize final writes with the ticker, just as stop/finish do in production.
	s.mu.Lock()
	err = s.finish(full)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	messages, result := decodeContinueEvents(t, capture.snapshot())
	if result == nil || !reflect.DeepEqual(messages, result.Messages) {
		t.Fatal(capture.snapshot())
	}
}

func TestContinueStreamChangedFutureParentFailsWithoutResult(t *testing.T) {
	capture := &continueStreamCapture{}
	s := &continueTranscriptStream{app: New(false, strings.NewReader(""), capture, &bytes.Buffer{}), agent: "claude"}
	messages := []sessions.Message{{ID: "a", ParentID: "future", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "answer"}}}}
	if err := s.emit(namespaceDelta(messages, "run")); err != nil {
		t.Fatal(err)
	}
	messages = append(messages, sessions.Message{ID: "future", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "next"}}})
	if err := s.finish(sessions.Session{Messages: namespaceDelta(messages, "run")}); err == nil {
		t.Fatal("changed parent accepted")
	}
	_, result := decodeContinueEvents(t, capture.snapshot())
	if result != nil {
		t.Fatal("result after changed streamed parent")
	}
}
