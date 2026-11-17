package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/hooks"
	"github.com/ArcheMind/agentx/internal/nativedirs"
	"github.com/ArcheMind/agentx/internal/runtime"
	"github.com/ArcheMind/agentx/internal/sessions"
)

// applyDirs activates S/C for one test and undoes the process env writes.
func applyDirs(t *testing.T, dirs nativedirs.Dirs) {
	t.Helper()
	for _, name := range nativedirs.IsolatedEnvNames() {
		value, ok := os.LookupEnv(name)
		t.Setenv(name, value)
		if !ok {
			os.Unsetenv(name)
		}
	}
	if err := nativedirs.ApplyToProcess(dirs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativedirs.ApplyToProcess(nativedirs.Dirs{}) })
}

// Defect A: a dry run plans the pi bridge extension without writing it.
func TestPiDryRunWritesNoBridge(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: home}
	application.Hooks = hooks.Service{Home: t.TempDir(), Project: project}
	if err := application.Run(context.Background(), []string{"pi", "--cwd", project, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sessionman-bridge.ts") {
		t.Fatalf("plan %q lacks the bridge extension", stdout.String())
	}
	if _, err := os.Lstat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote under home: %v", err)
	}
}

// Defect E: a dry run under S prepares nothing for the target agent either.
func TestDryRunUnderSessionDirPreparesNothing(t *testing.T) {
	s := t.TempDir()
	applyDirs(t, nativedirs.Dirs{SessionDir: s})
	project := t.TempDir()
	var stdout bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
	application.Sessions = sessions.Service{HomeDir: t.TempDir()}
	application.Hooks = hooks.Service{Home: t.TempDir(), Project: project}
	if err := application.Run(context.Background(), []string{"pi", "--cwd", project, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry run prepared %v (%v)", entries, err)
	}
}

// Defect H: an explicit --source for a blocked agent fails; the unfiltered
// catalog only warns.
func TestSessionListSourceBlockedFails(t *testing.T) {
	applyDirs(t, nativedirs.Dirs{SessionDir: t.TempDir()})
	var stdout, stderr bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &stderr)
	application.Sessions = sessions.Service{HomeDir: t.TempDir()}
	err := application.Run(context.Background(), []string{"session", "list", "--source", "opencode"})
	if err == nil || !strings.Contains(err.Error(), "opencode blocked") {
		t.Fatalf("err = %v, want opencode blocked", err)
	}
	stderr.Reset()
	if err := application.Run(context.Background(), []string{"session", "list"}); err != nil {
		t.Fatalf("unfiltered list err = %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: partial session catalog") {
		t.Fatalf("stderr = %q, want partial catalog warning", stderr.String())
	}
}

// Defect F: every help entry point shows the per-agent limitations.
func TestHelpShowsNativeDirLimitations(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"help", "session"}, {"session", "--help"}} {
		var stdout bytes.Buffer
		application := New(false, strings.NewReader(""), &stdout, &bytes.Buffer{})
		if err := application.Run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, want := range []string{"Per-agent limitations:", "opencode  does not support --session-dir", "CODEX_SQLITE_HOME"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%v output lacks %q", args, want)
			}
		}
	}
}

func treeSnapshot(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
			if err == nil {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}

// dryRunResume resumes a pi transcript into target as a dry run and returns
// stderr; cross-agent resume is the path that converts and writes a session.
func dryRunResume(t *testing.T, home, target string) string {
	t.Helper()
	project := t.TempDir()
	var stdout, stderr bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &stderr)
	application.Sessions = sessions.Service{HomeDir: home}
	application.Hooks = hooks.Service{Home: t.TempDir(), Project: project}
	application.Output = OutputJSON
	agent, err := application.Registry.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	detail := sessions.Session{ID: "src", Provider: "pi", Workspace: project, Messages: []sessions.Message{userMsg("u1", "hello"), assistantMsg("a1", "hi")}}
	if err := application.resumeSession(context.Background(), agent, detail, runtime.RunRequest{Cwd: project}, false, true); err != nil {
		t.Fatalf("dry-run resume %s: %v", target, err)
	}
	if stdout.Len() == 0 {
		t.Fatal("dry run printed no plan")
	}
	return stderr.String()
}

// Defect 1: a dry-run cross-agent resume reports the session path and writes
// nothing (no shadow projects dir that would block the native link).
func TestResumeDryRunClaudeConfigOnlyWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := t.TempDir()
	applyDirs(t, nativedirs.Dirs{ConfigDir: c})
	before := treeSnapshot(t, home, c)
	stderr := dryRunResume(t, home, "claude")
	if after := treeSnapshot(t, home, c); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("dry run wrote: before %v after %v", before, after)
	}
	shadowProjects := filepath.Join(home, ".local", "state", "sessionman", "shadow")
	if !strings.Contains(stderr, "would write resume session "+shadowProjects) || !strings.Contains(stderr, filepath.Join("claude", "projects")) {
		t.Fatalf("stderr = %q, want the planned claude session path", stderr)
	}
}

// Defect 1: a dry-run resume into codex on fresh S/C neither reads the Codex
// provider (which needs a prepared CODEX_HOME) nor creates anything.
func TestResumeDryRunCodexFreshSessionConfigWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	native := t.TempDir()
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", native)
	s, c := t.TempDir(), t.TempDir()
	applyDirs(t, nativedirs.Dirs{SessionDir: s, ConfigDir: c})
	before := treeSnapshot(t, home, native, s, c)
	stderr := dryRunResume(t, home, "codex")
	if after := treeSnapshot(t, home, native, s, c); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("dry run wrote: before %v after %v", before, after)
	}
	if !strings.Contains(stderr, "would write resume session "+filepath.Join(s, "codex", "sessions")) {
		t.Fatalf("stderr = %q, want the planned codex session path under S", stderr)
	}
}
