package agentparams

import "os"

func buildClaude(t Transport, r Request) (Plan, error) {
	if t != Interactive && t != ACP {
		return unsupported("claude", t, "transport")
	}
	if err := requireUUID("Claude resume", r.Session); err != nil {
		return Plan{}, err
	}
	if t == ACP {
		if r.UserConfig != nil && !*r.UserConfig {
			return unsupported("claude", t, "user_config=false (use --config-dir isolation)")
		}
		exe := os.Getenv("SM_CLAUDE_ACP_BIN")
		if exe == "" {
			exe = "claude-agent-acp"
		}
		mode := ""
		if r.Mode == ModeYolo {
			mode = "bypassPermissions"
		} else if r.Mode == ModeNormal {
			mode = "default"
		}
		params := map[string]any{"cwd": r.Cwd, "mcpServers": AgyMCPServers(r.MCP)}
		if r.Session != "" {
			params["sessionId"] = r.Session
		}
		return Plan{
			Executable:   exe,
			Args:         append([]string{}, r.Flags...),
			Params:       params,
			Model:        r.Model,
			Effort:       r.Effort,
			Mode:         mode,
			SystemPrompt: r.SystemPrompt,
		}, nil
	}
	args := []string{}
	if r.Session != "" {
		args = append(args, "--resume", r.Session)
	}
	if r.Mode == ModeYolo {
		args = append(args, "--dangerously-skip-permissions")
	}
	if r.UserConfig != nil && !*r.UserConfig {
		args = append(args, "--setting-sources", "project,local")
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	if r.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", r.SystemPrompt)
	}
	if len(r.MCP.Servers) > 0 {
		config, err := claudeMCP(r.MCP)
		if err != nil {
			return Plan{}, err
		}
		args = append(args, "--mcp-config", config)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, r.Flags...)
	return Plan{Args: args}, nil
}
