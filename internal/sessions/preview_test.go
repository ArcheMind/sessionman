package sessions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionPreviewsSkipRuntimeMessages(t *testing.T) {
	special := []string{
		"<local-command-caveat>context</local-command-caveat>",
		"<command-name>/model</command-name><command-message>model</command-message><command-args></command-args>",
		"<local-command-stdout>output</local-command-stdout>",
		"<local-command-stderr>error</local-command-stderr>",
		"<task-notification><task-id>task</task-id><summary>done</summary></task-notification>",
		"<subagent_notification>\n{\"status\":{\"completed\":\"done\"}}\n</subagent_notification>",
	}
	for _, agent := range []string{"claude", "codex", "pi"} {
		t.Run(agent, func(t *testing.T) {
			var lines []string
			if agent == "codex" {
				lines = append(lines, `{"type":"session_meta","payload":{"id":"preview","cwd":"/work"}}`)
			} else if agent == "pi" {
				lines = append(lines, `{"type":"session","id":"preview","cwd":"/work"}`)
			}
			texts := append(append(append([]string{}, special...), "first request", "latest request"), special...)
			for _, text := range texts {
				message := map[string]any{"role": "user", "content": text}
				record := map[string]any{"type": "user", "sessionId": "preview", "cwd": "/work", "message": message}
				if agent == "codex" {
					message["type"] = "message"
					record = map[string]any{"type": "response_item", "payload": message}
				} else if agent == "pi" {
					record["type"] = "message"
				}
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(data))
			}
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writeFixture(t, path, strings.Join(lines, "\n"))
			metadata, ok, err := readJSONLMetadata(agent, path)
			if err != nil || !ok || metadata.Title != "first request" || metadata.FirstUserMessage != "first request" {
				t.Fatalf("metadata = %+v, ok = %v, error = %v", metadata, ok, err)
			}
			for _, count := range []bool{false, true} {
				item, err := summarizeJSONL(Summary{Agent: agent, Source: path, collectUserPrompts: true}, count)
				if err != nil || item.FirstUserMessage != "first request" || item.LatestUserMessage != "latest request" || item.TurnCount != len(texts) || len(item.UserPrompts) != len(texts) {
					t.Fatalf("count=%v summary = %+v, error = %v", count, item, err)
				}
			}
		})
	}
}

func TestSessionPreviewsKeepRuntimeMessageDiscussion(t *testing.T) {
	for _, text := range []string{
		"Explain <task-notification>done</task-notification>",
		"```xml\n<subagent_notification>done</subagent_notification>\n```",
		"<task-notification>done</task-notification> what does this mean?",
		"<task-notification>incomplete",
	} {
		if !isSessionPreview(text) {
			t.Fatalf("discussion excluded: %q", text)
		}
	}
}
