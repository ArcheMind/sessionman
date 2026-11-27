package sessions

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestSerializeDropsClaudeLocalCommands(t *testing.T) {
	detail := Session{ID: "commands", Provider: "codex", Messages: []Message{
		{ID: "command", Role: "user", Content: []ContentBlock{{Type: "text", Text: "<local-command-caveat>command marker</local-command-caveat>"}}},
		{ID: "request", ParentID: "command", Role: "user", Content: []ContentBlock{{Type: "text", Text: "actual request"}}},
	}}
	agy, err := SerializeAgy(detail)
	if err != nil {
		t.Fatal(err)
	}
	for target, native := range map[string]any{
		"claude": SerializeClaude(detail), "codex": SerializeCodex(detail),
		"pi":  SerializePi(detail),
		"agy": string(bytes.Join(agy, nil)),
	} {
		data, err := json.Marshal(native)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("command marker")) || !bytes.Contains(data, []byte("actual request")) {
			t.Fatalf("%s did not preserve only the real request: %s", target, data)
		}
	}
}

func TestReplayDropsClaudeLocalCommands(t *testing.T) {
	text := func(id, parent, role, value string) Message {
		return Message{ID: id, ParentID: parent, Role: role, Content: []ContentBlock{{Type: "text", Text: value}}}
	}
	for _, provider := range []string{"claude", "codex", "pi", "agy"} {
		t.Run(provider, func(t *testing.T) {
			messages := []Message{
				text("caveat", "", "user", "<local-command-caveat>Not a request</local-command-caveat>"),
				text("first", "caveat", "user", "actual request"),
				text("command", "first", "user", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>"),
				text("stdout", "command", "user", "<local-command-stdout>Set model</local-command-stdout>"),
				text("stderr", "stdout", "user", "<local-command-stderr>Error</local-command-stderr>"),
				text("reply", "stderr", "assistant", "answer"),
			}
			want := RechainMessages([]Message{messages[1], messages[5]})
			got := replayMessages(Session{Provider: provider, Messages: messages})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("replayed messages = %#v, want %#v", got, want)
			}
			if messages[5].ParentID != "stderr" {
				t.Fatal("conversion mutated the source transcript")
			}
		})
	}
}

func TestReplayPreservesCommandDiscussion(t *testing.T) {
	for _, value := range []string{
		"Explain <local-command-caveat>text</local-command-caveat>",
		"```xml\n<local-command-stdout>output</local-command-stdout>\n```",
		"<command-name>/model</command-name>",
		"<local-command-stdout>unclosed",
		"<command-name>/model</command-name><command-message>model</command-message><command-args></command-args> explain this",
	} {
		message := Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: value}}}
		if got := replayMessages(Session{Messages: []Message{message}}); !reflect.DeepEqual(got, []Message{message}) {
			t.Fatalf("discussion was removed: %q", value)
		}
	}
	for _, message := range []Message{
		{Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "<local-command-stdout>output</local-command-stdout>"}}},
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "<local-command-stdout>output</local-command-stdout>"}, {Type: "image"}}},
	} {
		if got := replayMessages(Session{Messages: []Message{message}}); !reflect.DeepEqual(got, []Message{message}) {
			t.Fatalf("non-command message was removed: %#v", message)
		}
	}
}
