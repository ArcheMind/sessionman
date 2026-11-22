package agentparams

import (
	"reflect"
	"testing"
)

const testUUID = "11111111-2222-4333-8444-555555555555"

func TestResolveUsesExplicitOverridesWithoutInventingDefaults(t *testing.T) {
	falseValue := false
	base := Request{Model: "base", Effort: "low", Mode: ModeNormal, Flags: []string{"--verbose"}, UserConfig: &falseValue, MCP: MCPConfig{Servers: map[string]MCPServer{"one": {Command: "node"}}}}
	override := Request{Model: "override", Flags: []string{}, MCP: MCPConfig{Servers: map[string]MCPServer{}}}
	got, err := Resolve("claude", base, override)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "override" || got.Effort != "low" || got.Mode != ModeNormal || got.UserConfig == nil || *got.UserConfig || got.Flags == nil || len(got.Flags) != 0 || got.MCP.Servers == nil || len(got.MCP.Servers) != 0 {
		t.Fatalf("resolved %#v", got)
	}
	empty, err := Resolve("codex", Request{}, Request{})
	if err != nil || empty.Mode != "" {
		t.Fatalf("defaults injected: %#v %v", empty, err)
	}
}

func TestTransportBehavior(t *testing.T) {
	cases := []struct {
		agent string
		t     Transport
		r     Request
		want  []string
	}{
		{"claude", Interactive, Request{Model: "opus", Mode: ModeYolo, Session: testUUID}, []string{"--resume", testUUID, "--dangerously-skip-permissions", "--model", "opus"}},
		{"codex", Interactive, Request{Model: "gpt-6", Mode: ModeYolo, Session: testUUID}, []string{"resume", testUUID, "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6"}},
		{"pi", RPC, Request{Model: "native", Effort: "low", Mode: ModeYolo}, []string{"--mode", "rpc", "--no-approve", "--thinking", "low", "--model", "native"}},
		{"pi", Exec, Request{Model: "native"}, []string{"--print", "--mode", "json", "--model", "native"}},
		{"agy", Exec, Request{Model: "gemini", Session: testUUID}, []string{"--input-format", "stream-json", "--output-format", "stream-json", "--conversation", testUUID, "--model", "gemini"}},
		{"opencode", Interactive, Request{Session: "token"}, []string{"--session", "token"}},
	}
	for _, tc := range cases {
		t.Run(tc.agent+"/"+string(tc.t), func(t *testing.T) {
			got, err := Build(tc.agent, tc.t, tc.r)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Args, tc.want) {
				t.Fatalf("args %q, want %q", got.Args, tc.want)
			}
		})
	}
}

func TestACPTransportsCarryUnifiedFields(t *testing.T) {
	mcp := MCPConfig{Servers: map[string]MCPServer{"bridge": {Command: "node", Args: []string{"bridge.mjs"}, Env: map[string]string{"A": "1"}}}}
	agy, err := Build("agy", ACP, Request{Cwd: "/ws", Model: "gemini", Effort: "high", Mode: ModeYolo, Session: "native", SystemPrompt: "role", MCP: mcp})
	if err != nil {
		t.Fatal(err)
	}
	if agy.Executable != "agy" || !reflect.DeepEqual(agy.Args, []string{"acp", "--stdio", "--dangerously-skip-permissions"}) || agy.Params["sessionId"] != "native" || agy.Model != "gemini" || agy.Effort != "high" || agy.SystemPrompt != "role" {
		t.Fatalf("agy ACP %#v", agy)
	}
	claude, err := Build("claude", ACP, Request{Cwd: "/ws", Model: "opus", Effort: "high", Mode: ModeYolo, Session: testUUID, SystemPrompt: "role", MCP: mcp})
	if err != nil {
		t.Fatal(err)
	}
	if claude.Executable != "claude-agent-acp" || claude.Params["sessionId"] != testUUID || claude.Model != "opus" || claude.Effort != "high" || claude.Mode != "bypassPermissions" || claude.SystemPrompt != "role" {
		t.Fatalf("claude ACP %#v", claude)
	}
	codex, err := Build("codex", ACP, Request{Cwd: "/ws", Model: "gpt-6", Effort: "high", Mode: ModeYolo, Session: testUUID, SystemPrompt: "role", MCP: mcp, Flags: []string{"--verbose"}})
	if err != nil {
		t.Fatal(err)
	}
	if codex.Executable != "codex-acp" || !reflect.DeepEqual(codex.Args, []string{"--verbose"}) || codex.Params["sessionId"] != testUUID || codex.Model != "gpt-6" || codex.Effort != "high" || codex.Mode != "agent-full-access" || codex.Env["INITIAL_AGENT_MODE"] != "agent-full-access" || codex.SystemPrompt != "role" {
		t.Fatalf("codex ACP %#v", codex)
	}
}

func TestRejectsUnsupportedAndConflictingParameters(t *testing.T) {
	cases := []struct {
		agent string
		t     Transport
		r     Request
	}{
		{"codex", Interactive, Request{UserConfig: boolPtr(false)}},
		{"codex", ACP, Request{UserConfig: boolPtr(false)}},
		{"claude", ACP, Request{UserConfig: boolPtr(false)}},
		{"pi", RPC, Request{MCP: MCPConfig{Servers: map[string]MCPServer{"x": {Command: "node"}}}}},
		{"agy", Exec, Request{SystemPrompt: "role"}},
		{"opencode", Interactive, Request{Mode: ModeYolo}},
		{"dsh", Interactive, Request{Model: "model"}},
		{"claude", Interactive, Request{Model: "opus", Flags: []string{"--model=sonnet"}}},
		{"codex", Interactive, Request{Effort: "high", Flags: []string{"--config=model_reasoning_effort=low"}}},
		{"claude", Interactive, Request{Session: "bad"}},
		{"claude", ACP, Request{Session: "bad"}},
		{"codex", ACP, Request{Session: "bad"}},
		{"claude", Interactive, Request{Effort: "ultra"}},
	}
	for _, tc := range cases {
		t.Run(tc.agent+"/"+string(tc.t)+"/"+tc.r.Model+tc.r.Effort, func(t *testing.T) {
			if _, err := Build(tc.agent, tc.t, tc.r); err == nil {
				t.Fatalf("accepted %#v", tc.r)
			}
		})
	}
}

func TestFlagsTreatValuesAsValuesAndHonorTerminator(t *testing.T) {
	cases := [][]string{
		{"--prompt", "--model", "opus"},
		{"--", "--model", "opus"},
		{"--config", "mcp_servers.other.command='node'"},
	}
	for _, flags := range cases {
		if _, err := Build("codex", Interactive, Request{Model: "gpt-6", MCP: MCPConfig{Servers: map[string]MCPServer{"mine": {Command: "node"}}}, Flags: flags}); err != nil {
			t.Fatalf("flags %q: %v", flags, err)
		}
	}
	conflicts := []struct {
		r     Request
		flags []string
	}{
		{Request{Model: "gpt-6"}, []string{"-c", ` "model" = "other"`}},
		{Request{Mode: ModeNormal}, []string{"--config=approval_policy='never'"}},
		{Request{Mode: ModeYolo}, []string{"--sandbox", "read-only"}},
		{Request{MCP: MCPConfig{Servers: map[string]MCPServer{"mine": {Command: "node"}}}}, []string{"-c", `mcp_servers.mine.command='other'`}},
	}
	for _, tc := range conflicts {
		tc.r.Flags = tc.flags
		if _, err := Build("codex", Interactive, tc.r); err == nil {
			t.Fatalf("flags %q should conflict", tc.flags)
		}
	}
}

func TestPiProjectFileTrustFlagDoesNotConflictWithMode(t *testing.T) {
	got, err := Build("pi", Interactive, Request{Mode: ModeYolo, Flags: []string{"--no-approve"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, []string{"--no-approve"}) {
		t.Fatal(got.Args)
	}
}

func boolPtr(v bool) *bool { return &v }

func TestAgyACPMCPInheritsExplicitEnvironment(t *testing.T) {
	t.Setenv("AGY_TEST_PASSTHROUGH", "inherited")
	servers := AgyMCPServers(MCPConfig{Servers: map[string]MCPServer{"tool": {Command: "native", EnvPassthrough: []string{"AGY_TEST_PASSTHROUGH"}, Env: map[string]string{"EXPLICIT": "value"}}}})
	if len(servers) != 1 || len(servers[0].Env) != 2 || servers[0].Env[0].Name != "AGY_TEST_PASSTHROUGH" || servers[0].Env[0].Value != "inherited" || servers[0].Env[1].Name != "EXPLICIT" || servers[0].Env[1].Value != "value" {
		t.Fatal(servers)
	}
}

// TestSingleHeadlessExecutionPathGuard enforces that every headless-capable
// agent uses exactly one shared protocol transport (ACP preferred; Pi on RPC).
func TestSingleHeadlessExecutionPathGuard(t *testing.T) {
	for _, agent := range AgentOrder {
		_, supportsACP := Build(agent, ACP, Request{Cwd: "/ws"})
		_, supportsRPC := Build(agent, RPC, Request{Cwd: "/ws"})
		if supportsACP == nil && supportsRPC == nil {
			t.Fatalf("%s supports both ACP and RPC; keep one shared transport", agent)
		}
		if agent == "pi" {
			if supportsRPC != nil {
				t.Fatalf("pi must support RPC: %v", supportsRPC)
			}
		} else if agent != "opencode" {
			if supportsACP != nil {
				t.Fatalf("%s must support ACP: %v", agent, supportsACP)
			}
		}
	}
}

func TestCodexACPNormalAndYoloModes(t *testing.T) {
	normal, err := Build("codex", ACP, Request{Cwd: "/ws", Mode: ModeNormal})
	if err != nil || normal.Mode != "workspace-write" || normal.Env["INITIAL_AGENT_MODE"] != "workspace-write" {
		t.Fatalf("normal=%#v err=%v", normal, err)
	}
	yolo, err := Build("codex", ACP, Request{Cwd: "/ws", Mode: ModeYolo})
	if err != nil || yolo.Mode != "agent-full-access" || yolo.Env["INITIAL_AGENT_MODE"] != "agent-full-access" {
		t.Fatalf("yolo=%#v err=%v", yolo, err)
	}
}
