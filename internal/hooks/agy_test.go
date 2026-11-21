package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/hookmount"
)

func TestAgyHookSemantics(t *testing.T) {
	workspace := t.TempDir()
	payload, _ := json.Marshal(map[string]any{"conversationId": "native-id", "workspacePaths": []string{workspace}, "transcriptPath": "/native.db", "modelName": "future-model", "lastUserInput": "hello", "fullyIdle": true, "terminationReason": "complete"})
	event, err := Decode("agy", "Stop", payload)
	if err != nil {
		t.Fatal(err)
	}
	if event.Name != TurnAfter || event.SessionID != "native-id" || event.CWD != workspace || event.Model != "future-model" || event.TranscriptPath != "/native.db" {
		t.Fatalf("%+v", event)
	}
	for _, native := range []string{"SessionEnd", "UserPromptSubmit", "PreInvocation", "PostInvocation"} {
		if _, err := Decode("agy", native, payload); err == nil {
			t.Fatalf("incorrect portable mapping for %s", native)
		}
	}
	if _, err := hookmount.NativeEvent("agy", "SessionEnd"); err == nil {
		t.Fatal("SessionEnd projected")
	}
}

func TestAgyHookUsesActualWorkspace(t *testing.T) {
	workspace := t.TempDir()
	payload, _ := json.Marshal(map[string]any{"cwd": "/unusable/sessionman-test-path", "workspacePaths": []string{workspace}})
	event, err := Decode("agy", "SessionStart", payload)
	if err != nil || event.CWD != workspace {
		t.Fatalf("%+v %v", event, err)
	}
}

func TestAgyHookOutputUsesNativeContract(t *testing.T) {
	output, err := encodeOutput("agy", "SessionStart", "notice", "portable context", "", "")
	if err != nil || !strings.Contains(output, `"injectSteps"`) || strings.Contains(output, "hookSpecificOutput") || !strings.Contains(output, "portable context") {
		t.Fatalf("%s %v", output, err)
	}
	output, err = encodeOutput("agy", "Stop", "", "", "block", "finish first")
	if err != nil || !strings.Contains(output, `"decision":"continue"`) || !strings.Contains(output, "finish first") {
		t.Fatalf("%s %v", output, err)
	}
}

func TestAgyStopPreservesExecutionLoopState(t *testing.T) {
	for _, test := range []struct{ failure, outcome string }{{"", "running"}, {"native failure", "error"}} {
		payload, _ := json.Marshal(map[string]any{"conversationId": "native", "fullyIdle": false, "terminationReason": "background_pending", "error": test.failure})
		event, err := Decode("agy", "Stop", payload)
		if err != nil || event.Name != TurnAfter || event.Outcome != test.outcome || event.Reason != "background_pending" || event.Error != test.failure {
			t.Fatalf("%+v %v", event, err)
		}
		output, err := encodeOutput("agy", "Stop", "", "", "", "")
		if err != nil || !strings.Contains(output, `"decision":"stop"`) || strings.Contains(output, "SessionEnd") {
			t.Fatalf("%s %v", output, err)
		}
	}
}

func TestAgyPreToolUseDecodesAndEncodesNativeContract(t *testing.T) {
	payload := []byte(`{"conversationId":"c1","cwd":"/work","hookEventName":"PreToolUse","toolCall":{"id":"toolu_01","name":"run_command","args":{"CommandLine":"npm test"}}}`)
	event, err := Decode("agy", "PreToolUse", payload)
	if err != nil || event.Name != ToolBefore || event.SessionID != "c1" || event.ToolCall == nil || event.ToolCall.ID != "toolu_01" || event.ToolCall.Name != "run_command" || !strings.Contains(string(event.ToolCall.Args), "npm test") {
		t.Fatalf("%+v %v", event, err)
	}
	output, err := encodeOutput("agy", "PreToolUse", "", "", "block", "not allowed")
	if err != nil || output != "{\"decision\":\"deny\",\"reason\":\"not allowed\"}\n" {
		t.Fatalf("%s %v", output, err)
	}
	output, err = encodeOutput("agy", "PreToolUse", "", "", "", "")
	if err != nil || output != "{\"decision\":\"allow\"}\n" {
		t.Fatalf("%s %v", output, err)
	}
}
