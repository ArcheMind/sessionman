package sessions

import (
	"encoding/json"
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
	assertMessageRoles(t, got.Messages, []string{"system", "user", "assistant", "system", "system"})
	if got.Messages[2].ParentID != "u" || got.Messages[4].ParentID != "changed" {
		t.Fatalf("dangling filtered parent: %+v", got.Messages)
	}
	var texts []string
	for _, message := range got.Messages {
		if message.Role == "system" {
			texts = append(texts, TextContent(message.Content))
		}
	}
	if !reflect.DeepEqual(texts, []string{"keep this", "keep this\n\nand this", "keep this"}) {
		t.Fatal(texts)
	}
	decode := jsonlRecordDecoder("pi")
	count := 0
	for _, record := range records {
		count += len(decode(record).Messages)
	}
	if count != len(got.Messages) {
		t.Fatalf("summary count=%d, canonical count=%d", count, len(got.Messages))
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
