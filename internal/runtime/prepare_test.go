package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func setupCodexNativeDirs(t *testing.T, hooksJSON string) {
	t.Helper()
	s := t.TempDir()
	c := t.TempDir()
	native := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hooksJSON != "" {
		if err := os.WriteFile(filepath.Join(c, "hooks.json"), []byte(hooksJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Dir(native))
	t.Setenv("CODEX_HOME", native)
	if err := nativedirs.ApplyToProcess(nativedirs.Dirs{SessionDir: s, ConfigDir: c}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nativedirs.ApplyToProcess(nativedirs.Dirs{}) })
}

func TestPrepareAgentCodexHookTrustFlag(t *testing.T) {
	const hookConfig = `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"gate"}]}]}}`

	t.Run("includes flag when portable hooks mounted", func(t *testing.T) {
		setupCodexNativeDirs(t, hookConfig)
		plan := CommandPlan{
			Executable: "codex",
			Args:       []string{"exec", "--json", "--skip-git-repo-check", "-"},
			Agent:      "codex",
		}
		if err := prepareAgent(&plan); err != nil {
			t.Fatal(err)
		}
		want := []string{"--dangerously-bypass-hook-trust", "exec", "--json", "--skip-git-repo-check", "-"}
		if !reflect.DeepEqual(plan.Args, want) {
			t.Fatalf("Args = %q, want %q", plan.Args, want)
		}
		// Calling prepareAgent again must not mutate cached ResolvedArgs.
		second := CommandPlan{Executable: "codex", Args: []string{"app-server", "--stdio"}, Agent: "codex"}
		if err := prepareAgent(&second); err != nil {
			t.Fatal(err)
		}
		wantSecond := []string{"--dangerously-bypass-hook-trust", "app-server", "--stdio"}
		if !reflect.DeepEqual(second.Args, wantSecond) {
			t.Fatalf("second Args = %q, want %q", second.Args, wantSecond)
		}
	})

	t.Run("omits flag when no hooks mounted", func(t *testing.T) {
		setupCodexNativeDirs(t, "")
		plan := CommandPlan{
			Executable: "codex",
			Args:       []string{"exec", "--json", "--skip-git-repo-check", "-"},
			Agent:      "codex",
		}
		if err := prepareAgent(&plan); err != nil {
			t.Fatal(err)
		}
		want := []string{"exec", "--json", "--skip-git-repo-check", "-"}
		if !reflect.DeepEqual(plan.Args, want) {
			t.Fatalf("Args = %q, want %q", plan.Args, want)
		}
	})

	t.Run("dedupes caller-supplied flag", func(t *testing.T) {
		setupCodexNativeDirs(t, hookConfig)
		cases := []struct {
			name string
			in   []string
			want []string
		}{
			{
				name: "after exec",
				in:   []string{"exec", "--json", "--dangerously-bypass-hook-trust", "-"},
				want: []string{"exec", "--json", "--dangerously-bypass-hook-trust", "-"},
			},
			{
				name: "leading",
				in:   []string{"--dangerously-bypass-hook-trust", "exec", "--json", "-"},
				want: []string{"--dangerously-bypass-hook-trust", "exec", "--json", "-"},
			},
			{
				name: "after terminator is positional",
				in:   []string{"exec", "--", "--dangerously-bypass-hook-trust"},
				want: []string{"--dangerously-bypass-hook-trust", "exec", "--", "--dangerously-bypass-hook-trust"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				plan := CommandPlan{Executable: "codex", Args: append([]string(nil), tc.in...), Agent: "codex"}
				if err := prepareAgent(&plan); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(plan.Args, tc.want) {
					t.Fatalf("Args = %q, want %q", plan.Args, tc.want)
				}
			})
		}
	})
}
