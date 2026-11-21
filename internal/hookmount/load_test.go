package hookmount_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/hookmount"
)

func TestLoadReadsOnlyUserConfigDir(t *testing.T) {
	configDir := t.TempDir()
	writeConfig(t, filepath.Join(configDir, "hooks.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"user-start"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"gate","timeout":5}]}]}}`)

	result, err := hookmount.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hooks) != 2 || len(result.Warnings) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if result.Hooks[0].Scope != hookmount.ScopeUser || result.Hooks[0].Event != "SessionStart" || result.Hooks[0].Command != "user-start" {
		t.Fatalf("hook = %#v", result.Hooks[0])
	}
	if result.Hooks[1].Event != "PreToolUse" || result.Hooks[1].Handler().Timeout != 5 || !result.Hooks[1].Portable {
		t.Fatalf("hook = %#v", result.Hooks[1])
	}
}

func TestLoadWithoutConfigDirIgnoresLegacyLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, filepath.Join(home, ".agents", "hooks.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"legacy"}]}]}}`)
	result, err := hookmount.Load("")
	if err != nil || len(result.Hooks) != 0 {
		t.Fatalf("result = %#v, %v", result, err)
	}
	result, err = hookmount.Load(t.TempDir())
	if err != nil || len(result.Hooks) != 0 {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestLoadWarnsAndSkipsOnlyNonPortableItems(t *testing.T) {
	configDir := t.TempDir()
	writeConfig(t, filepath.Join(configDir, "hooks.json"), `{
  "hooks": {
    "SessionStart": [{"hooks": [
      {"type":"command","command":"portable"},
      {"type":"command","command":"background","async":true},
      {"type":"prompt","prompt":"check it"}
    ]}],
    "SessionEnd": [{"matcher":"(?=exit)","hooks":[{"type":"command","command":"complex"}]}],
    "PreToolUse": [{"matcher":"Bash","hooks":[{"type":"command","command":"gate"}]}],
    "PostToolUse": [{"hooks":[{"type":"command","command":"native-only"}]}]
  }
}`)

	result, err := hookmount.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hooks) != 2 || result.Hooks[0].Command != "portable" || result.Hooks[1].Command != "gate" {
		t.Fatalf("hooks = %#v", result.Hooks)
	}
	joined := warningText(result.Warnings)
	for _, expected := range []string{"PostToolUse", "async", "handler type prompt", "matcher"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("warnings %q do not contain %q", joined, expected)
		}
	}
	if !strings.Contains(joined, "PreToolUse matcher has no portable subject") {
		t.Fatalf("PreToolUse matcher not warned: %q", joined)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{"json", `{`},
		{"missing-hooks", `{}`},
		{"empty-command", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":""}]}]}}`},
		{"timeout", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"report","timeout":0}]}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			configDir := t.TempDir()
			writeConfig(t, filepath.Join(configDir, "hooks.json"), test.content)
			if _, err := hookmount.Load(configDir); err == nil {
				t.Fatal("expected invalid configuration to fail")
			}
		})
	}
}

func TestSessionEndTimeoutAboveThreeSecondsIsNotPortable(t *testing.T) {
	configDir := t.TempDir()
	writeConfig(t, filepath.Join(configDir, "hooks.json"), `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"slow-cleanup","timeout":4},{"type":"command","command":"fast-cleanup","timeout":3}]}]}}`)
	result, err := hookmount.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hooks) != 1 || result.Hooks[0].Command != "fast-cleanup" {
		t.Fatalf("hooks = %#v", result.Hooks)
	}
	if !strings.Contains(warningText(result.Warnings), "above 3 seconds") {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func warningText(warnings []hookmount.Warning) string {
	parts := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		parts = append(parts, warning.Event+" "+warning.Message)
	}
	return strings.Join(parts, "\n")
}
