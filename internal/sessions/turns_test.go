package sessions

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTurnCountExcludesToolsAndAssistantMessages(t *testing.T) {
	for _, test := range []struct {
		agent string
		lines []string
	}{
		{"claude", []string{
			`{"type":"user","message":{"role":"user","content":"first request"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"shell","input":{}}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"output"}]}}`,
			`{"type":"user","message":{"role":"user","content":"latest request"}}`,
		}},
		{"codex", []string{
			`{"type":"response_item","payload":{"type":"message","role":"user","content":"first request"}}`,
			`{"type":"event_msg","payload":{"type":"user_message","message":"first request"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":"result"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"answer"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":"latest request"}}`,
		}},
		{"pi", []string{
			`{"type":"message","message":{"role":"user","content":"first request"}}`,
			`{"type":"message","message":{"role":"toolResult","toolCallId":"t","content":[{"type":"text","text":"result"}]}}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
			`{"type":"message","message":{"role":"user","content":"latest request"}}`,
		}},
	} {
		t.Run(test.agent, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writeFixture(t, path, strings.Join(test.lines, "\n"))
			item, err := summarizeJSONL(Summary{Agent: test.agent, Source: path}, false)
			if err != nil || item.TurnCount != 2 || item.FirstUserMessage != "first request" || item.LatestUserMessage != "latest request" {
				t.Fatalf("summary = %+v, error = %v; want two turns with first/latest request", item, err)
			}
			again, err := summarizeJSONL(item, false)
			if err != nil || again.TurnCount != 2 || again.FirstUserMessage != item.FirstUserMessage || again.LatestUserMessage != item.LatestUserMessage {
				t.Fatalf("repeated summary accumulated counts or changed previews: %+v, %v", again, err)
			}
		})
	}
}

func TestRecentJSONLSummaryCollectsFullUserPrompts(t *testing.T) {
	longPrompt := "Needles " + strings.Repeat("LongPrompt ", 20)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFixture(t, path, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"session","cwd":"/work"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"FIRST prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"assistant-only marker"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"tool","output":"tool-only marker"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"# AGENTS.md instructions should still be searchable"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"` + longPrompt + `"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"Last PROMPT"}}`,
	}, "\n"))
	item, err := summarizeJSONL(Summary{Agent: "codex", Source: path, collectUserPrompts: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first prompt", "# agents.md instructions should still be searchable", strings.ToLower(longPrompt), "last prompt"}
	if !reflect.DeepEqual(item.UserPrompts, want) {
		t.Fatalf("user prompts = %#v, want %#v", item.UserPrompts, want)
	}
	if item.FirstUserMessage != "FIRST prompt" || item.LatestUserMessage != "Last PROMPT" {
		t.Fatalf("existing previews changed: first=%q latest=%q", item.FirstUserMessage, item.LatestUserMessage)
	}
	again, err := summarizeJSONL(item, false)
	if err != nil || !reflect.DeepEqual(again.UserPrompts, want) {
		t.Fatalf("repeated summary prompts = %#v, error = %v; want %#v", again.UserPrompts, err, want)
	}
}
