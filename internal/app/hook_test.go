package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/hooks"
)

func TestHookListUsesPublicConfigurationAndStructuredOutput(t *testing.T) {
	configDir := t.TempDir()
	path := filepath.Join(configDir, "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"./hooks/check.sh"}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"gate.sh"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
	application.Hooks = hooks.Service{Home: t.TempDir(), ConfigDir: configDir}
	if err := application.Run(context.Background(), []string{"--json", "hook", "list"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"event": "UserPromptSubmit"`, `"command": "./hooks/check.sh"`, `"portable": true`, `"PreToolUse"`, `matcher has no portable subject`} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("output %q does not contain %q", stdout.String(), expected)
		}
	}
	if strings.Contains(stdout.String(), `"targets"`) {
		t.Fatalf("output contains redundant targets field: %q", stdout.String())
	}
}

func TestHookListTextShowsPortableHooksAndWarning(t *testing.T) {
	configDir := t.TempDir()
	path := filepath.Join(configDir, "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"load.sh"}]}],"PostToolUse":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
	application.Hooks = hooks.Service{Home: t.TempDir(), ConfigDir: configDir}
	if err := application.Run(context.Background(), []string{"hook", "list"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"SCOPE", "EVENT", "COMMAND", "SessionStart", "load.sh", "warning: PostToolUse"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("output %q does not contain %q", stdout.String(), expected)
		}
	}
	if strings.Contains(stdout.String(), "TARGETS") {
		t.Fatalf("output contains redundant targets column: %q", stdout.String())
	}
}

func TestHookListRejectsOtherGrammar(t *testing.T) {
	application := New(false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	for _, args := range [][]string{{"hook"}, {"hook", "show"}, {"hook", "list", "extra"}} {
		if err := application.Run(context.Background(), args); err == nil {
			t.Fatalf("args %v unexpectedly succeeded", args)
		}
	}
}

func TestHookListWithoutConfigDirIsEmpty(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"legacy"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
	application.Hooks = hooks.Service{Home: home}
	if err := application.Run(context.Background(), []string{"hook", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "No portable hooks configured.") || strings.Contains(stdout.String(), "legacy") {
		t.Fatalf("output = %q", stdout.String())
	}
}
