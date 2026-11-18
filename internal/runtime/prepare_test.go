package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ArcheMind/agentx/internal/nativedirs"
)

func TestPrepareAgentCalledWhenAgentSet(t *testing.T) {
	s := t.TempDir()
	c := t.TempDir()
	native := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(native))
	if err := nativedirs.ApplyToProcess(nativedirs.Dirs{SessionDir: s, ConfigDir: c}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativedirs.ApplyToProcess(nativedirs.Dirs{}) })

	skillsLink := filepath.Join(s, "codex", "skills")
	if _, err := os.Lstat(skillsLink); err == nil {
		t.Fatal("symlink exists before Execute")
	}

	runner := ExecRunner{}
	_, _ = runner.Execute(context.Background(), CommandPlan{
		Executable: "sh", Args: []string{"-c", "true"}, Agent: "codex",
	}, ExecuteOptions{})

	info, err := os.Lstat(skillsLink)
	if err != nil {
		t.Fatalf("symlink not created by Execute with Agent: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at %s, got mode %v", skillsLink, info.Mode())
	}
}

func TestPrepareAgentSkippedWhenAgentEmpty(t *testing.T) {
	s := t.TempDir()
	c := t.TempDir()
	native := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(native))
	if err := nativedirs.ApplyToProcess(nativedirs.Dirs{SessionDir: s, ConfigDir: c}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativedirs.ApplyToProcess(nativedirs.Dirs{}) })

	runner := ExecRunner{}
	_, _ = runner.Execute(context.Background(), CommandPlan{
		Executable: "sh", Args: []string{"-c", "true"},
	}, ExecuteOptions{})

	skillsLink := filepath.Join(s, "codex", "skills")
	if _, err := os.Lstat(skillsLink); err == nil {
		t.Fatal("symlink should not exist after Execute without Agent")
	}
}
