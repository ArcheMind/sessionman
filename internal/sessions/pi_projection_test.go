package sessions

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPiStructuredSystemOnlyExportsAddendumChanges(t *testing.T) {
	native := []string{
		`{"type":"session","id":"s"}`,
		`{"type":"message","id":"setup","message":{"role":"system","content":"default content","sections":{"preamble":"builtin","rules":"builtin rules","tools":"bash schema","addendum":"<addendum>\nkeep this\n</addendum>"}}}`,
		`{"type":"message","id":"u","parentId":"setup","message":{"role":"user","content":"go"}}`,
		`{"type":"message","id":"tools","parentId":"u","message":{"role":"system","sections":{"tools":"new schema"}}}`,
		`{"type":"message","id":"same","parentId":"tools","message":{"role":"system","sections":{"addendum":"<addendum>\nkeep this\n</addendum>"}}}`,
		`{"type":"message","id":"a","parentId":"same","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
		`{"type":"message","id":"changed","parentId":"a","message":{"role":"system","sections":{"addendum":"<addendum>\nkeep this\n\nand this\n</addendum>"}}}`,
		`{"type":"message","id":"clear","parentId":"changed","message":{"role":"system","sections":{"addendum":null}}}`,
		`{"type":"message","id":"again","parentId":"clear","message":{"role":"system","sections":{"addendum":"<addendum>\nkeep this\n</addendum>"}}}`,
	}
	var records []map[string]any
	for _, line := range native {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	got, ok := ParseRecords("pi", records)
	if !ok {
		t.Fatal("not parsed")
	}
	assertMessageRoles(t, got.Messages, []string{"user", "assistant", "system"})
	if got.Messages[0].ParentID != "" || got.Messages[1].ParentID != "u" || got.Messages[2].ParentID != "a" {
		t.Fatalf("dangling filtered parent: %+v", got.Messages)
	}
	var texts []string
	for _, message := range got.Messages {
		if message.Role == "system" {
			texts = append(texts, TextContent(message.Content))
		}
	}
	if !reflect.DeepEqual(texts, []string{"keep this"}) {
		t.Fatal(texts)
	}
	path := filepath.Join(t.TempDir(), "pi.jsonl")
	writeFixture(t, path, strings.Join(native, "\n"))
	item, err := summarizeJSONL(Summary{Agent: "pi", Source: path}, true)
	if err != nil || item.MessageCount != len(got.Messages) {
		t.Fatalf("summary=%+v error=%v", item, err)
	}
}

func TestPreparePiTranscriptMovesInstructionsToAppendPrompt(t *testing.T) {
	detail := Session{Messages: []Message{
		{ID: "s", Role: "system", Content: []ContentBlock{{Type: "text", Text: "first"}}},
		{ID: "u", ParentID: "s", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
		{ID: "s2", ParentID: "u", Role: "system", Content: []ContentBlock{{Type: "text", Text: "second"}}},
		{ID: "a", ParentID: "s2", Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "hi"}}},
	}}
	got, prompt := PreparePiTranscript(detail, "current")
	if prompt != "first\n\nsecond\n\ncurrent" {
		t.Fatal(prompt)
	}
	if got.MessageCount != 2 || got.Messages[0].ParentID != "" || got.Messages[1].ParentID != "u" {
		t.Fatalf("chain: %+v", got)
	}
	for _, record := range SerializePi(got) {
		message, _ := record["message"].(map[string]any)
		if message["role"] == "system" {
			t.Fatal(record)
		}
	}
	if len(detail.Messages) != 4 || detail.Messages[1].ParentID != "s" {
		t.Fatal("mutated source")
	}
	if !strings.Contains(marshalJSONL(t, SerializePi(detail)), `"role":"system"`) {
		t.Fatal("pure conversion lost system")
	}
}

func TestPiAddendumProjectionRestoresOnlyCurrentValue(t *testing.T) {
	for _, value := range []any{"<addendum>\nB\n</addendum>", nil} {
		t.Run(map[bool]string{true: "replace", false: "clear"}[value != nil], func(t *testing.T) {
			records := []map[string]any{
				{"type": "session", "id": "s"},
				{"type": "message", "id": "s1", "message": map[string]any{"role": "system", "sections": map[string]any{"addendum": "<addendum>\nA\n</addendum>"}}},
				{"type": "message", "id": "u", "parentId": "s1", "message": map[string]any{"role": "user", "content": "go"}},
				{"type": "message", "id": "s2", "parentId": "u", "message": map[string]any{"role": "system", "sections": map[string]any{"addendum": value}}},
				{"type": "message", "id": "a", "parentId": "s2", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}}},
			}
			got, ok := ParseRecords("pi", records)
			if !ok {
				t.Fatal("parse")
			}
			_, prompt := PreparePiTranscript(got, "")
			want := "B"
			if value == nil {
				want = ""
			}
			if prompt != want {
				t.Fatalf("prompt=%q want=%q", prompt, want)
			}
		})
	}
}
