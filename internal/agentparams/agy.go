package agentparams

func buildAgy(t Transport, r Request) (Plan, error) {
	if t != Interactive && t != ACP && t != Exec {
		return unsupported("agy", t, "transport")
	}
	if t != ACP {
		if err := requireUUID("agy conversation", r.Session); err != nil {
			return Plan{}, err
		}
	}
	if t == ACP {
		args := []string{"acp", "--stdio"}
		if r.Mode == ModeYolo {
			args = append(args, "--dangerously-skip-permissions")
		}
		if r.UserConfig != nil && !*r.UserConfig {
			args = append(args, "--include-default-customizations=false")
		}
		args = append(args, r.Flags...)
		params := map[string]any{"cwd": r.Cwd, "mcpServers": AgyMCPServers(r.MCP)}
		if r.Session != "" {
			params["sessionId"] = r.Session
		}
		return Plan{
			Executable:   "agy",
			Args:         args,
			Params:       params,
			Model:        r.Model,
			Effort:       r.Effort,
			SystemPrompt: r.SystemPrompt,
		}, nil
	}
	if len(r.MCP.Servers) > 0 {
		return unsupported("agy", t, "temporary MCP mount")
	}
	if r.SystemPrompt != "" {
		return unsupported("agy", t, "system prompt")
	}
	if r.UserConfig != nil && !*r.UserConfig {
		return unsupported("agy", t, "user_config")
	}
	args := []string{}
	if t == Exec {
		args = append(args, "--input-format", "stream-json", "--output-format", "stream-json")
	}
	if r.Session != "" {
		args = append(args, "--conversation", r.Session)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	if r.Mode == ModeYolo {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, r.Flags...)
	return Plan{Args: args}, nil
}
