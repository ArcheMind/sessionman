package drivers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/agentparams"
)

func TestPlanConvertsNativeTransport(t *testing.T) {
	got, err := Plan("pi", agentparams.RPC, agentparams.Request{Cwd: "/ws", Model: "native"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mode", "rpc", "--no-approve"}
	if got.Executable != "pi" || got.Cwd != "/ws" || len(got.Args) < len(want) || !reflect.DeepEqual(got.Args[:len(want)], want) {
		t.Fatalf("plan = %#v", got)
	}
}

func TestPlanRejectsUnsupportedMCP(t *testing.T) {
	if _, err := Plan("pi", agentparams.RPC, agentparams.Request{MCP: agentparams.MCPConfig{Servers: map[string]agentparams.MCPServer{"tool": {Command: "node"}}}}); err == nil {
		t.Fatal("expected unsupported MCP")
	}
}

func TestPlanCarriesFlagsWithoutOverridingExplicitModel(t *testing.T) {
	got, err := Plan("claude", agentparams.Interactive, agentparams.Request{Model: "opus", Flags: []string{"--verbose"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, []string{"--model", "opus", "--verbose"}) {
		t.Fatal(got.Args)
	}
	_, err = Plan("claude", agentparams.Interactive, agentparams.Request{Model: "opus", Flags: []string{"--model", "sonnet"}})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestCommandPlanDoesNotDiscardProtocolParameters(t *testing.T) {
	for _, agent := range []string{"agy", "claude", "codex"} {
		if _, err := Plan(agent, agentparams.ACP, agentparams.Request{Cwd: "/ws", Model: "native"}); err == nil {
			t.Fatalf("%s discarded ACP session parameters", agent)
		}
	}
}
