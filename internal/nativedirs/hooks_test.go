package nativedirs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const toolHooks = `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"gate"}]}],"SessionStart":[{"hooks":[{"type":"command","command":"start"}]}]}}`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustPrepare(t *testing.T, agent string, dirs Dirs) *AgentDirs {
	t.Helper()
	ad, err := Resolve(agent, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if err := Prepare(ad); err != nil {
		t.Fatal(err)
	}
	return ad
}

func isLink(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()&os.ModeSymlink != 0
}

func TestCodexHooksJSONIsGeneratedFromC(t *testing.T) {
	t.Setenv("CODEX_HOME", codexAuth(t))
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	writeFile(t, filepath.Join(c, "codex", "config.toml"), "model = \"x\"\n")
	writeFile(t, filepath.Join(c, "codex", "hooks.json"), `{"hooks":{"PostToolUse":[]}}`)
	home := mustPrepare(t, "codex", Dirs{ConfigDir: c}).Env["CODEX_HOME"]

	if isLink(t, filepath.Join(home, "hooks.json")) {
		t.Fatal("hooks.json must be generated, not linked into C")
	}
	got := readFile(t, filepath.Join(home, "hooks.json"))
	if !strings.Contains(got, "hook __dispatch --agent codex --event PreToolUse") || strings.Contains(got, "PostToolUse") {
		t.Fatalf("hooks.json = %s", got)
	}
	if readFile(t, filepath.Join(c, "codex", "hooks.json")) != `{"hooks":{"PostToolUse":[]}}` {
		t.Fatal("C's own file was written through")
	}
	if !isLink(t, filepath.Join(home, "config.toml")) {
		t.Fatal("other entries stay links")
	}
}

func TestClaudeSettingsKeepCContentAndReplaceHooks(t *testing.T) {
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	original := `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"old-hook-cmd"}]}]}}`
	writeFile(t, filepath.Join(c, "claude", "settings.json"), original)
	home := mustPrepare(t, "claude", Dirs{ConfigDir: c}).Env["CLAUDE_CONFIG_DIR"]

	if isLink(t, filepath.Join(home, "settings.json")) {
		t.Fatal("settings.json must be generated")
	}
	got := readFile(t, filepath.Join(home, "settings.json"))
	if !strings.Contains(got, `"model": "opus"`) || strings.Contains(got, "old-hook-cmd") || !strings.Contains(got, "--agent claude --event PreToolUse") {
		t.Fatalf("settings.json = %s", got)
	}
	if readFile(t, filepath.Join(c, "claude", "settings.json")) != original {
		t.Fatal("C's settings.json was modified")
	}
}

func TestPiExtensionsDirHoldsUserEntriesAndGeneratedExtension(t *testing.T) {
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	writeFile(t, filepath.Join(c, "pi", "extensions", "mine.ts"), "export default () => {}\n")
	home := mustPrepare(t, "pi", Dirs{ConfigDir: c}).Env["PI_CODING_AGENT_DIR"]

	if isLink(t, filepath.Join(home, "extensions")) {
		t.Fatal("extensions must be a real directory")
	}
	if !isLink(t, filepath.Join(home, "extensions", "mine.ts")) {
		t.Fatal("user extension must stay linked")
	}
	got := readFile(t, filepath.Join(home, "extensions", "sessionman-hooks.ts"))
	if !strings.Contains(got, `pi.on("tool_call"`) {
		t.Fatalf("extension = %s", got)
	}
	if _, err := os.Lstat(filepath.Join(c, "pi", "extensions", "sessionman-hooks.ts")); err == nil {
		t.Fatal("generated extension leaked into C")
	}
}

func TestAgyPluginLivesInShadowGeminiRootAndNeverInNative(t *testing.T) {
	native := filepath.Join(os.Getenv("HOME"), ".gemini", "config")
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	ad := mustPrepare(t, "agy", Dirs{ConfigDir: c})

	plugin := filepath.Join(ad.ConfigDir, "plugins", "sessionman")
	if !strings.HasPrefix(plugin, filepath.Join(shadowDir(c, "agy"), ".gemini")) {
		t.Fatalf("plugin %s is outside the shadow gemini dir", plugin)
	}
	for _, name := range []string{"plugin.json", "hooks.json", "mcp_config.json"} {
		readFile(t, filepath.Join(plugin, name))
	}
	if !strings.Contains(readFile(t, filepath.Join(plugin, "hooks.json")), "--agent agy --event PreToolUse") ||
		!strings.Contains(readFile(t, filepath.Join(plugin, "mcp_config.json")), "__agy_serve") {
		t.Fatal("plugin lacks hooks or MCP registration")
	}
	if _, err := os.Lstat(native); err == nil {
		t.Fatalf("native %s was written", native)
	}
}

func TestAgyPluginWithoutHooksStillRegistersMCP(t *testing.T) {
	c := t.TempDir()
	ad := mustPrepare(t, "agy", Dirs{ConfigDir: c})
	plugin := filepath.Join(ad.ConfigDir, "plugins", "sessionman")
	readFile(t, filepath.Join(plugin, "mcp_config.json"))
	if _, err := os.Stat(filepath.Join(plugin, "hooks.json")); err == nil {
		t.Fatal("hooks.json written without hooks")
	}
}

func TestAgyWithoutCInjectsNothing(t *testing.T) {
	for _, dirs := range []Dirs{{}, {SessionDir: t.TempDir()}} {
		ad, err := Resolve("agy", dirs)
		if err != nil {
			t.Fatal(err)
		}
		if len(ad.PatchFiles) != 0 || len(ad.Generated) != 0 || ad.GenHome != "" {
			t.Fatalf("%+v injects without C: %+v", dirs, ad)
		}
	}
}

func TestHooksNeedCForEveryAgent(t *testing.T) {
	t.Setenv("CODEX_HOME", codexAuth(t))
	// hooks.json beside native config must not be consulted without C
	writeFile(t, filepath.Join(os.Getenv("HOME"), ".agents", "hooks.json"), toolHooks)
	for _, agent := range []string{"codex", "claude", "pi", "agy"} {
		ad, err := Resolve(agent, Dirs{SessionDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if len(ad.PatchFiles) != 0 || len(ad.Generated) != 0 {
			t.Fatalf("%s generated entries without C: %+v", agent, ad)
		}
	}
}

func TestNoHooksKeepsLinksAndRemovingHooksRestoresThem(t *testing.T) {
	t.Setenv("CODEX_HOME", codexAuth(t))
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "codex", "hooks.json"), `{"hooks":{}}`)
	home := mustPrepare(t, "codex", Dirs{ConfigDir: c}).Env["CODEX_HOME"]
	if !isLink(t, filepath.Join(home, "hooks.json")) {
		t.Fatal("no hooks defined: hooks.json must be a link")
	}

	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	mustPrepare(t, "codex", Dirs{ConfigDir: c})
	if isLink(t, filepath.Join(home, "hooks.json")) {
		t.Fatal("generated file expected")
	}

	if err := os.Remove(filepath.Join(c, "hooks.json")); err != nil {
		t.Fatal(err)
	}
	mustPrepare(t, "codex", Dirs{ConfigDir: c})
	if !isLink(t, filepath.Join(home, "hooks.json")) {
		t.Fatal("stale generated hooks.json was not replaced by the link")
	}
	if readFile(t, filepath.Join(c, "codex", "hooks.json")) != `{"hooks":{}}` {
		t.Fatal("C's file changed")
	}
}

func TestPiExtensionsReturnToLinkWhenHooksAreRemoved(t *testing.T) {
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), toolHooks)
	writeFile(t, filepath.Join(c, "pi", "extensions", "mine.ts"), "x")
	home := mustPrepare(t, "pi", Dirs{ConfigDir: c}).Env["PI_CODING_AGENT_DIR"]
	if err := os.Remove(filepath.Join(c, "hooks.json")); err != nil {
		t.Fatal(err)
	}
	mustPrepare(t, "pi", Dirs{ConfigDir: c})
	if !isLink(t, filepath.Join(home, "extensions")) {
		t.Fatal("extensions must link to C again")
	}
}

func TestInvalidHooksJSONFailsResolution(t *testing.T) {
	c := t.TempDir()
	writeFile(t, filepath.Join(c, "hooks.json"), `{`)
	if _, err := Resolve("claude", Dirs{ConfigDir: c}); err == nil {
		t.Fatal("invalid hooks.json accepted")
	}
}

func TestCodexHookTrustFlagFollowsMountedPortableHooks(t *testing.T) {
	const flag = "--dangerously-bypass-hook-trust"
	t.Setenv("CODEX_HOME", codexAuth(t))

	// With portable hooks in C (both C-only and S+C): flag is included.
	cWithHooks := t.TempDir()
	writeFile(t, filepath.Join(cWithHooks, "hooks.json"), toolHooks)
	for _, dirs := range []Dirs{{ConfigDir: cWithHooks}, {SessionDir: t.TempDir(), ConfigDir: cWithHooks}} {
		ad, err := Resolve("codex", dirs)
		if err != nil {
			t.Fatalf("Resolve(%+v): %v", dirs, err)
		}
		if !slices.Contains(ad.Args, flag) {
			t.Fatalf("Resolve(%+v).Args = %q, want %s", dirs, ad.Args, flag)
		}
	}

	// Empty C, empty hooks object, non-portable-only hooks, S-only, or native: flag is omitted.
	cEmpty := t.TempDir()
	cEmptyHooks := t.TempDir()
	writeFile(t, filepath.Join(cEmptyHooks, "hooks.json"), `{"hooks":{}}`)
	cNonPortable := t.TempDir()
	writeFile(t, filepath.Join(cNonPortable, "hooks.json"), `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"noop"}]}]}}`)
	for _, dirs := range []Dirs{
		{},
		{SessionDir: t.TempDir()},
		{ConfigDir: cEmpty},
		{SessionDir: t.TempDir(), ConfigDir: cEmpty},
		{ConfigDir: cEmptyHooks},
		{ConfigDir: cNonPortable},
	} {
		ad, err := Resolve("codex", dirs)
		if err != nil {
			t.Fatalf("Resolve(%+v): %v", dirs, err)
		}
		if slices.Contains(ad.Args, flag) {
			t.Fatalf("Resolve(%+v).Args = %q, should omit %s", dirs, ad.Args, flag)
		}
	}

	// Non-codex agents never receive Codex's hook-trust flag.
	for _, agent := range []string{"claude", "pi", "agy"} {
		ad, err := Resolve(agent, Dirs{ConfigDir: cWithHooks})
		if err != nil {
			t.Fatalf("Resolve(%s): %v", agent, err)
		}
		if slices.Contains(ad.Args, flag) {
			t.Fatalf("Resolve(%s).Args = %q, should not contain %s", agent, ad.Args, flag)
		}
	}
}
