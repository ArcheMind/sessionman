package drivers

import (
	"fmt"

	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/nativedirs"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

func Plan(agent string, transport agentparams.Transport, request agentparams.Request) (runtime.CommandPlan, error) {
	if err := nativedirs.CachedError(agent); err != nil {
		return runtime.CommandPlan{}, err
	}
	plan, err := agentparams.Build(agent, transport, request)
	if err != nil {
		return runtime.CommandPlan{}, err
	}
	if len(plan.Params) > 0 {
		return runtime.CommandPlan{}, fmt.Errorf("%s %s requires a native protocol session", agent, transport)
	}
	return runtime.CommandPlan{Executable: agent, Args: plan.Args, Cwd: request.Cwd, Agent: agent}, nil
}
