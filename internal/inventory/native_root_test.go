package inventory

import (
	"os"
	"testing"

	"github.com/ArcheMind/agentx/internal/nativedirs"
)

// Defect C: under S the user directory is still the user's own CODEX_HOME,
// not the session-dir home sm exported.
func TestUserDirectoryIgnoresSmOverride(t *testing.T) {
	codexHome := t.TempDir()
	if err := os.WriteFile(codexHome+"/auth.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	for _, name := range nativedirs.IsolatedEnvNames() {
		value, ok := os.LookupEnv(name)
		t.Setenv(name, value)
		if !ok {
			os.Unsetenv(name)
		}
	}
	if err := nativedirs.ApplyToProcess(nativedirs.Dirs{SessionDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativedirs.ApplyToProcess(nativedirs.Dirs{}) })
	if os.Getenv("CODEX_HOME") == codexHome {
		t.Fatal("CODEX_HOME should be overridden under S")
	}
	if got := UserDirectory(t.TempDir(), "codex"); got != codexHome {
		t.Fatalf("UserDirectory(codex) = %q, want %q", got, codexHome)
	}
}
