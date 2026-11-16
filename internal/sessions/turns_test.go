package sessions

import (
	"path/filepath"
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
			count, err := countSessionTurns(Summary{Agent: test.agent, Source: path})
			if err != nil || count != 2 {
				t.Fatalf("turns = %d, error = %v; want 2", count, err)
			}
		})
	}
}
