package subagents

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/presets"
)

func TestDaemonLaunchPlanUnifiedConfigTakeover(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SM_CONFIG_DIR", "")
	t.Setenv("SM_SESSION_DIR", "")

	// Shared user skill in ~/.agents/skills/chrome-use
	skillDir := filepath.Join(home, ".agents", "skills", "chrome-use")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: chrome-use\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		Home: home,
		Dir:  t.TempDir(),
		agent: &ExternalSubagentState{
			Cwd: home,
			Option: presets.Preset{
				Name:        "claude-unified",
				Agent:       "claude",
				Model:       "opus",
				Description: "test",
			},
		},
	}

	req, err := d.parameterRequest()
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := agentparams.Build("claude", agentparams.Stream, req)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := d.launchPlan(req, mapped)
	if err != nil {
		t.Fatal(err)
	}

	wantC := filepath.Join(home, ".sessionman", "config")
	if plan.Env["SM_CONFIG_DIR"] != wantC {
		t.Fatalf("SM_CONFIG_DIR = %q, want %q", plan.Env["SM_CONFIG_DIR"], wantC)
	}
	claudeDir := plan.Env["CLAUDE_CONFIG_DIR"]
	if claudeDir == "" {
		t.Fatal("CLAUDE_CONFIG_DIR was not set")
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "chrome-use", "SKILL.md")); err != nil {
		t.Fatalf("subagent shadow dir missing unified chrome-use skill: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills")); err == nil {
		t.Fatal("native ~/.claude/skills was unexpectedly created")
	}

	// When UnifiedConfig is explicitly false, subagent does not take over SM_CONFIG_DIR.
	disabled := false
	d.agent.Option.UnifiedConfig = &disabled
	planNoTakeover, _, err := d.launchPlan(req, mapped)
	if err != nil {
		t.Fatal(err)
	}
	if planNoTakeover.Env["SM_CONFIG_DIR"] != "" || planNoTakeover.Env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("unexpected takeover env when UnifiedConfig is false: %v", planNoTakeover.Env)
	}
}
