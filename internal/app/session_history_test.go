package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/sessions"
	"gopkg.in/yaml.v3"
)

func TestOffloadRestoreTwoRoundTrips(t *testing.T) {
	for _, item := range []struct {
		name   string
		states []turnDisposition
		files  int
	}{
		{"continuous", []turnDisposition{turnKeep, turnOffload, turnOffload, turnKeep}, 1},
		{"discontinuous", []turnDisposition{turnOffload, turnKeep, turnOffload, turnKeep}, 2},
		{"all", []turnDisposition{turnOffload, turnOffload, turnOffload, turnOffload}, 1},
	} {
		t.Run(item.name, func(t *testing.T) {
			application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
			application.Sessions = sessions.Service{HomeDir: t.TempDir()}
			original := sessions.Session{ID: "roundtrip", Provider: "codex", Messages: []sessions.Message{
				{ID: "msg_system", Role: "system", Content: []sessions.ContentBlock{{Type: "text", Text: "instructions"}}},
			}}
			for i := 0; i < 4; i++ {
				call := fmt.Sprintf("call%d", i)
				original.Messages = append(original.Messages,
					userMsg(fmt.Sprintf("msg_u%d", i), fmt.Sprintf("question %d\nsecond line", i)),
					sessions.Message{ID: fmt.Sprintf("msg_a%d", i), Role: "assistant", Content: []sessions.ContentBlock{
						{Type: "text", Text: "checking"},
						{Type: "tool_use", ID: call, Name: "read", Input: map[string]any{"path": "a file"}},
					}},
					toolMsg(fmt.Sprintf("msg_t%d", i), call, "first line\nsecond line"),
					assistantMsg(fmt.Sprintf("msg_end%d", i), "answer"),
				)
			}
			for i := range original.Messages {
				original.Messages[i].Timestamp = "2026-11-20T10:00:00Z"
			}
			original.Messages = sessions.RechainMessages(original.Messages)
			original.MessageCount = len(original.Messages)
			want, err := json.Marshal(original.Messages)
			if err != nil {
				t.Fatal(err)
			}
			current := original
			for round := 1; round <= 2; round++ {
				offloaded, err := application.applySessionTurns(current, item.states)
				if err != nil {
					t.Fatal(err)
				}
				pointers := 0
				for _, message := range offloaded.Messages {
					path := offloadHistoryPath(message)
					if path == "" {
						continue
					}
					pointers++
					if !strings.HasSuffix(path, ".yaml") {
						t.Fatalf("expected .yaml offload file, got %s", path)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var history sessions.Session
					if err := yaml.Unmarshal(data, &history); err != nil {
						t.Fatal(err)
					}
					stats := fmt.Sprintf(" — %d lines, %d turns", bytes.Count(data, []byte("\n")), len(sessionTurns(history.Messages)))
					if !strings.Contains(message.Content[0].Text, stats) {
						t.Fatalf("missing statistics: %#v", message)
					}
				}
				if pointers != item.files {
					t.Fatalf("round %d pointers = %d, want %d", round, pointers, item.files)
				}
				data, err := json.Marshal(offloaded)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &offloaded); err != nil {
					t.Fatal(err)
				}
				var restored bool
				current, restored, err = loadSessionHistory(offloaded)
				if err != nil || !restored {
					t.Fatalf("round %d restored = %t, err = %v", round, restored, err)
				}
				got, err := json.Marshal(current.Messages)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) || current.MessageCount != original.MessageCount || len(sessionTurns(current.Messages)) != 4 {
					t.Fatalf("round %d changed transcript:\ngot %s\nwant %s", round, got, want)
				}
				if _, changed, err := loadSessionHistory(current); err != nil || changed {
					t.Fatalf("restoring twice: changed = %t err = %v", changed, err)
				}
			}
		})
	}
}

func TestLoadSessionHistoryFailures(t *testing.T) {
	for _, kind := range []string{"missing", "invalid", "empty", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.json")
			pointer := freshHistoryMessage(path, 1, 1)
			var data []byte
			switch kind {
			case "invalid":
				data = []byte("{broken")
			case "empty":
				data = []byte(`{"messages":[]}`)
			case "cycle":
				var err error
				data, err = json.Marshal(sessions.Session{Messages: []sessions.Message{pointer}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if data != nil {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := loadSessionHistory(sessions.Session{Messages: []sessions.Message{pointer}})
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected error with history path: %v", err)
			}
		})
	}
}

func TestLoadSessionHistoryRestoresNestedLegacyReferences(t *testing.T) {
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: filepath.Join(t.TempDir(), "home with spaces")}
	original := sessions.Session{Messages: []sessions.Message{userMsg("msg_question", "original question")}}
	pointer, err := application.offloadSessionHistory(original)
	if err != nil {
		t.Fatal(err)
	}
	path := offloadHistoryPath(pointer)
	pointer.Content[0].Text = offloadHistoryPrefix + path + offloadHistorySuffix
	nested, err := application.offloadSessionHistory(sessions.Session{Messages: []sessions.Message{pointer}})
	if err != nil {
		t.Fatal(err)
	}
	restored, changed, err := loadSessionHistory(sessions.Session{Messages: []sessions.Message{nested}})
	if err != nil || !changed {
		t.Fatalf("restored = %t err = %v", changed, err)
	}
	if len(restored.Messages) != 1 || restored.Messages[0].Content[0].Text != "original question" {
		t.Fatalf("nested history was not expanded: %#v", restored)
	}
}

func TestResumeRestoresHistoryBeforeSelectionAndNativeLaunch(t *testing.T) {
	for _, mode := range []string{"manual", "manual offload", "keep", "auto", "cli", "file"} {
		t.Run(mode, func(t *testing.T) {
			input := "k\r"
			if mode == "manual" {
				input = "\r\r"
			}
			if mode == "auto" {
				input = "a\r"
			}
			if mode == "manual offload" {
				input = "jo\r\r"
			}
			application, stdout, runner := presetApp(t, input, "codex")
			application.Output = OutputText
			mustCreate(t, application.Hooks.Home, deepReview)
			home := t.TempDir()
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			application.Sessions = sessions.Service{HomeDir: home}
			detail := sessions.Session{ID: "original", Provider: "codex", Workspace: t.TempDir()}
			for i := 1; i <= 8; i++ {
				detail.Messages = append(detail.Messages, userMsg(fmt.Sprintf("u%d", i), fmt.Sprintf("question %d", i)))
			}
			offloaded, err := application.freshSessionDetail(detail)
			if err != nil {
				t.Fatal(err)
			}
			// Read through the native format so reference recognition cannot rely on message IDs.
			offloaded, ok := sessions.ParseRecords("codex", sessions.SerializeCodex(offloaded))
			if !ok {
				t.Fatal("parse native transcript")
			}
			if mode == "cli" {
				err = application.resumeSession(context.Background(), runtimeAgent(t, application, "codex"), offloaded, deepReview.Request(), false, false)
			} else if mode == "file" {
				path := filepath.Join(t.TempDir(), "transcript.json")
				data, err := json.Marshal(offloaded)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				err = application.resumeTranscriptFile(context.Background(), path)
			} else {
				err = application.resumeSessionInteractiveMode(context.Background(), bufio.NewReader(application.Stdin), offloaded, false, (mode == "manual" || mode == "manual offload"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if (mode == "manual" || mode == "manual offload") && !strings.Contains(stdout.String(), "Manual session load · 8 turns") {
				t.Fatalf("manual selector did not expand history: %s", stdout)
			}
			if (mode == "keep" || mode == "auto") && !strings.Contains(stdout.String(), "Load session · 8 turns") {
				t.Fatalf("turn count did not expand history: %s", stdout)
			}
			if len(runner.plans) != 1 {
				t.Fatalf("plans = %#v", runner.plans)
			}
			plan := runner.plans[0]
			token := ""
			for i, arg := range plan.Args {
				if arg == "resume" && i+1 < len(plan.Args) {
					token = plan.Args[i+1]
				}
			}
			if token == "" || token == "original" {
				t.Fatalf("restored content was not imported: %#v", plan)
			}
			paths, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "*-"+token+".jsonl"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("rollouts = %v err = %v", paths, err)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if mode == "auto" || mode == "manual offload" {
				absent := "question 3"
				if mode == "manual offload" {
					absent = "question 2"
				}
				if !bytes.Contains(data, []byte(offloadHistoryPrefix)) || bytes.Contains(data, []byte(absent)) {
					t.Fatalf("fresh offload expanded prematurely: %s", data)
				}
			} else {
				if bytes.Contains(data, []byte(offloadHistoryPrefix)) {
					t.Fatalf("reference survived restore: %s", data)
				}
				for i := 1; i <= 8; i++ {
					if !bytes.Contains(data, []byte(fmt.Sprintf("question %d", i))) {
						t.Fatalf("missing turn %d: %s", i, data)
					}
				}
			}
		})
	}
}

func TestOffloadHistoryLeadingTabBlockScalarRoundTrip(t *testing.T) {
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: t.TempDir()}

	tabResult1 := "\treturn true\n}"
	tabResult2 := "\tline1\n\tline2\n"
	original := sessions.Session{
		ID:       "tab-roundtrip",
		Provider: "claude",
		Messages: []sessions.Message{
			userMsg("msg_u1", "check code"),
			{ID: "msg_a1", Role: "assistant", Content: []sessions.ContentBlock{
				{Type: "tool_use", ID: "call1", Name: "Bash", Input: map[string]any{"command": "sed -n 1,5p a.go"}},
			}},
			toolMsg("msg_t1", "call1", tabResult1),
			toolMsg("msg_t2", "call2", tabResult2),
			assistantMsg("msg_end1", "done"),
			userMsg("msg_u2", "next"),
			assistantMsg("msg_a2", "ok"),
		},
	}

	offloaded, err := application.applySessionTurns(original, []turnDisposition{turnOffload, turnKeep})
	if err != nil {
		t.Fatal(err)
	}
	restored, changed, err := loadSessionHistory(offloaded)
	if err != nil || !changed {
		t.Fatalf("loadSessionHistory() changed = %t, err = %v", changed, err)
	}
	if restored.Messages[2].Result != tabResult1 {
		t.Fatalf("restored tool result 1 = %q, want %q", restored.Messages[2].Result, tabResult1)
	}
	if restored.Messages[3].Result != tabResult2 {
		t.Fatalf("restored tool result 2 = %q, want %q", restored.Messages[3].Result, tabResult2)
	}
}

func TestUnmarshalOffloadHistoryRepairsLegacyTabBlockScalar(t *testing.T) {
	legacyYAML := []byte(`id: legacy-tab
provider: claude
messages:
  - id: msg_1
    role: tool
    result: |-
      ` + "\t" + `return true
      }
`)
	var out sessions.Session
	if err := unmarshalOffloadHistory(legacyYAML, &out); err != nil {
		t.Fatalf("unmarshalOffloadHistory() error = %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Result != "\treturn true\n}" {
		t.Fatalf("unexpected decoded messages: %#v", out.Messages)
	}
}
