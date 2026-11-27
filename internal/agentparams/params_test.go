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

func TestHeadlessTransportsCarryUnifiedFields(t *testing.T) {
	mcp := MCPConfig{Servers: map[string]MCPServer{"bridge": {Command: "node", Args: []string{"bridge.mjs"}, Env: map[string]string{"A": "1"}}}}
	agy, err := Build("agy", ACP, Request{Cwd: "/ws", Model: "gemini", Effort: "high", Mode: ModeYolo, Session: "native", SystemPrompt: "role", MCP: mcp})
	if err != nil {
		t.Fatal(err)
	}
	if agy.Executable != "agy" || !reflect.DeepEqual(agy.Args, []string{"acp", "--stdio", "--dangerously-skip-permissions"}) || agy.Session != "native" || agy.Model != "gemini" || agy.Effort != "high" || agy.SystemPrompt != "role" {
		t.Fatalf("agy ACP %#v", agy)
	}
	claude, err := Build("claude", Stream, Request{Cwd: "/ws", Model: "opus", Effort: "high", Mode: ModeYolo, Session: testUUID, SystemPrompt: "role", UserConfig: boolPtr(false), MCP: mcp})
	if err != nil {
		t.Fatal(err)
	}
	wantClaude := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--resume", testUUID, "--dangerously-skip-permissions", "--setting-sources", "project,local", "--effort", "high", "--append-system-prompt", "role", "--mcp-config", `{"mcpServers":{"bridge":{"args":["bridge.mjs"],"command":"node","env":{"A":"1"},"type":"stdio"}}}`, "--model", "opus"}
	if claude.Executable != "claude" || claude.Session != testUUID || !reflect.DeepEqual(claude.Args, wantClaude) {
		t.Fatalf("claude stream %#v", claude)
	}
	codex, err := Build("codex", AppServer, Request{Cwd: "/ws", Model: "gpt-6", Effort: "high", Mode: ModeYolo, Session: testUUID, SystemPrompt: "role", MCP: mcp, Flags: []string{"--verbose"}})
	if err != nil {
		t.Fatal(err)
	}
	wantParams := map[string]any{
		"cwd": "/ws", "model": "gpt-6", "developerInstructions": "role", "approvalPolicy": "never", "sandbox": "danger-full-access",
		"config": map[string]any{"model_reasoning_effort": "high", "mcp_servers": map[string]any{"bridge": map[string]any{"command": "node", "args": []string{"bridge.mjs"}, "env": map[string]string{"A": "1"}}}},
	}
	if codex.Executable != "codex" || !reflect.DeepEqual(codex.Args, []string{"app-server", "--verbose"}) || codex.Session != testUUID || !reflect.DeepEqual(codex.Params, wantParams) {
		t.Fatalf("codex app-server %#v", codex)
	}
}

func TestRejectsUnsupportedAndConflictingParameters(t *testing.T) {
	cases := []struct {
		agent string
		t     Transport
		r     Request
	}{
		{"codex", Interactive, Request{UserConfig: boolPtr(false)}},
		{"codex", AppServer, Request{UserConfig: boolPtr(false)}},
		{"pi", RPC, Request{MCP: MCPConfig{Servers: map[string]MCPServer{"x": {Command: "node"}}}}},
		{"agy", Exec, Request{SystemPrompt: "role"}},
		{"dsh", Interactive, Request{Model: "model"}},
		{"claude", Interactive, Request{Model: "opus", Flags: []string{"--model=sonnet"}}},
		{"codex", Interactive, Request{Effort: "high", Flags: []string{"--config=model_reasoning_effort=low"}}},
		{"claude", Interactive, Request{Session: "bad"}},
		{"claude", Stream, Request{Session: "bad"}},
		{"codex", AppServer, Request{Session: "bad"}},
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

// TestSingleHeadlessExecutionPathGuard enforces that every agent has exactly one headless
// transport that both `session continue` and ExternalSubagents run over.
func TestSingleHeadlessExecutionPathGuard(t *testing.T) {
	want := map[string]Transport{"claude": Stream, "codex": AppServer, "agy": ACP, "pi": RPC}
	headless := []Transport{Stream, AppServer, ACP, RPC}
	for _, agent := range AgentOrder {
		transport, ok := HeadlessTransport(agent)
		if wantTransport, has := want[agent]; has != ok || transport != wantTransport {
			t.Fatalf("%s headless transport %q (ok=%v), want %q", agent, transport, ok, wantTransport)
		}
		var supported []Transport
		for _, candidate := range headless {
			if _, err := Build(agent, candidate, Request{Cwd: "/ws"}); err == nil {
				supported = append(supported, candidate)
			}
		}
		if ok && !reflect.DeepEqual(supported, []Transport{transport}) {
			t.Fatalf("%s supports headless transports %v, want only %s", agent, supported, transport)
		}
		if !ok && len(supported) != 0 {
			t.Fatalf("%s has no headless path but supports %v", agent, supported)
		}
	}
}
