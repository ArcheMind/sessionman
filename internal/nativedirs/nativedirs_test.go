package nativedirs

import (
	"os"
	"path/filepath"
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
	if ad.Env["PI_CODING_AGENT_SESSION_DIR"] != "/tmp/sessions/pi" {
		t.Errorf("PI_CODING_AGENT_SESSION_DIR = %q", ad.Env["PI_CODING_AGENT_SESSION_DIR"])
	}
	if _, ok := ad.Env["PI_CODING_AGENT_DIR"]; ok {
		t.Error("PI_CODING_AGENT_DIR should not be set for S-only")
	}
	if len(ad.SessionRoots) != 1 || ad.SessionRoots[0] != "/tmp/sessions/pi" {
		t.Errorf("SessionRoots = %v", ad.SessionRoots)
	}
}

func TestResolvePiConfigOnly(t *testing.T) {
	ad, err := Resolve("pi", Dirs{ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["PI_CODING_AGENT_DIR"] != "/tmp/config/pi" {
		t.Errorf("PI_CODING_AGENT_DIR = %q", ad.Env["PI_CODING_AGENT_DIR"])
	}
	// sessions must be pinned to native
	native := ad.NativeHome
	expected := filepath.Join(native, "sessions")
	if ad.Env["PI_CODING_AGENT_SESSION_DIR"] != expected {
		t.Errorf("PI_CODING_AGENT_SESSION_DIR = %q, want %q", ad.Env["PI_CODING_AGENT_SESSION_DIR"], expected)
	}
	if ad.ConfigDir != "/tmp/config/pi" {
		t.Errorf("ConfigDir = %q", ad.ConfigDir)
	}
	// should have auth symlinks
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

func TestResolvePiBoth(t *testing.T) {
	ad, err := Resolve("pi", Dirs{SessionDir: "/tmp/sessions", ConfigDir: "/tmp/config"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Env["PI_CODING_AGENT_DIR"] != "/tmp/config/pi" {
		t.Errorf("PI_CODING_AGENT_DIR = %q", ad.Env["PI_CODING_AGENT_DIR"])
	}
	if ad.Env["PI_CODING_AGENT_SESSION_DIR"] != "/tmp/sessions/pi" {
		t.Errorf("PI_CODING_AGENT_SESSION_DIR = %q", ad.Env["PI_CODING_AGENT_SESSION_DIR"])
	}
}

func TestResolveCodexSessionOnly(t *testing.T) {
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
