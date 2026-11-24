// Package execution opens the headless.Session for an agent's native protocol plan.
package execution

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/ArcheMind/agentx/internal/acp"
	"github.com/ArcheMind/agentx/internal/agentparams"
	"github.com/ArcheMind/agentx/internal/claudestream"
	"github.com/ArcheMind/agentx/internal/codexrpc"
	"github.com/ArcheMind/agentx/internal/headless"
	"github.com/ArcheMind/agentx/internal/nativedirs"
	"github.com/ArcheMind/agentx/internal/pirpc"
	"github.com/ArcheMind/agentx/internal/runtime"
)

func Open(agent string, stream *runtime.Stream, plan agentparams.Plan, h headless.Handlers) (headless.Session, error) {
	switch agent {
	case "claude":
		return claudestream.New(stream, plan.Session, h), nil
	case "codex":
		return codexrpc.New(stream, codexrpc.Options{Params: plan.Params, Resume: plan.Session, HooksFile: generatedCodexHooks()}, h), nil
	case "agy":
		return acp.NewSession(stream, acp.StartOptions{
			SessionID: plan.Session, Params: plan.Params, SystemPrompt: plan.SystemPrompt, Model: plan.Model, Effort: plan.Effort,
		}, h), nil
	case "pi":
		return pirpc.New(stream, plan.Model, h), nil
	}
	return nil, fmt.Errorf("%s has no headless execution path", agent)
}

func generatedCodexHooks() string {
	ad := nativedirs.Cached("codex")
	if ad == nil || !slices.Contains(ad.Generated, "hooks.json") {
		return ""
	}
	return filepath.Join(ad.GenHome, "hooks.json")
}
