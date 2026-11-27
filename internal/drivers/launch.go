package drivers

import (
	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

type NativeLaunch struct {
	Binary string
}

func (d NativeLaunch) PlanRun(request runtime.RunRequest) (runtime.CommandPlan, error) {
	return Plan(d.Binary, agentparams.Interactive, request)
}
