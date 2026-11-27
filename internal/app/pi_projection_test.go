package app

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

func TestPiContinueProjectsHistoryInstructionsWithStreamParity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "final", true: "stream"}[stream], func(t *testing.T) {
			workspace := testWorkspace(t)
			detail := sessions.Session{ID: "turn", Provider: "agy", Workspace: workspace, Messages: []sessions.Message{
				{ID: "s", Role: "system", Content: []sessions.ContentBlock{{Type: "text", Text: "history instructions"}}},
				{ID: "u", ParentID: "s", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "earlier"}}},
				{ID: "s2", ParentID: "u", Role: "system", Content: []sessions.ContentBlock{{Type: "text", Text: "later instructions"}}},
				{ID: "wake", ParentID: "s2", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "next"}}},
			}}
			input, _ := json.Marshal(detail)
			app, stdout, runner := continueApp(t, "pi", "provider/model", string(input))
			runner.reply = []sessions.Message{
				{ID: "setup", Role: "system", Content: []sessions.ContentBlock{{Type: "text", Text: "history instructions\n\nlater instructions\n\ncurrent"}}},
				{ID: "call", Role: "assistant", Content: []sessions.ContentBlock{{Type: "tool_use", ID: "bash-call", Name: "bash", Input: map[string]any{"command": "pwd"}}}},
				{ID: "result", ParentID: "call", Role: "tool", ToolName: "bash", ToolUseID: "bash-call", Result: "/work"},
				{ID: "answer", ParentID: "result", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "done"}}},
			}
			app.Runner = continueStreamTestRunner{
				continueRunner: runner,
				piBeforeEmit: func(_ func(string)) error {
					plan := runner.plans[len(runner.plans)-1]
					if got := flagValue(plan.Args, "--append-system-prompt"); got != "history instructions\n\nlater instructions\n\ncurrent" {
						t.Fatalf("append=%q", got)
					}
					// The fake runner has now recorded its reply. Inspect the imported prefix only.
					path := flagValue(plan.Args, "--session")
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					lines := strings.Split(strings.TrimSpace(string(data)), "\n")
					var first map[string]any
					if err := json.Unmarshal([]byte(lines[1]), &first); err != nil {
						return err
					}
					if m := first["message"].(map[string]any); m["role"] != "user" || first["parentId"] != nil {
						t.Fatalf("unsafe imported prefix: %s", lines[1])
					}
					return nil
				},
			}
			args := []string{"session", "continue", "pi", "--model", "provider/model", "--append-system-prompt", "current"}
			if stream {
				args = append(args, "--stream")
			}
			if err := app.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			var delta sessions.ProtocolSession
			if stream {
				entries, final := decodeContinueEvents(t, stdout.String())
				if final == nil || !reflect.DeepEqual(entries, final.Entries) {
					t.Fatal(stdout.String())
				}
				delta = *final
			} else {
				delta = decodeDelta(t, stdout)
			}
			if len(delta.Entries) != 3 || delta.Entries[0].Source != "s2" || delta.Entries[1].ToolResult == nil || delta.Entries[1].ToolResult.ToolName != "bash" {
				t.Fatalf("delta=%+v", delta)
			}
		})
	}
}

func TestPiResumeProjectsOnlyImportedInstructions(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "import", true: "native"}[native], func(t *testing.T) {
			app, _, runner := presetApp(t, "", "pi")
			app.Output = OutputText
			cwd, _ := os.Getwd()
			detail := sessions.Session{ID: "11111111-2222-4333-8444-555555555555", Provider: "agy", Workspace: cwd, Messages: []sessions.Message{
				{ID: "s", Role: "system", Content: []sessions.ContentBlock{{Type: "text", Text: "history"}}},
				{ID: "u", ParentID: "s", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "go"}}},
			}}
			if native {
				detail.Provider = "pi"
			}
			if err := app.resumePreparedSession(context.Background(), runtimeAgent(t, app, "pi"), detail, runtime.RunRequest{SystemPrompt: "current"}, false, false, false); err != nil {
				t.Fatal(err)
			}
			plan := runner.plans[len(runner.plans)-1]
			want := "history\n\ncurrent"
			if native {
				want = "current"
			}
			if got := flagValue(plan.Args, "--append-system-prompt"); got != want {
				t.Fatalf("append=%q want=%q", got, want)
			}
			if !native {
				parsed, err := sessions.ParseNativeSessionFile("pi", flagValue(plan.Args, "--session"))
				if err != nil {
					t.Fatal(err)
				}
				if len(parsed.Messages) != 1 || parsed.Messages[0].Role != "user" || parsed.Messages[0].ParentID != "" {
					t.Fatalf("import=%+v", parsed)
				}
			}
		})
	}
}

func TestPiStreamBuffersReplaceableSystemTail(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "clear"}[clear], func(t *testing.T) {
			workspace := testWorkspace(t)
			app, _, runner := continueApp(t, "pi", "provider/model", continueTranscript(t, workspace))
			capture := &continueStreamCapture{}
			app.Stdout = capture
			runner.reply = []sessions.Message{
				{ID: "call", Role: "assistant", Content: []sessions.ContentBlock{{Type: "tool_use", ID: "bash-call", Name: "bash", Input: map[string]any{"command": "pwd"}}}},
				{ID: "tool", ParentID: "call", Role: "tool", ToolName: "bash", ToolUseID: "bash-call", Result: "/work"},
			}
			app.Runner = continueStreamTestRunner{
				continueRunner: runner,
				piBeforeEmit: func(_ func(string)) error {
					path := flagValue(runner.plans[len(runner.plans)-1].Args, "--session")
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					var rebuilt strings.Builder
					encoder := json.NewEncoder(&rebuilt)
					for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
						var record map[string]any
						if err := json.Unmarshal([]byte(line), &record); err != nil {
							return err
						}
						if record["id"] == "call" {
							if err := encoder.Encode(map[string]any{"type": "message", "id": "initial-a", "parentId": record["parentId"], "message": map[string]any{"role": "system", "sections": map[string]any{"addendum": "<addendum>\nA\n</addendum>"}}}); err != nil {
								return err
							}
							record["parentId"] = "initial-a"
						}
						if err := encoder.Encode(record); err != nil {
							return err
						}
					}
					return os.WriteFile(path, []byte(rebuilt.String()), 0600)
				},
				piHook: func(emit func(string)) error {
					path := flagValue(runner.plans[len(runner.plans)-1].Args, "--session")
					deadline := time.Now().Add(2 * time.Second)
					for {
						messages, final := decodeContinueEvents(t, capture.snapshot())
						if len(messages) == 2 && final == nil && messages[0].ParentID == nil {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("tools not streamed immediately: %s", capture.snapshot())
						}
						time.Sleep(10 * time.Millisecond)
					}
					appendRecord := func(id, parent string, message map[string]any) error {
						file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
						if err != nil {
							return err
						}
						defer file.Close()
						return json.NewEncoder(file).Encode(map[string]any{"type": "message", "id": id, "parentId": parent, "message": message})
					}
					if err := appendRecord("patch-a", "tool", map[string]any{"role": "system", "sections": map[string]any{"addendum": "<addendum>\nA\n</addendum>"}}); err != nil {
						return err
					}
					if err := appendRecord("middle", "patch-a", map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "working"}}}); err != nil {
						return err
					}
					emit(`{"type":"turn_update"}`)
					time.Sleep(50 * time.Millisecond)
					messages, _ := decodeContinueEvents(t, capture.snapshot())
					if len(messages) != 2 {
						t.Fatalf("mutable tail emitted: %s", capture.snapshot())
					}
					var value any = "<addendum>\nB\n</addendum>"
					if clear {
						value = nil
					}
					if err := appendRecord("patch-b", "middle", map[string]any{"role": "system", "sections": map[string]any{"addendum": value}}); err != nil {
						return err
					}
					if err := appendRecord("answer", "patch-b", map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}}); err != nil {
						return err
					}
					emit(`{"type":"turn_update"}`)
					return nil
				},
			}
			if err := app.Run(context.Background(), []string{"session", "continue", "pi", "--model", "provider/model", "--stream"}); err != nil {
				t.Fatal(err)
			}
			entries, final := decodeContinueEvents(t, capture.snapshot())
			if final == nil || !reflect.DeepEqual(entries, final.Entries) {
				t.Fatalf("stream/final mismatch: %s", capture.snapshot())
			}
			_, prompt := sessions.PreparePiProtocolTranscript(*final, "")
			want := "B"
			if clear {
				want = ""
			}
			if prompt != want {
				t.Fatalf("restored prompt=%q want=%q", prompt, want)
			}
			if len(entries) < 4 || entries[2].Source != "s2" || entries[2].ParentID == nil || *entries[2].ParentID != entries[1].ID {
				t.Fatalf("broken filtered parent: %+v", entries)
			}
		})
	}
}
