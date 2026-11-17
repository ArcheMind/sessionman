package nativedirs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetCache also snapshots every directory env var: ApplyToProcess writes
// them with os.Setenv, which t.Setenv cannot undo on its own.
func resetCache(t *testing.T) {
	for _, name := range IsolatedEnvNames() {
		value, ok := os.LookupEnv(name)
		t.Setenv(name, value)
		if !ok {
			os.Unsetenv(name)
		}
	}
	t.Cleanup(func() {
		resolvedMu.Lock()
		resolved, resolvedErrs, prepared = nil, nil, nil
		resolvedMu.Unlock()
	})
}

func codexAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// Every env var a resolution sets must come from the agent's table, so set,
// restore and unset are all derived from one place.
func TestResolvedEnvCoveredByTable(t *testing.T) {
	t.Setenv("CODEX_HOME", codexAuth(t))
	for _, dirs := range []Dirs{{SessionDir: "/s"}, {ConfigDir: "/c"}, {SessionDir: "/s", ConfigDir: "/c"}} {
		for _, agent := range KnownAgents {
			ad, err := Resolve(agent, dirs)
			if err != nil {
				continue
			}
			declared := map[string]bool{}
			for _, v := range AgentEnv(agent) {
				declared[v.Name] = true
			}
			for key := range ad.Env {
				if !declared[key] {
					t.Errorf("%s %+v sets %s, missing from its env table", agent, dirs, key)
				}
			}
		}
	}
}

// Defect D: a nested sm without S/C restores the user's value or unsets the
// var; it never writes a native default.
func TestNestedRestoreUnsetsWhenUserHadNone(t *testing.T) {
	resetCache(t)
	t.Setenv("CODEX_HOME", codexAuth(t))
	userCodex := os.Getenv("CODEX_HOME")
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_SQLITE_HOME", "PI_CODING_AGENT_DIR"} {
		unsetForTest(t, name)
	}
	s := t.TempDir()
	if err := ApplyToProcess(Dirs{SessionDir: s}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "CODEX_SQLITE_HOME", "PI_CODING_AGENT_DIR"} {
		if v := os.Getenv(name); !strings.HasPrefix(v, s+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a path under S", name, v)
		}
	}
	if _, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); !ok {
		t.Error("CLAUDE_SECURESTORAGE_CONFIG_DIR should be overridden under S")
	}

	if err := ApplyToProcess(Dirs{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_SQLITE_HOME", "PI_CODING_AGENT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s = %q after nested restore, want unset", name, v)
		}
		if v, ok := os.LookupEnv(nativePrefix + name); ok {
			t.Errorf("%s%s = %q after restore, want cleared", nativePrefix, name, v)
		}
	}
	if got := os.Getenv("CODEX_HOME"); got != userCodex {
		t.Errorf("CODEX_HOME = %q, want user value %q", got, userCodex)
	}
}

// Defect C: the native root is the user's own value, not the one sm set, and
// listing without S/C follows it too.
func TestNativeRootIgnoresSmOverride(t *testing.T) {
	resetCache(t)
	userCodex := codexAuth(t)
	t.Setenv("CODEX_HOME", userCodex)
	t.Setenv("SM_NATIVE_CODEX_HOME", "")
	os.Unsetenv("SM_NATIVE_CODEX_HOME")
	if got := SessionRoots("codex", "/home"); got[0] != filepath.Join(userCodex, "sessions") {
		t.Errorf("native SessionRoots(codex) = %v, want under CODEX_HOME", got)
	}
	if err := ApplyToProcess(Dirs{SessionDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CODEX_HOME") == userCodex {
		t.Fatal("CODEX_HOME should be overridden under S")
	}
	if got := NativeRoot("codex", ""); got != userCodex {
		t.Errorf("NativeRoot(codex) = %q, want user's %q", got, userCodex)
	}
}

func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err == nil && path != root {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// Defect E: resolution writes nothing; Ready prepares only the target agent.
func TestReadyPreparesOnlyTarget(t *testing.T) {
	resetCache(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexAuth(t))
	s, c := t.TempDir(), t.TempDir()
	if err := ApplyToProcess(Dirs{SessionDir: s, ConfigDir: c}); err != nil {
		t.Fatal(err)
	}
	if got := treeEntries(t, s); len(got) != 0 {
		t.Fatalf("ApplyToProcess wrote %v", got)
	}
	if got := treeEntries(t, home); len(got) != 0 {
		t.Fatalf("ApplyToProcess wrote %v", got)
	}
	if err := Ready("pi", false); err != nil {
		t.Fatal(err)
	}
	if got := treeEntries(t, s); len(got) != 0 {
		t.Fatalf("Ready without prepare wrote %v", got)
	}
	if err := Ready("pi", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s, "pi")); err != nil {
		t.Errorf("pi not prepared: %v", err)
	}
	for _, other := range []string{"codex", "claude", "dsh", "agy"} {
		if _, err := os.Lstat(filepath.Join(s, other)); err == nil {
			t.Errorf("preparing pi created %s", other)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "state", "sessionman")); err == nil {
		t.Error("preparing pi wrote sessionman state (agy shadow / dsh patch)")
	}
}
