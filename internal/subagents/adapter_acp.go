package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ArcheMind/agentx/internal/acp"
	"github.com/ArcheMind/agentx/internal/agentparams"
	"github.com/ArcheMind/agentx/internal/hooks"
	"github.com/ArcheMind/agentx/internal/runtime"
	"github.com/ArcheMind/agentx/internal/sessions"
)

// steerPrefix frames mid-turn text so the model attributes it to its coordinator instead of
// treating it as injection inside command output (verified hazard: unframed steers are ignored).
const steerPrefix = "Message from your coordinator (via sessionman Send):\n"

type agyPromptResult = acp.PromptResult

type acpState struct {
	client       *acp.Client
	text         string
	terminal     string
	failure      string
	cancelled    bool
	role         string
	stderrOffset int
}

func (d *Daemon) sendACPLocked(message, delivery string) controlResponse {
	if d.proc == nil {
		if err := d.startACPLocked(); err != nil {
			return controlResponse{Error: err.Error()}
		}
	}
	seq := LastEventSeq(d.Dir)
	state := d.acp
	if d.running {
		if delivery == "queued" {
			d.queue = append(d.queue, message)
			return controlResponse{Disposition: "queued", Seq: seq}
		}
		disposition, _, err := state.client.Steer(context.Background(), d.agent.Session, steerPrefix+message)
		if err != nil {
			return controlResponse{Error: err.Error()}
		}
		if disposition == "queued" {
			d.queue = append(d.queue, message)
			return controlResponse{Disposition: "queued", Seq: seq}
		}
		return controlResponse{Disposition: "steered", Seq: seq}
	}
	state.text, state.terminal, state.failure, state.cancelled = "", "", "", false
	state.stderrOffset = len(d.proc.stream.Stderr())
	if state.role != "" {
		if body, marked := strings.CutPrefix(message, sessions.SubagentMarker+"\n"); marked {
			message = sessions.SubagentMarker + "\n" + state.role + "\n\n" + body
		} else {
			message = state.role + "\n\n" + message
		}
	}
	state.client.ResetTurn(d.agent.Session)
	if _, err := state.client.SendPrompt(d.agent.Session, message); err != nil {
		return controlResponse{Error: err.Error()}
	}
	d.running = true
	return controlResponse{Disposition: "started", Seq: seq}
}

func (d *Daemon) startACPLocked() error {
	a := d.agent
	role, err := d.preStartLocked()
	if err != nil {
		return err
	}
	req, err := d.parameterRequest(role)
	if err != nil {
		return err
	}
	mapped, err := agentparams.Build(a.Option.Agent, agentparams.ACP, req)
	if err != nil {
		return err
	}
	env := map[string]string{}
	for k, v := range mapped.Env {
		env[k] = v
	}
	var unset []string
	var auth Auth
	switch a.Option.Agent {
	case "agy":
		for k, v := range d.agyEnvironment() {
			env[k] = v
		}
		unset = []string{hooks.SessionReportEnv}
	case "codex":
		env["RUST_LOG"] = codexTelemetryFilter
		auth = Auth{Key: "CODEX_HOME", Dir: os.Getenv("CODEX_HOME")}
	}
	stream, err := d.startStream(runtime.CommandPlan{
		Executable: mapped.Executable, Args: mapped.Args, Cwd: req.Cwd,
		Env: env, UnsetEnv: unset, Agent: a.Option.Agent,
	})
	if err != nil {
		return err
	}
	proc := &nativeProc{stream: stream, done: make(chan struct{})}
	state := &acpState{}
	client := acp.New(stream, acp.Handlers{
		Agent: a.Option.Agent,
		OnRequest: func(_ json.RawMessage, method string) (any, *acp.Error) {
			if method == "session/request_permission" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
			}
			return nil, &acp.Error{Code: -32601, Message: "interactive client request unavailable in an ExternalSubagent: " + method}
		},
		OnLine:    func(line string) { d.handleACPLine(state, line) },
		OnTurnEnd: func(res acp.PromptResult) { d.handleACPTurnEnd(state, res) },
		OnExit:    func() { d.procExit(proc, a.Option.Agent+" ACP execution") },
		OnDebug:   d.debugf,
	})
	state.client = client
	d.proc, d.acp, d.procAuth = proc, state, auth

	fail := func(err error) error { stream.Kill(); return err }
	clientVer := ""
	if a.Option.Agent != "agy" {
		clientVer = "1.0"
	}
	info, err := client.Start(context.Background(), acp.StartOptions{
		ClientVersion:   clientVer,
		SessionID:       a.Session,
		Params:          mapped.Params,
		SystemPrompt:    mapped.SystemPrompt,
		Model:           mapped.Model,
		Effort:          mapped.Effort,
		Mode:            mapped.Mode,
		TrustCodexHooks: a.Option.Agent == "codex",
		Cwd:             req.Cwd,
	})
	if err != nil {
		return fail(err)
	}
	state.role = info.PromptPrefix
	a.Session = info.SessionID
	a.NativeID = info.SessionID
	d.persistMetaLocked()
	return nil
}

func (d *Daemon) agyEnvironment() map[string]string {
	env := map[string]string{"SM_MCP_CWD": d.agent.Cwd, "SM_HOST_AGENT_DIR": d.Dir, "SM_DELEGATION_DEPTH": fmt.Sprint(d.meta.DelegationDepth + 1)}
	if home := os.Getenv("SM_SUBAGENT_HOME"); home != "" {
		env["SM_SUBAGENT_HOME"] = home
	}
	return env
}

func (d *Daemon) handleACPLine(state *acpState, line string) {
	sid, chunk, _, ok := acp.ParseLine(line)
	if !ok || chunk == "" {
		return
	}
	d.mu.Lock()
	if d.acp == state && !d.stopping && sid == d.agent.Session {
		state.text += chunk
	}
	d.mu.Unlock()
}

func (d *Daemon) handleACPTurnEnd(state *acpState, prompt acp.PromptResult) {
	d.mu.Lock()
	if d.acp != state || d.stopping {
		d.mu.Unlock()
		return
	}
	agentID := d.agent.Option.Agent
	if prompt.Err != nil {
		state.failure = fmt.Sprintf("%s session/prompt failed %s", agentID, prompt.Err.Error())
	} else {
		switch prompt.StopReason {
		case "end_turn":
		case "cancelled":
			state.cancelled = true
		case "max_tokens", "max_turn_requests", "refusal":
			state.terminal = fmt.Sprintf("%s prompt stopped with native stopReason=%s; no implicit retry", agentID, prompt.StopReason)
		default:
			state.failure = fmt.Sprintf("%s prompt returned invalid native stopReason %q; no implicit retry", agentID, prompt.StopReason)
		}
	}

	report := Event{Status: "completed", Report: state.text, Usage: prompt.Usage}
	switch agentID {
	case "agy":
		report.Auth = "agy native credential resolution (no identity override)"
		if d.agent.Option.Effort != "" {
			report.Evidence = "WARNING: agy accepted the thought_level request=" + d.agent.Option.Effort + "; native provider request effort was not recorded and is unverified"
		}
	case "codex":
		if d.proc != nil {
			stderr := d.proc.stream.Stderr()
			if state.stderrOffset <= len(stderr) {
				stderr = stderr[state.stderrOffset:]
			}
			report.Evidence, _ = verifyCodex(d.agent, stderr)
		}
	}
	switch {
	case state.failure != "":
		report.Status, report.Report, report.Failure = "failed", "", state.failure
	case state.cancelled:
		report.Status, report.Report, report.Failure = "cancelled", "", "Native turn cancelled"
	case state.terminal != "":
		report.Status, report.Failure = "failed", state.terminal
	case state.text == "":
		report.Status, report.Failure = "failed", "ExternalSubagent exited without a complete report; no implicit retry"
	}
	d.running = false
	report.Worktree = d.worktreeState()
	d.appendReportLocked(report)
	d.mu.Unlock()
	d.deliverQueue()
}
