package subagents

import (
	"fmt"
	"os"
	"strings"

	"github.com/ArcheMind/agentx/internal/agentparams"
	"github.com/ArcheMind/agentx/internal/presets"
)

const (
	ModeNormal = agentparams.ModeNormal
	ModeYolo   = agentparams.ModeYolo
)

// Auth is the native credential source for one turn's report.
type Auth struct {
	Key, Dir string
}

func (a Auth) String() string {
	return fmt.Sprintf("%s=%q", a.Key, a.Dir)
}

// AuthFromEnv constructs an Auth from the process env the resolver already set.
func AuthFromEnv(agent string) Auth {
	switch agent {
	case "codex":
		return Auth{Key: "CODEX_HOME", Dir: os.Getenv("CODEX_HOME")}
	case "pi":
		return Auth{Key: "PI_CODING_AGENT_DIR", Dir: os.Getenv("PI_CODING_AGENT_DIR")}
	default:
		return Auth{}
	}
}

func Limits(o presets.Preset) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Mode: preset default is %s. ", presets.ModeLabel(o.Mode))
	text.WriteString("normal passes no permission or sandbox flags, so the agent's own configuration decides; yolo bypasses the agent's permission checks and sandbox. ")
	if !agentparams.Agents[o.Agent].Bypassable {
		text.WriteString("This agent rejects yolo. ")
	}
	text.WriteString("Preset user_config and flags are passed through to the agent. ")
	text.WriteString("The native credential directory is pinned before each execution; the report names it. ")
	text.WriteString(IsolationLimits())
	text.WriteString("Effort is fixed by the preset.")
	return text.String()
}
