package sessions

import "testing"

func TestNativeAssistantExplicitEmptyText(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			var records []map[string]any
			switch agent {
			case "claude":
				records = []map[string]any{{"type": "assistant", "sessionId": "session", "uuid": "a", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": ""}}}}}
			case "codex":
				records = []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": "session"}}, {"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": ""}, map[string]any{"type": "output_text"}}}}}
			case "pi":
				records = []map[string]any{{"type": "session", "id": "session"}, {"type": "message", "id": "a", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": ""}}}}}
			}
			session, ok := ParseRecords(agent, records)
			if !ok || len(session.Messages) != 1 || len(session.Messages[0].Content) != 1 || session.Messages[0].Content[0].Type != "text" || session.Messages[0].Content[0].Text != "" {
				t.Fatalf("explicit empty lost or absent text invented: %+v", session)
			}
		})
	}
}
