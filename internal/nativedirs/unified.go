package nativedirs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type unifiedMCPServer struct {
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"envPassthrough,omitempty"`
}

type unifiedMCPConfig struct {
	Servers map[string]unifiedMCPServer `json:"mcpServers"`
}

// mountUnifiedSkills composes <home>/skills with symlinks from:
//  1. <configSource>/skills (agent-specific in C, highest priority)
//  2. <configDir>/skills    (unified skills in C)
//  3. ~/.agents/skills      (shared user skills)
//
// If neither <configDir>/skills nor ~/.agents/skills contains any entries,
// <home>/skills remains a direct symlink to <configSource>/skills.
func mountUnifiedSkills(ad *AgentDirs, configDir, skillsParent, configSource, relGenerated string) error {
	if configDir == "" {
		return nil
	}
	sharedRoots := []string{
		filepath.Join(configDir, "skills"),
		filepath.Join(userHome(), ".agents", "skills"),
	}
	hasShared := false
	for _, root := range sharedRoots {
		entries, err := readSkillEntries(root)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			hasShared = true
			break
		}
	}
	if !hasShared {
		return nil
	}

	sources := append([]string{filepath.Join(configSource, "skills")}, sharedRoots...)
	merged := map[string]string{}
	for _, root := range sources {
		entries, err := readSkillEntries(root)
		if err != nil {
			return err
		}
		for _, name := range entries {
			if _, exists := merged[name]; !exists {
				merged[name] = filepath.Join(root, name)
			}
		}
	}

	ad.unlink(skillsParent, "skills")
	ad.Generated = append(ad.Generated, relGenerated)
	skillsDir := filepath.Join(skillsParent, "skills")
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ad.Symlinks = append(ad.Symlinks, Symlink{
			Link:   filepath.Join(skillsDir, name),
			Target: merged[name],
		})
	}
	return nil
}

func readSkillEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skills directory %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// mountUnifiedMCP reads <configDir>/mcp_config.json (if present) and projects
// its servers into the agent's isolated/shadow root without touching native files.
func mountUnifiedMCP(ad *AgentDirs, agent, configDir, home, configSource string) error {
	if configDir == "" {
		return nil
	}
	unified, ok, err := readMCPConfigFile(filepath.Join(configDir, "mcp_config.json"))
	if err != nil {
		return err
	}
	if !ok || len(unified.Servers) == 0 {
		return nil
	}

	switch agent {
	case "claude":
		raw, err := claudeUnifiedMCPJSON(unified)
		if err != nil {
			return fmt.Errorf("render claude unified MCP config: %w", err)
		}
		const fileName = "sessionman-mcp.json"
		ad.generate(home, fileName, []byte(raw))
		ad.Args = append(ad.Args, "--mcp-config", filepath.Join(home, fileName))
	case "codex":
		content, err := mergeCodexMCP(filepath.Join(configSource, "config.toml"), unified)
		if err != nil {
			return err
		}
		ad.unlink(home, "config.toml")
		ad.generate(home, "config.toml", content)
	case "agy":
		content, err := mergeAgyMCP(filepath.Join(configSource, "mcp_config.json"), unified)
		if err != nil {
			return err
		}
		ad.unlink(home, "mcp_config.json")
		ad.Generated = append(ad.Generated, "config/mcp_config.json")
		ad.PatchFiles = append(ad.PatchFiles, PatchFile{filepath.Join(home, "mcp_config.json"), content})
	}
	return nil
}

func readMCPConfigFile(path string) (unifiedMCPConfig, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return unifiedMCPConfig{}, false, nil
	}
	if err != nil {
		return unifiedMCPConfig{}, false, fmt.Errorf("read MCP config %s: %w", path, err)
	}
	var cfg unifiedMCPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return unifiedMCPConfig{}, false, fmt.Errorf("parse MCP config %s: %w", path, err)
	}
	for name, server := range cfg.Servers {
		if name == "" || server.Command == "" {
			return unifiedMCPConfig{}, false, fmt.Errorf("MCP config %s: server %q requires a name and command", path, name)
		}
	}
	return cfg, true, nil
}

func claudeUnifiedMCPJSON(m unifiedMCPConfig) ([]byte, error) {
	servers := map[string]any{}
	for name, s := range m.Servers {
		entry := map[string]any{"type": "stdio", "command": s.Command}
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
		servers[name] = entry
	}
	return json.Marshal(map[string]any{"mcpServers": servers})
}

func mergeCodexMCP(agentConfigPath string, unified unifiedMCPConfig) ([]byte, error) {
	root := map[string]any{}
	data, err := os.ReadFile(agentConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read codex config.toml %s: %w", agentConfigPath, err)
	}
	if len(data) > 0 {
		if err := toml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse codex config.toml %s: %w", agentConfigPath, err)
		}
	}
	servers := map[string]any{}
	if existing, ok := root["mcp_servers"].(map[string]any); ok {
		for k, v := range existing {
			servers[k] = v
		}
	}
	for name, s := range unified.Servers {
		if _, exists := servers[name]; exists {
			continue
		}
		entry := map[string]any{"command": s.Command}
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
		if len(s.EnvPassthrough) > 0 {
			entry["env_vars"] = s.EnvPassthrough
		}
		servers[name] = entry
	}
	root["mcp_servers"] = servers
	out, err := toml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode codex config.toml: %w", err)
	}
	return out, nil
}

func mergeAgyMCP(agentConfigPath string, unified unifiedMCPConfig) ([]byte, error) {
	root := map[string]any{}
	data, err := os.ReadFile(agentConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read agy mcp_config.json %s: %w", agentConfigPath, err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse agy mcp_config.json %s: %w", agentConfigPath, err)
		}
	}
	servers := map[string]any{}
	if existing, ok := root["mcpServers"].(map[string]any); ok {
		for k, v := range existing {
			servers[k] = v
		}
	}
	for name, s := range unified.Servers {
		if _, exists := servers[name]; exists {
			continue
		}
		entry := map[string]any{"command": s.Command}
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
		if len(s.EnvPassthrough) > 0 {
			entry["envPassthrough"] = s.EnvPassthrough
		}
		servers[name] = entry
	}
	root["mcpServers"] = servers
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode agy mcp_config.json: %w", err)
	}
	return out, nil
}
