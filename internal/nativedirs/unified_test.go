package nativedirs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestUnifiedSkillsLinkedForAllAgentsWithoutNativePollution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Codex requires native auth.json when SM_CONFIG_DIR is set.
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), `{}`)
	// Shared user skill in ~/.agents/skills
	writeFile(t, filepath.Join(home, ".agents", "skills", "chrome-use", "SKILL.md"), "---\nname: chrome-use\n---\n")

	c := t.TempDir()
	writeFile(t, filepath.Join(c, "skills", "shared-skill", "SKILL.md"), "---\nname: shared-skill\n---\n")
	writeFile(t, filepath.Join(c, "claude", "skills", "chrome-use", "SKILL.md"), "---\nname: chrome-use\n---\n")
	writeFile(t, filepath.Join(c, "claude", "skills", "agent-only", "SKILL.md"), "---\nname: agent-only\n---\n")

	for _, agent := range []string{"claude", "codex", "pi", "agy"} {
		t.Run(agent, func(t *testing.T) {
			ad := mustPrepare(t, agent, Dirs{ConfigDir: c})
			skillsDir := filepath.Join(shadowDir(c, agent), "skills")
			if agent == "agy" {
				skillsDir = filepath.Join(ad.ConfigDir, "skills")
			}
			if isLink(t, skillsDir) {
				t.Fatalf("%s skills dir must be a composed directory, not a symlink", agent)
			}
			if _, err := os.Stat(filepath.Join(skillsDir, "shared-skill", "SKILL.md")); err != nil {
				t.Fatalf("%s missing shared-skill: %v", agent, err)
			}
			chromeLink := filepath.Join(skillsDir, "chrome-use")
			target, err := os.Readlink(chromeLink)
			if err != nil {
				t.Fatalf("%s missing chrome-use symlink: %v", agent, err)
			}
			if agent == "claude" {
				want := filepath.Join(c, "claude", "skills", "chrome-use")
				if target != want {
					t.Fatalf("claude chrome-use = %q, want agent override %q", target, want)
				}
				if _, err := os.Stat(filepath.Join(skillsDir, "agent-only", "SKILL.md")); err != nil {
					t.Fatalf("claude missing agent-only: %v", err)
				}
			} else {
				want := filepath.Join(home, ".agents", "skills", "chrome-use")
				if target != want {
					t.Fatalf("%s chrome-use = %q, want %q", agent, target, want)
				}
			}
		})
	}

	// Native config/skill directories must remain unpolluted.
	for _, native := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".pi", "agent", "skills"),
		filepath.Join(home, ".gemini", "config"),
		filepath.Join(home, ".codex", "skills"),
	} {
		if _, err := os.Lstat(native); err == nil {
			t.Fatalf("native path %s was unexpectedly created", native)
		}
	}
}

func TestUnifiedMCPMountedForSupportedAgentsWithoutNativePollution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".codex", "auth.json"), `{}`)

	c := t.TempDir()
	writeFile(t, filepath.Join(c, "mcp_config.json"), `{
  "mcpServers": {
    "unified-tool": {"command": "npx", "args": ["-y", "unified-mcp"]},
    "shared": {"command": "unified-cmd"}
  }
}`)
	writeFile(t, filepath.Join(c, "codex", "config.toml"), `
model = "gpt-5"
[mcp_servers.shared]
command = "codex-override"
`)
	writeFile(t, filepath.Join(c, "agy", "config", "mcp_config.json"), `{
  "mcpServers": {
    "shared": {"command": "agy-override"}
  }
}`)

	// Claude: generates sessionman-mcp.json and passes --mcp-config without touching ~/.claude.json
	claudeAD := mustPrepare(t, "claude", Dirs{ConfigDir: c})
	mcpFile := filepath.Join(shadowDir(c, "claude"), "sessionman-mcp.json")
	if !slices.Contains(claudeAD.Args, "--mcp-config") || !slices.Contains(claudeAD.Args, mcpFile) {
		t.Fatalf("claude Args = %v, want --mcp-config %s", claudeAD.Args, mcpFile)
	}
	if !strings.Contains(readFile(t, mcpFile), `"unified-tool"`) {
		t.Fatalf("claude sessionman-mcp.json missing unified-tool")
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude.json")); err == nil {
		t.Fatal("native .claude.json was created")
	}

	// Codex: merges mcp_servers into shadow config.toml with agent override preserved
	mustPrepare(t, "codex", Dirs{ConfigDir: c})
	var codexCfg map[string]any
	if err := toml.Unmarshal([]byte(readFile(t, filepath.Join(shadowDir(c, "codex"), "config.toml"))), &codexCfg); err != nil {
		t.Fatal(err)
	}
	servers := codexCfg["mcp_servers"].(map[string]any)
	if servers["unified-tool"].(map[string]any)["command"] != "npx" {
		t.Fatalf("codex unified-tool = %v", servers["unified-tool"])
	}
	if servers["shared"].(map[string]any)["command"] != "codex-override" {
		t.Fatalf("codex shared = %v, want codex-override", servers["shared"])
	}

	// Agy: merges mcpServers into shadow .gemini/config/mcp_config.json with agent override preserved
	agyAD := mustPrepare(t, "agy", Dirs{ConfigDir: c})
	var agyCfg struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(agyAD.ConfigDir, "mcp_config.json"))), &agyCfg); err != nil {
		t.Fatal(err)
	}
	if agyCfg.MCPServers["unified-tool"].Command != "npx" {
		t.Fatalf("agy unified-tool = %+v", agyCfg.MCPServers["unified-tool"])
	}
	if agyCfg.MCPServers["shared"].Command != "agy-override" {
		t.Fatalf("agy shared = %+v, want agy-override", agyCfg.MCPServers["shared"])
	}
}
