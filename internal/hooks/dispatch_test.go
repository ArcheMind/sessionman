package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/ArcheMind/agentx/internal/hooks"
	"github.com/ArcheMind/agentx/internal/runtime"
)

func TestDispatchPresentsCanonicalCommandIOAcrossAgents(t *testing.T) {
	tests := []struct {
		agent   string
		event   string
		payload string
	}{
		{"claude", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/work","prompt":"review this"}`},
		{"codex", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/work","prompt":"review this"}`},
		{"pi", "UserPromptSubmit", `{"hook_event_name":"input","session_id":"s1","cwd":"/work","text":"review this"}`},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			runner := &recordingHookRunner{stdout: `{"systemMessage":"checked","hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"policy context"}}`}
			result, err := hooks.DispatchOne(context.Background(), runner, test.agent, test.event, []byte(test.payload), hooks.Handler{Command: "policy-check", Timeout: 600})
			if err != nil {
				t.Fatal(err)
			}
			input := runner.singleInput(t)
			if input["hook_event_name"] != "UserPromptSubmit" || input["prompt"] != "review this" || input["session_id"] != "s1" {
				t.Fatalf("canonical input = %#v", input)
			}
			var output map[string]any
			if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
				t.Fatal(err)
			}
			specific := output["hookSpecificOutput"].(map[string]any)
			if output["systemMessage"] != "checked" || specific["hookEventName"] != "UserPromptSubmit" || specific["additionalContext"] != "policy context" {
				t.Fatalf("native output = %#v", output)
			}
		})
	}
}

func TestUserPromptBlockUsesEachAgentDecision(t *testing.T) {
	for agent, payload := range map[string]string{
		"claude": `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/work","prompt":"stop"}`,
		"codex":  `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/work","prompt":"stop"}`,
		"pi":     `{"hook_event_name":"input","session_id":"s1","cwd":"/work","text":"stop"}`,
	} {
		t.Run(agent, func(t *testing.T) {
			runner := &recordingHookRunner{stdout: `{"decision":"block","reason":"denied"}`}
			result, err := hooks.DispatchOne(context.Background(), runner, agent, "UserPromptSubmit", []byte(payload), hooks.Handler{Command: "policy", Timeout: 600})
			if err != nil {
				t.Fatal(err)
			}
			var output map[string]any
			if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
				t.Fatal(err)
			}
			if output["decision"] != "block" || output["reason"] != "denied" {
				t.Fatalf("output = %#v", output)
			}
		})
	}
}

var toolBeforePayloads = map[string]string{
	"claude": `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/work","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"tool_use_id":"t1"}`,
	"codex":  `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/work","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"tool_use_id":"t1"}`,
	"pi":     `{"hook_event_name":"tool_call","session_id":"s1","cwd":"/work","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"tool_use_id":"t1"}`,
	"agy":    `{"hookEventName":"PreToolUse","conversationId":"s1","cwd":"/work","toolCall":{"id":"t1","name":"Bash","args":{"command":"rm -rf /"}}}`,
}

func TestToolBeforeInputIsCanonical(t *testing.T) {
	for agent, payload := range toolBeforePayloads {
		t.Run(agent, func(t *testing.T) {
			runner := &recordingHookRunner{}
			if _, err := hooks.DispatchOne(context.Background(), runner, agent, "PreToolUse", []byte(payload), hooks.Handler{Command: "gate", Timeout: 5}); err != nil {
				t.Fatal(err)
			}
			input := runner.singleInput(t)
			call, _ := input["tool_call"].(map[string]any)
			args, _ := call["args"].(map[string]any)
			if input["hook_event_name"] != "PreToolUse" || input["session_id"] != "s1" || call["id"] != "t1" || call["name"] != "Bash" || args["command"] != "rm -rf /" {
				t.Fatalf("canonical input = %#v", input)
			}
		})
	}
}

func TestToolBeforeDenyIsTranslatedToEachNativeFormat(t *testing.T) {
	hookOutputs := map[string]func() *recordingHookRunner{
		"json": func() *recordingHookRunner {
			return &recordingHookRunner{stdout: `{"decision":"block","reason":"no shell"}`}
		},
		"exit": func() *recordingHookRunner { return &recordingHookRunner{stderr: "no shell", exitCode: 2} },
	}
	for agent, payload := range toolBeforePayloads {
		for name, newRunner := range hookOutputs {
			t.Run(agent+"/"+name, func(t *testing.T) {
				result, err := hooks.DispatchOne(context.Background(), newRunner(), agent, "PreToolUse", []byte(payload), hooks.Handler{Command: "gate", Timeout: 5})
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("%+v %v", result, err)
				}
				var output map[string]any
				if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
					t.Fatalf("%q: %v", result.Stdout, err)
				}
				switch agent {
				case "agy":
					if output["decision"] != "deny" || output["reason"] != "no shell" {
						t.Fatalf("agy output = %#v", output)
					}
				case "pi":
					if output["decision"] != "block" || output["reason"] != "no shell" {
						t.Fatalf("pi output = %#v", output)
					}
				default:
					specific, _ := output["hookSpecificOutput"].(map[string]any)
					if specific["hookEventName"] != "PreToolUse" || specific["permissionDecision"] != "deny" || specific["permissionDecisionReason"] != "no shell" {
						t.Fatalf("%s output = %#v", agent, output)
					}
				}
			})
		}
	}
}

func TestToolBeforeAllowIsEmptyExceptAgy(t *testing.T) {
	for agent, payload := range toolBeforePayloads {
		t.Run(agent, func(t *testing.T) {
			result, err := hooks.DispatchOne(context.Background(), &recordingHookRunner{}, agent, "PreToolUse", []byte(payload), hooks.Handler{Command: "gate", Timeout: 5})
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if agent == "agy" {
				want = "{\"decision\":\"allow\"}\n"
			}
			if result.Stdout != want {
				t.Fatalf("stdout = %q, want %q", result.Stdout, want)
			}
		})
	}
}

func TestToolBeforeHookFailureIsNotAnImplicitDeny(t *testing.T) {
	for agent, payload := range toolBeforePayloads {
		t.Run(agent, func(t *testing.T) {
			runner := &recordingHookRunner{stderr: "boom", exitCode: 1}
			result, err := hooks.DispatchOne(context.Background(), runner, agent, "PreToolUse", []byte(payload), hooks.Handler{Command: "gate", Timeout: 5})
			if err == nil || result.ExitCode != 1 || result.Stdout != "" {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestGeminiHooksAreNotDispatched(t *testing.T) {
	for _, event := range []string{"SessionStart", "SessionEnd", "BeforeAgent", "AfterAgent"} {
		if _, err := hooks.Decode("gemini", event, []byte(`{}`)); err == nil {
			t.Fatalf("legacy event %s accepted", event)
		}
	}
	runner := &recordingHookRunner{}
	if _, err := hooks.DispatchOne(context.Background(), runner, "gemini", "UserPromptSubmit", []byte(`{}`), hooks.Handler{Command: "policy"}); err == nil || len(runner.inputs) != 0 {
		t.Fatalf("legacy dispatch error = %v, inputs = %+v", err, runner.inputs)
	}
}

type recordingHookRunner struct {
	mu       sync.Mutex
	inputs   []map[string]any
	stdout   string
	stderr   string
	exitCode int
}

func (r *recordingHookRunner) Execute(_ context.Context, _ runtime.CommandPlan, options runtime.ExecuteOptions) (runtime.CommandResult, error) {
	data, err := io.ReadAll(options.Stdin)
	if err != nil {
		return runtime.CommandResult{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		return runtime.CommandResult{}, err
	}
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	r.mu.Unlock()
	result := runtime.CommandResult{Stdout: r.stdout, Stderr: r.stderr, ExitCode: r.exitCode}
	if r.exitCode != 0 {
		return result, errors.New("exit status")
	}
	return result, nil
}

func (r *recordingHookRunner) singleInput(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inputs) != 1 {
		t.Fatalf("inputs = %#v", r.inputs)
	}
	return r.inputs[0]
}
