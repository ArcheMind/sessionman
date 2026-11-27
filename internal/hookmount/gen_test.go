package hookmount_test

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/hookmount"
)

func loadFixture(t *testing.T) []hookmount.Hook {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "hooks.json"), `{"hooks":{
  "SessionStart":[{"hooks":[{"type":"command","command":"start"}]}],
  "SessionEnd":[{"hooks":[{"type":"command","command":"end"}]}],
  "UserPromptSubmit":[{"hooks":[{"type":"command","command":"prompt"}]}],
  "PreToolUse":[{"hooks":[{"type":"command","command":"gate","timeout":7}]}],
  "Stop":[{"hooks":[{"type":"command","command":"stop"}]}]}}`)
	result, err := hookmount.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return result.Hooks
}

func handlerAfter(t *testing.T, content, agent, event string) hookmount.Handler {
	t.Helper()
	marker := "--agent " + agent + " --event " + event + " --handler "
	index := strings.Index(content, marker)
	if index < 0 {
		t.Fatalf("no %q in:\n%s", marker, content)
	}
	suffix := content[index+len(marker):]
	end := strings.IndexAny(suffix, "\"'\\ ")
	if end < 0 {
		end = len(suffix)
	}
	data, err := base64.RawURLEncoding.DecodeString(suffix[:end])
	if err != nil {
		t.Fatal(err)
	}
	var handler hookmount.Handler
	if err := json.Unmarshal(data, &handler); err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestCodexHooksMapAllEventsThroughBridge(t *testing.T) {
	data, err := hookmount.CodexHooks("/opt/sm/bin/sm", loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string
				Command string
				Timeout int
			}
		}
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "Stop"} {
		if len(root.Hooks[event]) != 1 {
			t.Fatalf("%s groups = %#v", event, root.Hooks)
		}
		handlerAfter(t, root.Hooks[event][0].Hooks[0].Command, "codex", event)
	}
	gate := root.Hooks["PreToolUse"][0].Hooks[0]
	if gate.Timeout != 7 || handlerAfter(t, gate.Command, "codex", "PreToolUse").Command != "gate" || !strings.Contains(gate.Command, "'/opt/sm/bin/sm' hook __dispatch") {
		t.Fatalf("gate = %#v", gate)
	}
}

func TestClaudeSettingsReplacesOnlyHooksKey(t *testing.T) {
	base := []byte(`{"model":"opus","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"native"}]}]},"permissions":{"allow":["Bash"]}}`)
	data, err := hookmount.ClaudeSettings("/opt/sm/bin/sm", base, loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["model"]) != `"opus"` || !strings.Contains(string(root["permissions"]), "Bash") {
		t.Fatalf("other keys changed: %s", data)
	}
	hooks := string(root["hooks"])
	if strings.Contains(hooks, "native") {
		t.Fatalf("native hooks kept: %s", hooks)
	}
	handlerAfter(t, hooks, "claude", "PreToolUse")
	handlerAfter(t, hooks, "claude", "UserPromptSubmit")

	data, err = hookmount.ClaudeSettings("/opt/sm/bin/sm", nil, loadFixture(t))
	if err != nil || !strings.Contains(string(data), `"hooks"`) {
		t.Fatalf("%s %v", data, err)
	}
	if _, err := hookmount.ClaudeSettings("/sm", []byte(`[]`), nil); err == nil {
		t.Fatal("non-object settings accepted")
	}
}

func TestPiExtensionHandlesToolCallBlock(t *testing.T) {
	source, err := hookmount.PiExtension("/opt/sm/bin/sm", loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`pi.on("tool_call"`, `{ block: true, reason:`, `invoke("PreToolUse"`, `pi.on("input"`, `pi.on("agent_settled"`, `pi.on("session_shutdown"`, `"/opt/sm/bin/sm"`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("extension lacks %q", expected)
		}
	}
	marker := `"PreToolUse":[{"payload":"`
	index := strings.Index(source, marker)
	if index < 0 {
		t.Fatal("PreToolUse handler payload not embedded")
	}
	rest := source[index+len(marker):]
	data, err := base64.RawURLEncoding.DecodeString(rest[:strings.Index(rest, `"`)])
	if err != nil || !strings.Contains(string(data), `"command":"gate"`) {
		t.Fatalf("payload = %s %v", data, err)
	}
}

func TestAgyPluginCarriesMCPAndSupportedHooksOnly(t *testing.T) {
	files, err := hookmount.AgyPlugin("/opt/sm/bin/sm", loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plugin.json", "mcp_config.json", "hooks.json"} {
		if len(files[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	if !strings.Contains(string(files["mcp_config.json"]), `"__agy_serve"`) {
		t.Fatalf("mcp_config.json = %s", files["mcp_config.json"])
	}
	var root map[string]map[string]json.RawMessage
	if err := json.Unmarshal(files["hooks.json"], &root); err != nil {
		t.Fatal(err)
	}
	events := root["sessionman"]
	if len(events) != 3 || events["SessionStart"] == nil || events["Stop"] == nil || events["PreToolUse"] == nil {
		t.Fatalf("events = %s", files["hooks.json"])
	}
	if !strings.Contains(string(events["PreToolUse"]), `"matcher": "*"`) {
		t.Fatalf("PreToolUse must be grouped: %s", events["PreToolUse"])
	}
	if strings.Contains(string(events["SessionStart"]), "matcher") {
		t.Fatalf("SessionStart must be flat: %s", events["SessionStart"])
	}

	files, err = hookmount.AgyPlugin("/opt/sm/bin/sm", nil)
	if err != nil || files["hooks.json"] != nil || files["mcp_config.json"] == nil {
		t.Fatalf("no-hook plugin = %v %v", files, err)
	}
}

func TestHasAgentHooks(t *testing.T) {
	if hookmount.HasAgentHooks("codex", nil) {
		t.Fatal("nil hooks should not match codex")
	}
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "hooks.json"), `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"prompt"}]}]}}`)
	result, err := hookmount.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hookmount.HasAgentHooks("codex", result.Hooks) {
		t.Fatal("UserPromptSubmit should match codex")
	}
	if hookmount.HasAgentHooks("agy", result.Hooks) {
		t.Fatal("UserPromptSubmit should not match agy")
	}
}
