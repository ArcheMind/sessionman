package nativedirs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveNone(t *testing.T) {
	for _, agent := range []string{"pi", "codex", "claude", "opencode", "dsh", "agy"} {
		t.Run(agent, func(t *testing.T) {
			ad, err := Resolve(agent, Dirs{})
			if err != nil {
				t.Fatalf("Resolve(%s, empty): %v", agent, err)
			}
			if len(ad.Env) != 0 {
				t.Errorf("expected no env overrides, got %v", ad.Env)
			}
			if len(ad.Args) != 0 {
				t.Errorf("expected no extra args, got %v", ad.Args)
			}
			if len(ad.Symlinks) != 0 {
				t.Errorf("expected no symlinks, got %v", ad.Symlinks)
			}
			if len(ad.SessionRoots) == 0 {
				t.Error("expected at least one session root")
			}
			if ad.NativeHome == "" {
				t.Error("expected non-empty NativeHome")
			}
		})
	}
}

func TestNestedNativeRecovery(t *testing.T) {
	t.Setenv("SM_NATIVE_CODEX_HOME", "set:/original/codex")
	t.Setenv("CODEX_HOME", "/overridden/codex")
	if native := NativeRoot("codex", ""); native != "/original/codex" {
		t.Errorf("NativeRoot should prefer SM_NATIVE_CODEX_HOME, got %q", native)
	}
	t.Setenv("SM_NATIVE_CODEX_HOME", "unset")
	if native := NativeRoot("codex", "/home"); native != "/home/.codex" {
		t.Errorf("NativeRoot with an originally unset CODEX_HOME = %q, want default", native)
	}
}

func TestNativeEnvExport(t *testing.T) {
	t.Setenv("CODEX_HOME", "/user/codex")
	os.Unsetenv("CODEX_SQLITE_HOME")
	ad := &AgentDirs{
		Env:        map[string]string{"CODEX_HOME": "/new/codex", "CODEX_SQLITE_HOME": "/new/codex"},
		NativeHome: "/native/codex",
	}
	exports := NativeEnvExport(ad)
	if exports["SM_NATIVE_CODEX_HOME"] != "set:/user/codex" {
		t.Errorf("SM_NATIVE_CODEX_HOME = %q", exports["SM_NATIVE_CODEX_HOME"])
	}
	if exports["SM_NATIVE_CODEX_SQLITE_HOME"] != "unset" {
		t.Errorf("SM_NATIVE_CODEX_SQLITE_HOME = %q, want unset", exports["SM_NATIVE_CODEX_SQLITE_HOME"])
	}
}

func TestSymlinkCollision(t *testing.T) {
	tmp := t.TempDir()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink("/original/target", link); err != nil {
		t.Fatal(err)
	}
	err := Prepare(&AgentDirs{Symlinks: []Symlink{{link, "/different/target"}}})
	if err == nil {
		t.Error("expected collision error when re-pointing symlink")
	}
}

func TestSymlinkSameTarget(t *testing.T) {
	tmp := t.TempDir()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink("/same/target", link); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(&AgentDirs{Symlinks: []Symlink{{link, "/same/target"}}}); err != nil {
		t.Errorf("same-target symlink should not error: %v", err)
	}
}

func TestApplyToProcess(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	resetCache(t)
	t.Setenv("CODEX_HOME", tmp)
	t.Setenv("HOME", t.TempDir())

	sessDir := t.TempDir()
	t.Setenv("SM_SESSION_DIR", sessDir)
	t.Setenv("SM_CONFIG_DIR", "")

	if err := ApplyToProcess(FromEnv()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		resolvedMu.Lock()
		resolved = nil
		resolvedErrs = nil
		resolvedMu.Unlock()
	}()

	ccd := os.Getenv("CLAUDE_CONFIG_DIR")
	if ccd == "" {
		t.Error("CLAUDE_CONFIG_DIR should be set in process env after ApplyToProcess")
	}
	ch := os.Getenv("CODEX_HOME")
	if ch == tmp {
		t.Error("CODEX_HOME should be overridden by ApplyToProcess")
	}
	if Cached("claude") == nil {
		t.Error("Cached(claude) should be non-nil after ApplyToProcess")
	}
	if args := ResolvedArgs("agy"); args == nil {
		t.Error("ResolvedArgs(agy) should be non-nil when S is set")
	}
}

func TestGeminiDirFollowsConfig(t *testing.T) {
	resolvedMu.Lock()
	resolved = map[string]*AgentDirs{
		"agy": {ConfigDir: "/shadow/agy/.gemini/config"},
	}
	resolvedMu.Unlock()
	defer func() {
		resolvedMu.Lock()
		resolved = nil
		resolvedMu.Unlock()
	}()

	got := GeminiDir("/fallback")
	want := "/shadow/agy/.gemini"
	if got != want {
		t.Errorf("GeminiDir = %q, want %q", got, want)
	}
}

func TestGeminiDirFallback(t *testing.T) {
	resolvedMu.Lock()
	resolved = nil
	resolvedMu.Unlock()

	got := GeminiDir("/myhome")
	want := "/myhome/.gemini"
	if got != want {
		t.Errorf("GeminiDir = %q, want %q", got, want)
	}
}

func TestRestoreNativeEnvOnEmptyDirs(t *testing.T) {
	resetCache(t)
	t.Setenv("SM_NATIVE_CODEX_HOME", "set:/original/codex")
	t.Setenv("CODEX_HOME", "/shadow/codex")
	t.Setenv("SM_NATIVE_CLAUDE_CONFIG_DIR", "set:/original/claude")
	t.Setenv("CLAUDE_CONFIG_DIR", "/shadow/claude")
	t.Setenv("SM_SESSION_DIR", "")
	t.Setenv("SM_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())

	if err := ApplyToProcess(FromEnv()); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CODEX_HOME"); got != "/original/codex" {
		t.Errorf("CODEX_HOME = %q, want /original/codex", got)
	}
	if got := os.Getenv("CLAUDE_CONFIG_DIR"); got != "/original/claude" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want /original/claude", got)
	}
	if got := os.Getenv("SM_NATIVE_CODEX_HOME"); got != "" {
		t.Errorf("SM_NATIVE_CODEX_HOME should be cleared, got %q", got)
	}
}

func TestClaudeJSONAlwaysLinksToNative(t *testing.T) {
	ad, err := Resolve("claude", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range ad.Symlinks {
		if strings.HasSuffix(link.Link, ".claude.json") {
			if strings.Contains(link.Target, "/tmp/config") {
				t.Errorf(".claude.json links to C (%s); must always link to native", link.Target)
			}
			return
		}
	}
	t.Error("missing .claude.json symlink")
}

func TestCachedErrorOpenCode(t *testing.T) {
	resolvedMu.Lock()
	resolvedErrs = map[string]error{
		"opencode": fmt.Errorf("opencode does not support --config-dir"),
	}
	resolved = map[string]*AgentDirs{
		"opencode": {Env: map[string]string{}, SessionRoots: []string{}},
	}
	resolvedMu.Unlock()
	defer func() {
		resolvedMu.Lock()
		resolved = nil
		resolvedErrs = nil
		resolvedMu.Unlock()
	}()

	if err := CachedError("opencode"); err == nil {
		t.Error("expected cached error for opencode")
	}
	if err := CachedError("claude"); err != nil {
		t.Errorf("unexpected cached error for claude: %v", err)
	}
}
