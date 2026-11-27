package agentparams

func buildClaude(t Transport, r Request) (Plan, error) {
	if t != Interactive && t != Stream {
		return unsupported("claude", t, "transport")
	}
	if err := requireUUID("Claude resume", r.Session); err != nil {
		return Plan{}, err
	}
	args := []string{}
	if t == Stream {
		args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	}
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
		config, err := ClaudeMCPJSON(r.MCP)
		if err != nil {
			return Plan{}, err
		}
		args = append(args, "--mcp-config", config)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, r.Flags...)
	if t == Stream {
		if _, ok := r.MCP.Servers["sessionman"]; ok {
			args = append(args, "--allowedTools", "mcp__sessionman__StartExternalSubagent", "mcp__sessionman__SendExternalSubagentMessage", "mcp__sessionman__StopExternalSubagent", "mcp__sessionman__ListExternalSubagentSessions", "mcp__sessionman__MessageHost")
		}
		return Plan{Executable: "claude", Args: args, Session: r.Session}, nil
	}
	return Plan{Args: args}, nil
}
