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

func TestResolvePiSessionOnly(t *testing.T) {
	ad, err := Resolve("pi", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	// S sets PI_CODING_AGENT_DIR to the shadow home at S/pi
	if ad.Env["PI_CODING_AGENT_DIR"] != "/tmp/sessions/pi" {
		t.Errorf("PI_CODING_AGENT_DIR = %q", ad.Env["PI_CODING_AGENT_DIR"])
	}
	if _, ok := ad.Env["PI_CODING_AGENT_SESSION_DIR"]; ok {
		t.Error("PI_CODING_AGENT_SESSION_DIR should not be set")
	}
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != "/tmp/sessions/pi/sessions" {
		t.Errorf("SessionRoots = %v", ad.SessionRoots)
	}
	// auth symlinks into shadow home
	found := 0
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "auth.json" || filepath.Base(link.Link) == "mcp-auth.json" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 auth symlinks, got %d", found)
	}
}

func TestResolvePiConfigOnly(t *testing.T) {
	ad, err := Resolve("pi", Dirs{ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	// C only: shadow home, PI_CODING_AGENT_DIR points there
	shadow := ad.Env["PI_CODING_AGENT_DIR"]
	if shadow == "" || shadow == "/tmp/config/pi" {
		t.Errorf("PI_CODING_AGENT_DIR should be a shadow home, got %q", shadow)
	}
	if _, ok := ad.Env["PI_CODING_AGENT_SESSION_DIR"]; ok {
		t.Error("PI_CODING_AGENT_SESSION_DIR should not be set")
	}
	if ad.ConfigDir != "/tmp/config/pi" {
		t.Errorf("ConfigDir = %q", ad.ConfigDir)
	}
	// sessions stay native
	native := ad.NativeHome
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != filepath.Join(native, "sessions") {
		t.Errorf("SessionRoots = %v", ad.SessionRoots)
	}
	// should have auth + config symlinks
	authFound := 0
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "auth.json" || filepath.Base(link.Link) == "mcp-auth.json" {
			authFound++
		}
	}
	if authFound != 2 {
		t.Errorf("expected 2 auth symlinks, got %d", authFound)
	}
}

func TestResolvePiBoth(t *testing.T) {
	ad, err := Resolve("pi", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	// S+C: shadow home at S/pi, config from C/pi
	if ad.Env["PI_CODING_AGENT_DIR"] != "/tmp/sessions/pi" {
		t.Errorf("PI_CODING_AGENT_DIR = %q", ad.Env["PI_CODING_AGENT_DIR"])
	}
	if _, ok := ad.Env["PI_CODING_AGENT_SESSION_DIR"]; ok {
		t.Error("PI_CODING_AGENT_SESSION_DIR should not be set")
	}
	// config entries should come from C
	configFromC := false
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "settings.json" && filepath.Dir(link.Target) == "/tmp/config/pi" {
			configFromC = true
		}
	}
	if !configFromC {
		t.Error("config entries should be symlinked from C/pi")
	}
}

func TestResolveCodexSessionOnly(t *testing.T) {
	// D6: S also moves CODEX_HOME, so auth.json at native is required
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", tmp)
	ad, err := Resolve("codex", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["CODEX_HOME"] != "/tmp/sessions/codex" {
		t.Errorf("CODEX_HOME = %q", ad.Env["CODEX_HOME"])
	}
	if ad.Env["CODEX_SQLITE_HOME"] != "/tmp/sessions/codex" {
		t.Errorf("CODEX_SQLITE_HOME = %q", ad.Env["CODEX_SQLITE_HOME"])
	}
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != "/tmp/sessions/codex/sessions" {
		t.Errorf("SessionRoots = %v", ad.SessionRoots)
	}
	// auth.json should be symlinked to native
	hasAuth := false
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "auth.json" {
			hasAuth = true
			if link.Target != filepath.Join(ad.NativeHome, "auth.json") {
				t.Errorf("auth.json target = %q, want under native", link.Target)
			}
		}
	}
	if !hasAuth {
		t.Error("missing auth.json symlink")
	}
}

func TestResolveCodexConfigOnlyKeychainError(t *testing.T) {
	// C-only with no auth.json at native should fail
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	_, err := Resolve("codex", Dirs{ConfigDir: "/tmp/config"})
	if err == nil {
		t.Error("expected keychain error for C-only without auth.json")
	}
}

func TestResolveCodexConfigOnlyWithAuth(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", tmp)
	ad, err := Resolve("codex", Dirs{ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	// CODEX_HOME should be a shadow dir
	if ad.Env["CODEX_HOME"] == tmp {
		t.Error("CODEX_HOME should be a shadow, not the native dir")
	}
	// CODEX_SQLITE_HOME should point to native
	if ad.Env["CODEX_SQLITE_HOME"] != tmp {
		t.Errorf("CODEX_SQLITE_HOME = %q, want %q (native)", ad.Env["CODEX_SQLITE_HOME"], tmp)
	}
	if ad.Limitation == "" {
		t.Error("expected limitation note for C-only codex")
	}
}

func TestResolveCodexBoth(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", tmp)
	ad, err := Resolve("codex", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["CODEX_HOME"] != "/tmp/sessions/codex" {
		t.Errorf("CODEX_HOME = %q", ad.Env["CODEX_HOME"])
	}
	// config entries should point to config dir
	configTarget := false
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "config.toml" && filepath.Dir(link.Target) == "/tmp/config/codex" {
			configTarget = true
		}
	}
	if !configTarget {
		t.Error("config.toml should be symlinked to config dir")
	}
}

func TestResolveClaudeSessionOnly(t *testing.T) {
	ad, err := Resolve("claude", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["CLAUDE_CONFIG_DIR"] != "/tmp/sessions/claude" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q", ad.Env["CLAUDE_CONFIG_DIR"])
	}
	if ad.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != "" {
		t.Errorf("CLAUDE_SECURESTORAGE_CONFIG_DIR should be empty string, got %q", ad.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"])
	}
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != "/tmp/sessions/claude/projects" {
		t.Errorf("SessionRoots = %v", ad.SessionRoots)
	}
}

func TestResolveClaudeConfigOnly(t *testing.T) {
	ad, err := Resolve("claude", Dirs{ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	// should be a shadow
	shadow := ad.Env["CLAUDE_CONFIG_DIR"]
	if shadow == "" {
		t.Fatal("CLAUDE_CONFIG_DIR not set")
	}
	// projects/ should be symlinked to native
	hasProjects := false
	for _, link := range ad.Symlinks {
		if filepath.Base(link.Link) == "projects" {
			hasProjects = true
		}
	}
	if !hasProjects {
		t.Error("missing projects/ symlink to native")
	}
}

func TestResolveOpenCodeConfigError(t *testing.T) {
	_, err := Resolve("opencode", Dirs{ConfigDir: "/tmp/config"})
	if err == nil {
		t.Error("expected error for opencode with --config-dir")
	}
}

func TestResolveOpenCodeSessionOnly(t *testing.T) {
	ad, err := Resolve("opencode", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["OPENCODE_DB"] != "/tmp/sessions/opencode/opencode.db" {
		t.Errorf("OPENCODE_DB = %q", ad.Env["OPENCODE_DB"])
	}
}

func TestResolveDSHBoth(t *testing.T) {
	ad, err := Resolve("dsh", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.Args) < 2 || ad.Args[0] != "--patch" {
		t.Errorf("expected --patch arg, got %v", ad.Args)
	}
	if len(ad.PatchFiles) != 1 {
		t.Errorf("expected 1 patch file, got %d", len(ad.PatchFiles))
	}
}

func TestResolveAgyBoth(t *testing.T) {
	ad, err := Resolve("agy", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	hasGeminiDir := false
	hasAppDataDir := false
	for _, arg := range ad.Args {
		if len(arg) > 13 && arg[:13] == "--gemini_dir=" {
			hasGeminiDir = true
			if arg[13:] != "/tmp/config/agy" {
				t.Errorf("--gemini_dir = %q", arg[13:])
			}
		}
		if len(arg) > 15 && arg[:15] == "--app_data_dir=" {
			hasAppDataDir = true
		}
	}
	if !hasGeminiDir {
		t.Error("missing --gemini_dir")
	}
	if !hasAppDataDir {
		t.Error("missing --app_data_dir")
	}
}

func TestResolveAgySessionOnly(t *testing.T) {
	ad, err := Resolve("agy", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	// should NOT have --gemini_dir, but should have --app_data_dir relative to native gemini
	hasGeminiDir := false
	hasAppDataDir := false
	for _, arg := range ad.Args {
		if len(arg) > 13 && arg[:13] == "--gemini_dir=" {
			hasGeminiDir = true
		}
		if len(arg) > 15 && arg[:15] == "--app_data_dir=" {
			hasAppDataDir = true
		}
	}
	if hasGeminiDir {
		t.Error("S-only should not set --gemini_dir")
	}
	if !hasAppDataDir {
		t.Error("missing --app_data_dir for S-only")
	}
}

func TestNestedNativeRecovery(t *testing.T) {
	t.Setenv("SM_NATIVE_CODEX_HOME", "/original/codex")
	t.Setenv("CODEX_HOME", "/overridden/codex")
	native := nativeHome("CODEX_HOME", ".codex")
	if native != "/original/codex" {
		t.Errorf("nativeHome should prefer SM_NATIVE_CODEX_HOME, got %q", native)
	}
}

func TestRelativeDirError(t *testing.T) {
	_, err := Resolve("pi", Dirs{SessionDir: "relative/path"})
	if err == nil {
		t.Error("expected error for relative session dir")
	}
}

func TestNativeEnvExport(t *testing.T) {
	ad := &AgentDirs{
		Env:        map[string]string{"CODEX_HOME": "/new/codex"},
		NativeHome: "/native/codex",
	}
	exports := NativeEnvExport(ad)
	if exports["SM_NATIVE_CODEX_HOME"] != "/native/codex" {
		t.Errorf("SM_NATIVE_CODEX_HOME = %q", exports["SM_NATIVE_CODEX_HOME"])
	}
}

func TestResolveCodexSessionKeychainError(t *testing.T) {
	// D6: S also requires file-based auth
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	_, err := Resolve("codex", Dirs{SessionDir: "/tmp/sessions"})
	if err == nil {
		t.Error("expected keychain error for S without auth.json")
	}
}

func TestResolveDSHPatchFormat(t *testing.T) {
	// D4: patch YAML must be a top-level array of loader entries
	ad, err := Resolve("dsh", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.PatchFiles) != 1 {
		t.Fatalf("expected 1 patch file, got %d", len(ad.PatchFiles))
	}
	content := string(ad.PatchFiles[0].Content)
	if content[0] != '-' {
		t.Errorf("patch file must start with a YAML array entry '- id:', got %q", content[:20])
	}
	if !strings.Contains(content, "- id: session-persistence-jsonl") || !strings.Contains(content, "- id: storage-json") {
		t.Errorf("patch file missing expected entries:\n%s", content)
	}
}

func TestResolveAgyConfigOnly(t *testing.T) {
	// D7: C-only must pass --app_data_dir pinned to native
	ad, err := Resolve("agy", Dirs{ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	hasGeminiDir := false
	hasAppDataDir := false
	for _, arg := range ad.Args {
		if len(arg) > 13 && arg[:13] == "--gemini_dir=" {
			hasGeminiDir = true
		}
		if len(arg) > 15 && arg[:15] == "--app_data_dir=" {
			hasAppDataDir = true
		}
	}
	if !hasGeminiDir {
		t.Error("C-only should set --gemini_dir")
	}
	if !hasAppDataDir {
		t.Error("C-only should set --app_data_dir to pin sessions to native")
	}
}

func TestResolveOpenCodeSessionRoots(t *testing.T) {
	// D8: SessionRoots must follow S
	ad, err := Resolve("opencode", Dirs{SessionDir: "/tmp/sessions"})
	if err != nil {
		t.Fatal(err)
	}
	want := "/tmp/sessions/opencode"
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != want {
		t.Errorf("SessionRoots = %v, want [%s]", ad.SessionRoots, want)
	}
}

func TestSymlinkCollision(t *testing.T) {
	// D9a: refuse to re-point existing symlink to a different target
	tmp := t.TempDir()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink("/original/target", link); err != nil {
		t.Fatal(err)
	}
	err := EnsureSymlinks([]Symlink{{link, "/different/target"}})
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
	if err := EnsureSymlinks([]Symlink{{link, "/same/target"}}); err != nil {
		t.Errorf("same-target symlink should not error: %v", err)
	}
}

func TestApplyToProcess(t *testing.T) {
	// Need a codex auth.json for the resolver
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", tmp)

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

	// claude's CLAUDE_CONFIG_DIR should now be set in process env
	ccd := os.Getenv("CLAUDE_CONFIG_DIR")
	if ccd == "" {
		t.Error("CLAUDE_CONFIG_DIR should be set in process env after ApplyToProcess")
	}
	// codex's CODEX_HOME should be overridden
	ch := os.Getenv("CODEX_HOME")
	if ch == tmp {
		t.Error("CODEX_HOME should be overridden by ApplyToProcess")
	}
	// Cached should return the resolution
	if Cached("claude") == nil {
		t.Error("Cached(claude) should be non-nil after ApplyToProcess")
	}
	if args := ResolvedArgs("agy"); args == nil {
		t.Error("ResolvedArgs(agy) should be non-nil when S is set")
	}
}

func TestGeminiDirFollowsConfig(t *testing.T) {
	// Set up cached resolution with C
	resolvedMu.Lock()
	resolved = map[string]*AgentDirs{
		"agy": {ConfigDir: "/custom/config/agy/config"},
	}
	resolvedMu.Unlock()
	defer func() {
		resolvedMu.Lock()
		resolved = nil
		resolvedMu.Unlock()
	}()

	got := GeminiDir("/fallback")
	want := "/custom/config/agy"
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

