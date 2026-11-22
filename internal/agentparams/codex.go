package agentparams

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func buildCodex(t Transport, r Request) (Plan, error) {
	if t != Interactive && t != ACP {
		return unsupported("codex", t, "transport")
	}
	if err := requireUUID("Codex resume", r.Session); err != nil {
		return Plan{}, err
	}
	if r.UserConfig != nil && !*r.UserConfig {
		return unsupported("codex", t, "user_config=false")
	}
	if t == ACP {
		exe := os.Getenv("SM_CODEX_ACP_BIN")
		if exe == "" {
			exe = "codex-acp"
		}
		env := map[string]string{}
		if os.Getenv("CODEX_PATH") == "" {
			if codexBin, err := exec.LookPath("codex"); err == nil {
				env["CODEX_PATH"] = codexBin
			}
		}
		mode := ""
		if r.Mode == ModeYolo {
			mode = "agent-full-access"
			env["INITIAL_AGENT_MODE"] = mode
		} else if r.Mode == ModeNormal {
			mode = "workspace-write"
			env["INITIAL_AGENT_MODE"] = mode
		}
		params := map[string]any{"cwd": r.Cwd, "mcpServers": AgyMCPServers(r.MCP)}
		if r.Session != "" {
			params["sessionId"] = r.Session
		}
		return Plan{
			Executable:   exe,
			Args:         append([]string{}, r.Flags...),
			Env:          env,
			Params:       params,
			Model:        r.Model,
			Effort:       r.Effort,
			Mode:         mode,
			SystemPrompt: r.SystemPrompt,
		}, nil
	}
	args := []string{}
	if r.Session != "" {
		args = append(args, "resume", r.Session)
	}
	if r.Mode == ModeYolo {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if r.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", r.Effort))
	}
	if r.SystemPrompt != "" {
		args = append(args, "-c", fmt.Sprintf("developer_instructions=%q", r.SystemPrompt))
	}
	mcp, err := codexMCPArgs(r.MCP)
	if err != nil {
		return Plan{}, err
	}
	args = append(args, mcp...)
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, r.Flags...)
	return Plan{Args: args}, nil
}

// Config discovery must use the same native configuration selection as execution.
func CodexConfigArgs(r Request) ([]string, error) {
	if r.UserConfig != nil && !*r.UserConfig {
		return nil, fmt.Errorf("Codex app-server config discovery cannot honor user_config=false")
	}
	var args []string
	for i := 0; i < len(r.Flags); i++ {
		flag := r.Flags[i]
		if flag == "--" {
			break
		}
		switch {
		case flag == "-p" || flag == "--profile" || strings.HasPrefix(flag, "--profile=") || strings.HasPrefix(flag, "-p="):
			return nil, fmt.Errorf("Codex app-server config discovery cannot honor --profile")
		case flag == "--ignore-user-config":
			return nil, fmt.Errorf("Codex app-server config discovery cannot honor --ignore-user-config")
		case flag == "-c" || flag == "--config":
			if i+1 >= len(r.Flags) {
				return nil, fmt.Errorf("%s requires a value", flag)
			}
			args = append(args, flag, r.Flags[i+1])
			i++
		case strings.HasPrefix(flag, "--config="):
			args = append(args, flag)
		}
	}
	return args, nil
}
