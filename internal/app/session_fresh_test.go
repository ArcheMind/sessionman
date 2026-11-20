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

	"github.com/ArcheMind/agentx/internal/sessions"
)

func userMsg(id, text string) sessions.Message {
	return sessions.Message{ID: id, Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: text}}}
}

func assistantMsg(id, text string) sessions.Message {
	return sessions.Message{ID: id, Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: text}}}
}

func toolMsg(id, toolUseID string, result any) sessions.Message {
	return sessions.Message{ID: id, Role: "tool", ToolUseID: toolUseID, Result: result}
}

func TestFreshTurnThreshold(t *testing.T) {
	messages := []sessions.Message{assistantMsg("a", strings.Repeat("a", 1_000_004)), toolMsg("t", "call", "output")}
	for i := 0; i <= 8; i++ {
		turns, large := exceedsFreshThreshold(messages)
		if turns != i || large != (i > 7) {
			t.Fatalf("%d users: turns = %d large = %t", i, turns, large)
		}
		messages = append(messages, userMsg(fmt.Sprint(i), "question"))
	}
}

func TestFreshSessionDetailOffloadsAndRechains(t *testing.T) {
	home := t.TempDir()
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: home}
	detail := sessions.Session{ID: "orig-session", Provider: "codex", Workspace: "/work"}
	for i := 1; i <= 8; i++ {
		detail.Messages = append(detail.Messages,
			userMsg(fmt.Sprintf("u%d", i), fmt.Sprintf("question %d", i)),
			toolMsg(fmt.Sprintf("t%d", i), fmt.Sprintf("call%d", i), fmt.Sprintf("complete output %d", i)),
		)
	}
	detail.MessageCount = len(detail.Messages)
	out, err := application.freshSessionDetail(detail)
	if err != nil {
		t.Fatal(err)
	}
	historyFiles, err := filepath.Glob(filepath.Join(home, ".local", "state", "sessionman", "history", "offload-*.json"))
	if err != nil || len(historyFiles) != 1 {
		t.Fatalf("history files = %v err = %v", historyFiles, err)
	}
	historyPath := historyFiles[0]
	info, err := os.Stat(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("history mode = %v", info.Mode().Perm())
	}
	data, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	var history sessions.Session
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 6 || history.MessageCount != 6 {
		t.Fatalf("history = %#v", history)
	}
	for i, turn := range []int{3, 4, 5} {
		if history.Messages[i*2].Content[0].Text != fmt.Sprintf("question %d", turn) || history.Messages[i*2+1].Result != fmt.Sprintf("complete output %d", turn) {
			t.Fatalf("history turn %d = %#v", turn, history.Messages[i*2:i*2+2])
		}
	}
	if len(out.Messages) != 11 || out.MessageCount != 11 {
		t.Fatalf("messages = %d count = %d, want 11", len(out.Messages), out.MessageCount)
	}
	header := out.Messages[4]
	want := fmt.Sprintf("Offloaded conversation history: %s — %d lines, %d turns — read it when earlier context is needed.", historyPath, bytes.Count(data, []byte("\n")), len(sessionTurns(history.Messages)))
	if header.Role != "user" || len(header.Content) != 1 || header.Content[0].Text != want {
		t.Fatalf("header = %#v", header)
	}
	for i, turn := range []int{1, 2, 6, 7, 8} {
		index := i * 2
		if i >= 2 {
			index++
		}
		if out.Messages[index].Content[0].Text != fmt.Sprintf("question %d", turn) || out.Messages[index+1].Result != fmt.Sprintf("complete output %d", turn) {
			t.Fatalf("retained turn %d = %#v", turn, out.Messages[index:index+2])
		}
	}
	for i, message := range out.Messages {
		if message.Timestamp == "" {
			t.Fatalf("messages[%d] has no timestamp", i)
		}
		if i > 0 && message.ParentID != out.Messages[i-1].ID {
			t.Fatalf("chain broken at %d: parent %q, previous id %q", i, message.ParentID, out.Messages[i-1].ID)
		}
	}
}

func TestApplySessionTurnsKeepsOffloadsAndDeletesWholeTurns(t *testing.T) {
	home := t.TempDir()
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: home}
	detail := sessions.Session{ID: "manual", Provider: "codex"}
	for i := 1; i <= 4; i++ {
		detail.Messages = append(detail.Messages,
			userMsg(fmt.Sprintf("u%d", i), fmt.Sprintf("question %d", i)),
			toolMsg(fmt.Sprintf("t%d", i), fmt.Sprintf("call%d", i), fmt.Sprintf("complete output %d", i)),
		)
	}
	out, err := application.applySessionTurns(detail, []turnDisposition{turnKeep, turnOffload, turnDelete, turnKeep})
	if err != nil {
		t.Fatal(err)
	}
	historyFiles, err := filepath.Glob(filepath.Join(home, ".local", "state", "sessionman", "history", "offload-*.json"))
	if err != nil || len(historyFiles) != 1 {
		t.Fatalf("history files = %v err = %v", historyFiles, err)
	}
	historyPath := historyFiles[0]
	data, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	var history sessions.Session
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 2 || history.MessageCount != 2 || history.Messages[0].Content[0].Text != "question 2" || history.Messages[1].Result != "complete output 2" {
		t.Fatalf("history must contain only the offloaded turn: %#v", history)
	}
	if len(out.Messages) != 5 || out.MessageCount != 5 {
		t.Fatalf("retained messages = %#v", out.Messages)
	}
	if out.Messages[0].Content[0].Text != "question 1" || out.Messages[1].Result != "complete output 1" || out.Messages[3].Content[0].Text != "question 4" || out.Messages[4].Result != "complete output 4" {
		t.Fatalf("retained turns lost content: %#v", out.Messages)
	}
	want := fmt.Sprintf("Offloaded conversation history: %s — %d lines, %d turns — read it when earlier context is needed.", historyPath, bytes.Count(data, []byte("\n")), len(sessionTurns(history.Messages)))
	if out.Messages[2].Role != "user" || out.Messages[2].Content[0].Text != want {
		t.Fatalf("history pointer = %#v", out.Messages[2])
	}
}

func TestFreshSessionDetailKeepsShortTranscriptsWhole(t *testing.T) {
	home := t.TempDir()
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: home}
	detail := sessions.Session{
		ID: "short", Provider: "claude",
		Messages: []sessions.Message{userMsg("u1", "only"), toolMsg("t1", "call1", "output"), assistantMsg("a1", "done")},
	}
	out, err := application.freshSessionDetail(detail)
	if err != nil {
		t.Fatal(err)
	}
	historyFiles, err := filepath.Glob(filepath.Join(home, ".local", "state", "sessionman", "history", "*.json"))
	if err != nil || len(historyFiles) != 0 {
		t.Fatalf("history files should not exist: %v err = %v", historyFiles, err)
	}
	if len(out.Messages) != 3 || out.MessageCount != 3 {
		t.Fatalf("messages = %d count = %d, want 3", len(out.Messages), out.MessageCount)
	}
	if out.Messages[0].Content[0].Text != "only" || out.Messages[1].Result != "output" || out.Messages[2].Content[0].Text != "done" {
		t.Fatalf("short transcript changed: %#v", out.Messages)
	}
}

func TestAutoOffloadKeyReadsAsSelectionFresh(t *testing.T) {
	for _, input := range []string{"t", "T"} {
		key, err := readSelectionKey(bufio.NewReader(strings.NewReader(input)))
		if err != nil || key != selectionFresh {
			t.Fatalf("input %q: key = %v err = %v", input, key, err)
		}
	}
}

func TestConfirmLargeContextKeys(t *testing.T) {
	for input, want := range map[string]selectionKey{"a": selectionFresh, "A": selectionFresh, "k": selectionConfirm, "K": selectionConfirm, "m": selectionManage, "M": selectionManage, "\x1b": selectionCancel, "\x03": selectionCancel} {
		var stdout bytes.Buffer
		application := New(false, strings.NewReader(input), &stdout, &bytes.Buffer{})
		key, err := application.confirmLargeContext(bufio.NewReader(application.Stdin), 12)
		if err != nil || key != want {
			t.Fatalf("input %q: key = %v err = %v", input, key, err)
		}
		if !strings.Contains(stdout.String(), "Load session · 12 turns\r\n\r\n[a] Auto offload  [k] Keep all  [m] Manual  [Esc] Cancel") {
			t.Fatalf("prompt = %q", stdout.String())
		}
	}
}

func TestChooseSessionTurnsSupportsRangeAndIndividualEdits(t *testing.T) {
	var stdout bytes.Buffer
	application := New(false, strings.NewReader("V2joGdgr\r"), &stdout, &bytes.Buffer{})
	detail := sessions.Session{Messages: []sessions.Message{
		userMsg("u1", "first"), userMsg("u2", "second"), userMsg("u3", "third"),
		userMsg("u4", "fourth"), userMsg("u5", "fifth"),
	}}
	states, err := application.chooseSessionTurns(bufio.NewReader(application.Stdin), detail)
	if err != nil {
		t.Fatal(err)
	}
	want := []turnDisposition{turnKeep, turnOffload, turnOffload, turnKeep, turnDelete}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

func TestResumeInteractiveLargeContextCancel(t *testing.T) {
	var stdout bytes.Buffer
	application := New(false, strings.NewReader("\x1b"), &stdout, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: t.TempDir()}
	detail := sessions.Session{
		ID: "big", Provider: "codex",
		Messages: []sessions.Message{userMsg("u1", "1"), userMsg("u2", "2"), userMsg("u3", "3"), userMsg("u4", "4"), userMsg("u5", "5"), userMsg("u6", "6"), userMsg("u7", "7"), userMsg("u8", "8")},
	}
	err := application.resumeSessionInteractive(context.Background(), bufio.NewReader(application.Stdin), detail, false)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stdout.String(), "Load session · 8 turns") {
		t.Fatalf("no large-context prompt in %q", stdout.String())
	}
}

func TestFreshKeyResumesAsNewNativeSessionSameAgent(t *testing.T) {
	application, _, runner := presetApp(t, "jjt\r", "codex")
	mustCreate(t, application.Hooks.Home, deepReview)
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	application.Sessions = sessions.Service{HomeDir: home}
	workspace := t.TempDir()
	path := filepath.Join(codexHome, "sessions", "2026", "11", "01", "orig.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":\"2026-11-01T10:00:00Z\",\"payload\":{\"id\":\"orig-id\",\"cwd\":%q}}\n{\"type\":\"response_item\",\"timestamp\":\"2026-11-01T10:01:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":\"original question\"}}", workspace)
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := application.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
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
	if plan.Executable != "codex" || token == "" || token == "orig-id" {
		t.Fatalf("fresh resume must launch a new native session, plan = %#v", plan)
	}
	historyFiles, err := filepath.Glob(filepath.Join(home, ".local", "state", "sessionman", "history", "*.json"))
	if err != nil || len(historyFiles) != 0 {
		t.Fatalf("history files should not exist: %v err = %v", historyFiles, err)
	}
	matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "*-"+token+".jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("rollouts = %v err = %v", matches, err)
	}
	rollout, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rollout), "Offloaded conversation history:") || !strings.Contains(string(rollout), "original question") {
		t.Fatalf("rollout = %q", rollout)
	}
}

func TestManualSessionEntrypointsImportEditsForSameAgent(t *testing.T) {
	for _, item := range []struct {
		name  string
		input string
		turns int
	}{
		{name: "home manual key", input: "jjmGd\r\r", turns: 3},
		{name: "large context manual choice", input: "jj\rmGd\r\r", turns: 8},
	} {
		t.Run(item.name, func(t *testing.T) {
			application, stdout, runner := presetApp(t, item.input, "codex")
			mustCreate(t, application.Hooks.Home, deepReview)
			home := t.TempDir()
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			application.Sessions = sessions.Service{HomeDir: home}
			workspace := t.TempDir()
			path := filepath.Join(codexHome, "sessions", "2026", "11", "01", "orig.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			var fixture strings.Builder
			fmt.Fprintf(&fixture, "{\"type\":\"session_meta\",\"timestamp\":\"2026-11-01T10:00:00Z\",\"payload\":{\"id\":\"orig-id\",\"cwd\":%q}}\n", workspace)
			for i := 1; i <= item.turns; i++ {
				text := fmt.Sprintf("retained question %d", i)
				if i == item.turns {
					text = "deleted final question"
				}
				fmt.Fprintf(&fixture, "{\"type\":\"response_item\",\"timestamp\":\"2026-11-01T10:01:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":%q}}\n", text)
			}
			if err := os.WriteFile(path, []byte(fixture.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := application.Run(context.Background(), nil); err != nil {
				t.Fatal(err)
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
			if plan.Executable != "codex" || token == "" || token == "orig-id" {
				t.Fatalf("manual edits must launch a new native session: %#v", plan)
			}
			matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "*-"+token+".jsonl"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("rollouts = %v err = %v", matches, err)
			}
			rollout, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(rollout), "retained question 1") || strings.Contains(string(rollout), "deleted final question") || strings.Contains(string(rollout), "Offloaded conversation history:") {
				t.Fatalf("manual edits were not imported: %q", rollout)
			}
			if !strings.Contains(stdout.String(), "Manual session load") {
				t.Fatalf("manual selector missing: %q", stdout.String())
			}
			if item.turns > 7 && !strings.Contains(stdout.String(), "Load session · 8 turns") {
				t.Fatalf("large context prompt missing: %q", stdout.String())
			}
		})
	}
}
