package sessions

import (
	"encoding/json"
	"strings"
	"testing"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
)

func TestSerializeClaudeProtocolV2SafetyAndVisibility(t *testing.T) {
	parentU1 := "u1"
	parentA1 := "a1"
	parentA2 := "a2"
	ps := ProtocolSession{
		ProtocolVersion: transcript.ProtocolVersion,
		ID:              "sess-v2-claude",
		Provider:        "agy",
		Workspace:       "/w",
		StartedAt:       "2026-11-27T00:00:00Z",
		Messages: []ProtocolMessage{
			{
				ID:         "s1-id",
				Source:     transcript.SourceS1,
				Mutability: transcript.MutabilityConstant,
				Slot:       "identity",
				Content:    []ContentBlock{{Type: transcript.BlockText, Text: "Identity block"}},
				Timestamp:  "2026-11-27T00:00:00Z",
			},
			{
				ID:         "rules-id",
				Source:     transcript.SourceWorld,
				Mutability: transcript.MutabilityConstant,
				Slot:       "agents-md",
				Content:    []ContentBlock{{Type: transcript.BlockText, Text: "CLAUDE.md rules"}},
				Timestamp:  "2026-11-27T00:00:00Z",
			},
			{
				ID:         "u1",
				Source:     transcript.SourceUser,
				Mutability: transcript.MutabilityDelta,
				Content:    []ContentBlock{{Type: transcript.BlockText, Text: "hello"}},
				Timestamp:  "2026-11-27T00:00:01Z",
			},
			{
				ID:         "a1",
				ParentID:   &parentU1,
				Source:     transcript.SourceS2,
				Mutability: transcript.MutabilityDelta,
				Producer:   &Producer{Provider: "agy", Model: "gemini-3"},
				Content: []ContentBlock{
					{Type: transcript.BlockThinking, Text: "Initial hypothesis pointed toward foreign reasoning"},
					{Type: transcript.BlockText, Text: "agy reply"},
				},
				Timestamp: "2026-11-27T00:00:02Z",
			},
			{
				ID:         "a2",
				ParentID:   &parentA1,
				Source:     transcript.SourceS2,
				Mutability: transcript.MutabilityDelta,
				Producer:   &Producer{Provider: "agy", Model: "gemini-3"},
				Content: []ContentBlock{
					{Type: transcript.BlockThinking, Text: "thinking-only turn"},
				},
				Timestamp: "2026-11-27T00:00:03Z",
			},
			{
				ID:         "a3",
				ParentID:   &parentA2,
				Source:     transcript.SourceS2,
				Mutability: transcript.MutabilityDelta,
				Producer:   &Producer{Provider: "claude", Model: "claude-opus-5-5"},
				Content: []ContentBlock{
					{Type: transcript.BlockThinking, Text: "native claude thought", Signature: "sig-claude-1"},
					{Type: transcript.BlockText, Text: "claude reply"},
				},
				Timestamp: "2026-11-27T00:00:04Z",
			},
		},
		RuntimeEvents: []RuntimeEvent{
			{
				ID:       "re-1",
				Provider: "claude",
				Kind:     transcript.RuntimeSafetyRefusal,
				Code:     "reasoning_extraction",
				Message:  "API Error: Opus 5.5's safeguards flagged this message",
			},
		},
	}

	records := SerializeClaudeProtocol(ps)
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)

	// world×constant must be dropped
	if strings.Contains(encoded, "CLAUDE.md rules") {
		t.Fatalf("world×constant leaked into Claude output: %s", encoded)
	}
	// runtimeEvents must be ignored
	if strings.Contains(encoded, "safeguards flagged") || strings.Contains(encoded, "reasoning_extraction") {
		t.Fatalf("runtimeEvents leaked into Claude output: %s", encoded)
	}
	// foreign thinking must be omitted completely (no flattening, no <think>, no ※)
	if strings.Contains(encoded, "Initial hypothesis") || strings.Contains(encoded, "thinking-only turn") || strings.Contains(encoded, "※") {
		t.Fatalf("foreign thinking reached Claude output: %s", encoded)
	}
	// s1 must be emitted as user(isMeta: true), not system/informational
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4 (s1, u1, a1, a3): %s", len(records), encoded)
	}
	if records[0]["type"] != "user" || records[0]["isMeta"] != true {
		t.Fatalf("s1 record = %#v, want type=user isMeta=true", records[0])
	}
	// a2 was skipped because it only had foreign thinking; a3 must chain to a1 (no broken parentUuid)
	if records[3]["uuid"] != "a3" || records[3]["parentUuid"] != "a1" {
		t.Fatalf("a3 parentUuid = %v, want a1 (rechained after skipping a2)", records[3]["parentUuid"])
	}
	// same-provider signed thinking on a3 is preserved as native thinking block
	if !strings.Contains(encoded, `"type":"thinking"`) || !strings.Contains(encoded, `"signature":"sig-claude-1"`) {
		t.Fatalf("same-provider signed thinking was not preserved: %s", encoded)
	}
}

func TestSerializeCodexAndPiProtocolV2Channels(t *testing.T) {
	ps := ProtocolSession{
		ProtocolVersion: transcript.ProtocolVersion,
		ID:              "sess-v2-other",
		Provider:        "claude",
		Workspace:       "/w",
		StartedAt:       "2026-11-27T00:00:00Z",
		Messages: []ProtocolMessage{
			{
				ID:         "s1-1",
				Source:     transcript.SourceS1,
				Mutability: transcript.MutabilityConstant,
				Slot:       "identity",
				Content:    []ContentBlock{{Type: transcript.BlockText, Text: "s1 identity"}},
				Timestamp:  "2026-11-27T00:00:00Z",
			},
			{
				ID:         "u1",
				Source:     transcript.SourceUser,
				Mutability: transcript.MutabilityDelta,
				Content:    []ContentBlock{{Type: transcript.BlockText, Text: "hi"}},
				Timestamp:  "2026-11-27T00:00:01Z",
			},
			{
				ID:         "a1",
				Source:     transcript.SourceS2,
				Mutability: transcript.MutabilityDelta,
				Producer:   &Producer{Provider: "claude"},
				Content: []ContentBlock{
					{Type: transcript.BlockThinking, Text: "foreign thought"},
					{Type: transcript.BlockText, Text: "final answer"},
				},
				Timestamp: "2026-11-27T00:00:02Z",
			},
		},
	}

	codexRaw, _ := json.Marshal(SerializeCodexProtocol(ps))
	codexStr := string(codexRaw)
	if !strings.Contains(codexStr, `"role":"developer"`) || !strings.Contains(codexStr, `"foreign thought"`) {
		t.Fatalf("expected foreign thinking in Codex developer channel: %s", codexStr)
	}
	if strings.Contains(codexStr, "<think>") {
		t.Fatalf("unexpected <think> wrapper in Codex output: %s", codexStr)
	}

	piRaw, _ := json.Marshal(SerializePiProtocol(ps))
	piStr := string(piRaw)
	if !strings.Contains(piStr, `"role":"system"`) || !strings.Contains(piStr, `"foreign thought"`) {
		t.Fatalf("expected foreign thinking in Pi system channel: %s", piStr)
	}
	if strings.Contains(piStr, "<think>") {
		t.Fatalf("unexpected <think> wrapper in Pi output: %s", piStr)
	}
}
