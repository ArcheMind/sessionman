package drivers

import (
	"fmt"
	"sort"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

type Registry struct {
	agents map[string]runtime.Agent
}

func NewRegistry() Registry {
	piModels := PiModels{Plan: runtime.CommandPlan{Executable: "pi", Args: []string{"--list-models"}, Agent: "pi"}}
	agents := []runtime.Agent{
		agyAgent(),
		nativeAgent("claude", "Claude Code", "claude", "@anthropic-ai/claude-code", ClaudeModels{}, true, NativeAuth{Command: runtime.CommandPlan{Executable: "claude", Args: []string{"auth", "login", "--claudeai"}, Agent: "claude"}, LogoutCommand: runtime.CommandPlan{Executable: "claude", Args: []string{"auth", "logout"}, Agent: "claude"}, StatusCommand: runtime.CommandPlan{Executable: "claude", Args: []string{"auth", "status", "--json"}, Agent: "claude"}, ParseStatus: parseClaudeAuthStatus}, ClaudeUsage{}),
		nativeAgent("codex", "Codex CLI", "codex", "@openai/codex", CodexAppServerModels{}, true, NativeAuth{Command: runtime.CommandPlan{Executable: "codex", Args: []string{"login"}, Agent: "codex"}, LogoutCommand: runtime.CommandPlan{Executable: "codex", Args: []string{"logout"}, Agent: "codex"}, StatusCommand: runtime.CommandPlan{Executable: "codex", Args: []string{"login", "status"}, Agent: "codex"}, ParseStatus: parseCodexAuthStatus}, CodexUsage{}),
		{ID: "dsh", Name: "DeepSeek Harness", Binary: "dsh", Launch: NativeLaunch{Binary: "dsh"}, Models: DSHModels{}, Auth: ProviderAuth{NativeAuth: NativeAuth{Command: runtime.CommandPlan{Executable: "dsh", Args: []string{"web"}, Agent: "dsh"}, Instruction: "Open the DSH Models page and configure the provider credential there."}, Providers: DSHAuthProviders{}}, Install: NPMPackage{Package: "@deepseek-ai/dsh"}, Capabilities: []runtime.Capability{runtime.CapabilityLaunch, runtime.CapabilityAuthLogin, runtime.CapabilityAuthStatus, runtime.CapabilityModelList}},
		nativeAgent("opencode", "OpenCode", "opencode", "opencode-ai", CommandModels{Plan: runtime.CommandPlan{Executable: "opencode", Args: []string{"models"}, Agent: "opencode"}}, true, NativeAuth{Command: runtime.CommandPlan{Executable: "opencode", Args: []string{"auth", "login"}, Agent: "opencode"}, LogoutCommand: runtime.CommandPlan{Executable: "opencode", Args: []string{"auth", "logout"}, Agent: "opencode"}, StatusCommand: runtime.CommandPlan{Executable: "opencode", Args: []string{"auth", "list"}, Agent: "opencode"}, ParseStatus: parseOpenCodeAuthStatus}, nil),
		nativeAgent("pi", "Pi Coding Agent", "pi", "@earendil-works/pi-coding-agent", piModels, true, PiSDKAuth{ProviderAuth: ProviderAuth{NativeAuth: NativeAuth{LogoutCommand: runtime.CommandPlan{Executable: "pi", Agent: "pi"}, LogoutInstruction: "Run /logout in Pi and select the provider."}}, Models: piModels}, nil),
	}
	items := make(map[string]runtime.Agent, len(agents))
	for _, agent := range agents {
		items[agent.ID] = agent
	}
	return Registry{agents: items}
}

func nativeAgent(id, name, binary, npmPackage string, models runtime.ModelDriver, listsModels bool, auth runtime.AuthDriver, usage runtime.UsageDriver) runtime.Agent {
	capabilities := []runtime.Capability{
		runtime.CapabilityLaunch,
		runtime.CapabilityAuthLogin,
		runtime.CapabilityModelSelect,
	}
	if listsModels {
		capabilities = append(capabilities, runtime.CapabilityModelList)
	}
	if auth.SupportsStatus() {
		capabilities = append(capabilities, runtime.CapabilityAuthStatus)
	}
	if auth.SupportsLogout() {
		capabilities = append(capabilities, runtime.CapabilityAuthLogout)
	}
	if usage != nil {
		capabilities = append(capabilities, runtime.CapabilityUsage)
	}
	return runtime.Agent{
		ID: id, Name: name, Binary: binary,
		Launch:       NativeLaunch{Binary: binary},
		Models:       models,
		Auth:         auth,
		Usage:        usage,
		Install:      NPMPackage{Package: npmPackage},
		Capabilities: capabilities,
	}
}

func (r Registry) Get(id string) (runtime.Agent, error) {
	agent, ok := r.agents[id]
	if !ok {
		return runtime.Agent{}, fmt.Errorf("unknown agent %q", id)
	}
	return agent, nil
}

func (r Registry) All() []runtime.Agent {
	items := make([]runtime.Agent, 0, len(r.agents))
	for _, agent := range r.agents {
		items = append(items, agent)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}
