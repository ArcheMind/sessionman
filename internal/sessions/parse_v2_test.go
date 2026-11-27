package sessions

import (
	"testing"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
)

func TestParseClaudeProtocolClassificationAndRuntimeEvents(t *testing.T) {
	records := []map[string]any{
		{
			"type": "attachment", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:00Z", "uuid": "att-env",
			"attachment": map[string]any{"type": "environment", "content": "OS: macOS"},
		},
		{
			"type": "attachment", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:00Z", "uuid": "att-mem",
			"attachment": map[string]any{"type": "relevant_memories", "content": "memory note"},
		},
		{
			"type": "user", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:01Z", "uuid": "u-meta",
			"isMeta":  true,
			"message": map[string]any{"role": "user", "content": "identity s1"},
		},
		{
			"type": "user", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:02Z", "uuid": "u-1",
			"message": map[string]any{"role": "user", "content": "hello world"},
		},
		{
			"type": "assistant", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:03Z", "uuid": "a-refusal",
			"isApiErrorMessage": true,
			"message": map[string]any{
				"role": "assistant", "model": "<synthetic>", "stop_reason": "refusal",
				"content": []any{map[string]any{"type": "text", "text": "API Error: safeguards flagged [reasoning_extraction]"}},
			},
		},
		{
			"type": "assistant", "sessionId": "s-claude", "cwd": "/w", "timestamp": "2026-11-27T00:00:04Z", "uuid": "a-1",
			"message": map[string]any{
				"role": "assistant", "model": "claude-opus-5-5", "stop_reason": "end_turn",
				"content": []any{map[string]any{"type": "text", "text": "normal reply"}},
			},
		},
	}

	ps, ok := ParseProtocolRecords("claude", records, "mem")
	if !ok {
		t.Fatal("ParseProtocolRecords(claude) returned false")
	}
	if len(ps.Messages) != 4 {
		t.Fatalf("got %d entries, want 4 (mem, u-meta, u-1, a-1): %+v", len(ps.Messages), ps.Messages)
	}
	if ps.Messages[0].Source != transcript.SourceS1 || ps.Messages[0].Mutability != transcript.MutabilityCurrent {
		t.Fatalf("relevant_memories = %s×%s, want s1×current", ps.Messages[0].Source, ps.Messages[0].Mutability)
	}
	if ps.Messages[1].Source != transcript.SourceS1 || ps.Messages[1].Mutability != transcript.MutabilityDelta {
		t.Fatalf("isMeta user = %s×%s, want s1×delta", ps.Messages[1].Source, ps.Messages[1].Mutability)
	}
	if ps.Messages[2].Source != transcript.SourceUser || ps.Title != "hello world" {
		t.Fatalf("user entry/title = %+v title=%q", ps.Messages[2], ps.Title)
	}
	if len(ps.RuntimeEvents) != 1 || ps.RuntimeEvents[0].Kind != transcript.RuntimeSafetyRefusal || ps.RuntimeEvents[0].Code != "reasoning_extraction" {
		t.Fatalf("runtimeEvents = %+v, want 1 safety_refusal(reasoning_extraction)", ps.RuntimeEvents)
	}
}

func TestParseCodexProtocolStripsCarrierAndSeparatesTurnAborted(t *testing.T) {
	records := []map[string]any{
		{"type": "session_meta", "timestamp": "2026-11-27T00:00:00Z", "payload": map[string]any{"id": "s-codex", "cwd": "/w", "timestamp": "2026-11-27T00:00:00Z"}},
		{"type": "response_item", "timestamp": "2026-11-27T00:00:01Z", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "<permissions instructions>sandbox</permissions instructions>"}}}},
		{"type": "response_item", "timestamp": "2026-11-27T00:00:02Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>...</environment_context>"}}}},
		{"type": "response_item", "timestamp": "2026-11-27T00:00:03Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "# AGENTS.md instructions for /w"}}}},
		{"type": "response_item", "timestamp": "2026-11-27T00:00:04Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "real question"}}}},
		{"type": "response_item", "timestamp": "2026-11-27T00:00:05Z", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "<turn_aborted>cancelled</turn_aborted>"}}}},
	}
	ps, ok := ParseProtocolRecords("codex", records, "mem")
	if !ok {
		t.Fatal("ParseProtocolRecords(codex) returned false")
	}
	if len(ps.Messages) != 2 {
		t.Fatalf("got %d entries, want 2 (world×constant + user×delta): %+v", len(ps.Messages), ps.Messages)
	}
	if ps.Messages[0].Source != transcript.SourceWorld || ps.Messages[0].Mutability != transcript.MutabilityConstant {
		t.Fatalf("AGENTS.md = %s×%s, want world×constant", ps.Messages[0].Source, ps.Messages[0].Mutability)
	}
	if ps.Messages[1].Source != transcript.SourceUser || transcript.TextContent(ps.Messages[1]) != "real question" {
		t.Fatalf("user entry = %+v", ps.Messages[1])
	}
	if len(ps.RuntimeEvents) != 1 || ps.RuntimeEvents[0].Kind != transcript.RuntimeTurnAborted {
		t.Fatalf("runtimeEvents = %+v, want 1 turn_aborted", ps.RuntimeEvents)
	}
}

func TestUpgradeAgyLegacyLiftsUnclosedThink(t *testing.T) {
	legacy := Session{
		ID: "s-agy", Provider: "agy", Workspace: "/w",
		Messages: []Message{
			{ID: "u1", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
			{ID: "a1", Role: "assistant", Content: []ContentBlock{
				{Type: "text", Text: "<think>\nLook at the latest notification"},
				{Type: "text", Text: "final visible answer"},
			}},
		},
	}
	ps, err := upgradeAgyLegacy(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Messages[1].Content[0].Type != transcript.BlockThinking {
		t.Fatalf("unclosed <think> was not lifted to thinking: %+v", ps.Messages[1].Content)
	}
	// When serialized to Claude, that lifted thinking must be omitted completely.
	records := SerializeClaudeProtocol(ps)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(records), records)
	}
	blocks := records[1]["message"].(map[string]any)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "final visible answer" {
		t.Fatalf("unexpected Claude assistant blocks: %+v", blocks)
	}
}
