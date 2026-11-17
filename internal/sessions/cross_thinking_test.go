package sessions

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestThinkingReplayIgnoresSourceAgent(t *testing.T) {
	for _, source := range []string{"claude", "codex", "pi", "agy", "unknown", ""} {
		for _, target := range []string{"claude", "codex", "pi", "agy"} {
			t.Run(source+"->"+target, func(t *testing.T) {
				detail := Session{ID: "3f9a1c2e-aaaa-4bbb-8ccc-123456789abc", Provider: source, Workspace: "/w", StartedAt: "2026-11-12T12:00:00Z", Messages: []Message{
					{ID: "u1", Role: "user", Timestamp: "2026-11-12T12:00:00Z", Content: []ContentBlock{{Type: "text", Text: "remember"}}},
					{ID: "a1", ParentID: "u1", Role: "assistant", Model: "claude-model", Timestamp: "2026-11-12T12:00:01Z", Content: []ContentBlock{
						{Type: "text", Text: "before"},
						{Type: "thinking", Text: "unique readable marker", Signature: "source-secret-signature", ID: "rs_thinking1"},
						{Type: "thinking", Signature: "inline-opaque-signature", ID: "rs_inline"},
						{Type: "text", Text: "answer"},
						{Type: "tool_use", ID: "call-1", Name: "shell", Input: map[string]any{"cmd": "ls"}},
						{Type: "thinking", Text: "after call", Signature: "other-model-signature", ID: "rs_after"},
					}},
					{ID: "a2", ParentID: "a1", Role: "assistant", Timestamp: "2026-11-12T12:00:02Z", Content: []ContentBlock{{Type: "thinking", Signature: "opaque-only-signature", ID: "rs_opaque1"}}},
					{ID: "a3", ParentID: "a2", Role: "assistant", Timestamp: "2026-11-12T12:00:03Z"},
					{ID: "u2", ParentID: "a3", Role: "user", Timestamp: "2026-11-12T12:00:04Z", Content: []ContentBlock{{Type: "text", Text: "recall"}}},
					{ID: "a4", ParentID: "u2", Role: "assistant", Model: "gpt-model", Timestamp: "2026-11-12T12:00:05Z", Content: []ContentBlock{{Type: "thinking", Text: "second model thought", Signature: "second-model-signature"}, {Type: "text", Text: "final"}}},
				}}
				before, err := json.Marshal(detail)
				if err != nil {
					t.Fatal(err)
				}
				var parsed Session
				var encoded string
				if target == "agy" {
					steps, err := SerializeAgy(detail)
					if err != nil {
						t.Fatal(err)
					}
					for _, step := range steps {
						encoded += string(step)
						p, err := agyDecode(step)
						if err != nil {
							t.Fatal(err)
						}
						if p.integer(1) == 15 {
							body, err := agyDecode(p.bytes(20))
							if err != nil {
								t.Fatal(err)
							}
							if len(body.bytes(4)) != 0 || len(body.bytes(14)) != 0 || len(body.bytes(3)) != 0 || len(body.bytes(16)) != 0 {
								t.Fatal("native thinking fields reached Agy output")
							}
						}
					}
					path := filepath.Join(t.TempDir(), "replay.db")
					if err := WriteAgyDatabaseFile(detail, path); err != nil {
						t.Fatal(err)
					}
					parsed, err = ParseAgySessionFile(path)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					serialize := map[string]func(Session) []map[string]any{"claude": SerializeClaude, "codex": SerializeCodex, "pi": SerializePi}[target]
					encoded = marshalJSONL(t, serialize(detail))
					parsed = parseNativeContract(t, target, encoded)
					for _, key := range []string{`"signature":`, `"thinkingSignature":`, `"encrypted_content":`, `"type":"reasoning"`, `"type":"thinking"`} {
						if strings.Contains(encoded, key) {
							t.Fatalf("native thinking field reached output: %s", key)
						}
					}
				}
				for _, secret := range []string{"source-secret-signature", "inline-opaque-signature", "opaque-only-signature", "other-model-signature", "second-model-signature", "rs_thinking1", "rs_inline", "rs_opaque1", "rs_after"} {
					if strings.Contains(encoded, secret) {
						t.Fatalf("opaque thinking data reached output: %s", secret)
					}
				}
				taggedOutput := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(encoded)
				if target == "claude" {
					if !strings.Contains(encoded, `"text":"`+importedNotePrefix[:len("※")]+`\nunique readable marker"`) || strings.Contains(taggedOutput, "<think>") {
						t.Fatal("readable thinking was not emitted as imported-note text")
					}
				} else if !strings.Contains(taggedOutput, "<think>") || !strings.Contains(taggedOutput, "</think>") {
					t.Fatal("readable thinking was not emitted as tagged text")
				}
				var got []ContentBlock
				for _, message := range parsed.Messages {
					if message.Role == "assistant" {
						if len(message.Content) == 0 {
							t.Fatal("empty assistant survived")
						}
						got = append(got, message.Content...)
					}
				}
				want := []ContentBlock{
					{Type: "text", Text: "before"},
					{Type: "thinking", Text: "unique readable marker"},
					{Type: "text", Text: "answer"},
					{Type: "tool_use", ID: "call-1", Name: "shell", Input: map[string]any{"cmd": "ls"}},
					{Type: "thinking", Text: "after call"},
					{Type: "thinking", Text: "second model thought"},
					{Type: "text", Text: "final"},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("blocks = %#v, want %#v", got, want)
				}
				if target == "claude" || target == "pi" {
					if len(parsed.Messages) != 4 || parsed.Messages[2].ID != "u2" || parsed.Messages[2].ParentID != "a1" || parsed.Messages[3].ParentID != "u2" {
						t.Fatalf("dropped assistants broke the parent chain: %+v", parsed.Messages)
					}
					if parsed.Messages[1].Model != "claude-model" || parsed.Messages[3].Model != "gpt-model" {
						t.Fatalf("mixed model history changed: %+v", parsed.Messages)
					}
				}
				after, err := json.Marshal(detail)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("source session was mutated")
				}
			})
		}
	}
}

func TestReplayMessagesDropsOpaqueThinkingAndRepairsParents(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "pi", "agy", "unknown", ""} {
		t.Run(provider, func(t *testing.T) {
			detail := Session{Provider: provider, Messages: []Message{
				{ID: "leading", Role: "assistant", Content: []ContentBlock{{Type: "thinking", Signature: "leading-secret"}}},
				{ID: "u", ParentID: "leading", Role: "user", Content: []ContentBlock{{Type: "text", Text: "question"}}},
				{ID: "a", ParentID: "u", Role: "assistant", Model: "model-a", Content: []ContentBlock{{Type: "text", Text: "before"}, {Type: "thinking", Text: "plan", Signature: "secret", ID: "reasoning-id"}, {Type: "thinking", Signature: "opaque"}, {Type: "text", Text: "after"}}},
				{ID: "empty", ParentID: "a", Role: "assistant"},
				{ID: "opaque", ParentID: "empty", Role: "assistant", Content: []ContentBlock{{Type: "thinking", Signature: "opaque"}, {Type: "thinking"}}},
				{ID: "u2", ParentID: "opaque", Role: "user", Content: []ContentBlock{{Type: "text", Text: "next"}}},
				{ID: "b", ParentID: "u2", Role: "assistant", Model: "model-b", Content: []ContentBlock{{Type: "thinking", Text: "other plan", Signature: "other-secret"}}},
				{ID: "trailing", ParentID: "b", Role: "assistant", Content: []ContentBlock{{Type: "thinking", Signature: "trailing-secret"}}},
			}}
			before, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			want := []Message{
				{ID: "u", Role: "user", Content: []ContentBlock{{Type: "text", Text: "question"}}},
				{ID: "a", ParentID: "u", Role: "assistant", Model: "model-a", Content: []ContentBlock{{Type: "text", Text: "before"}, {Type: "thinking", Text: "plan"}, {Type: "text", Text: "after"}}},
				{ID: "u2", ParentID: "a", Role: "user", Content: []ContentBlock{{Type: "text", Text: "next"}}},
				{ID: "b", ParentID: "u2", Role: "assistant", Model: "model-b", Content: []ContentBlock{{Type: "thinking", Text: "other plan"}}},
			}
			if got := replayMessages(detail); !reflect.DeepEqual(got, want) {
				t.Fatalf("replay = %#v, want %#v", got, want)
			}
			after, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("source session was mutated")
			}
		})
	}
}
