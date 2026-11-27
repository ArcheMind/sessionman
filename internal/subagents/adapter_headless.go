package subagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/execution"
	"github.com/ArcheMind/sessionman/internal/headless"
	"github.com/ArcheMind/sessionman/internal/hooks"
	"github.com/ArcheMind/sessionman/internal/nativedirs"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

// steerPrefix frames mid-turn text so the model attributes it to its coordinator instead of
// treating it as injection inside command output (verified hazard: unframed steers are ignored).
const steerPrefix = "Message from your coordinator (via sessionman Send):\n"

// headlessState is the per-process scratch beside the live headless session.
type headlessState struct {
	session      headless.Session
	stderrOffset int
	// piOffset is the Pi session file size at round start; the observer's provider-request
	// records after it are this round's effort evidence.
	piOffset int64
}

func (d *Daemon) sendHeadlessLocked(message, delivery string) controlResponse {
	if d.proc == nil {
		if err := d.startHeadlessLocked(); err != nil {
			return controlResponse{Error: err.Error()}
		}
	}
	seq := LastEventSeq(d.Dir)
	state := d.exec
	if d.running {
		if delivery == "queued" {
			d.queue = append(d.queue, message)
			return controlResponse{Disposition: "queued", Seq: seq}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		disposition, err := state.session.Steer(ctx, steerPrefix+message)
		if err != nil {
			return controlResponse{Error: err.Error()}
		}
		if disposition == headless.Idle {
			// A settled turn cannot consume the message; the daemon owns its next-round delivery.
			d.queue = append(d.queue, message)
			return controlResponse{Disposition: "queued", Seq: seq}
		}
		return controlResponse{Disposition: "steered", Seq: seq}
	}
	state.stderrOffset = len(d.proc.stream.Stderr())
	state.piOffset = 0
	if d.agent.Option.Agent == "pi" {
		if info, err := os.Stat(d.agent.Session); err == nil {
			state.piOffset = info.Size()
		}
	}
	if err := d.startHeadlessTurn(state, message); err != nil {
		return controlResponse{Error: err.Error()}
	}
	d.running = true
	return controlResponse{Disposition: "started", Seq: seq}
}

// startHeadlessTurn returns once the native side accepted the prompt, so a steer that
// follows is delivered into the running turn; the settled turn is reported asynchronously.
func (d *Daemon) startHeadlessTurn(state *headlessState, prompt string) error {
	accepted := make(chan error, 1)
	go func() {
		var once sync.Once
		res, err := state.session.Turn(context.Background(), prompt, func() { once.Do(func() { accepted <- nil }) })
		rejected := false
		once.Do(func() { rejected = true; accepted <- err })
		if !rejected {
			d.finishHeadlessTurn(state, res, err)
		}
	}()
	return <-accepted
}

func (d *Daemon) startHeadlessLocked() error {
	a := d.agent
	agent := a.Option.Agent
	transport, ok := agentparams.HeadlessTransport(agent)
	if !ok {
		return fmt.Errorf("%s has no native subagent execution adapter", agent)
	}
	if err := d.preStartLocked(); err != nil {
		return err
	}
	if agent == "pi" {
		if err := d.preparePiSessionLocked(); err != nil {
			return err
		}
	}
	req, err := d.parameterRequest()
	if err != nil {
		return err
	}
	mapped, err := agentparams.Build(agent, transport, req)
	if err != nil {
		return err
	}
	plan, auth, err := d.launchPlan(req, mapped)
	if err != nil {
		return err
	}
	stream, err := d.startStream(plan)
	if err != nil {
		return err
	}
	proc := &nativeProc{stream: stream, done: make(chan struct{})}
	state := &headlessState{}
	session, err := execution.Open(agent, stream, mapped, headless.Handlers{
		OnSession: d.handleSession,
		OnExit:    func() { d.procExit(proc, agent+" execution") },
		OnDebug:   d.debugf,
	})
	if err != nil {
		stream.Kill()
		return err
	}
	state.session = session
	d.proc, d.exec, d.procAuth = proc, state, auth

	id, err := session.Start(context.Background())
	if err != nil {
		stream.Kill()
		return err
	}
	if id != "" {
		a.NativeID = id
		if agent != "pi" {
			a.Session = id
		}
	}
	d.persistMetaLocked()
	return nil
}

// launchPlan adds the per-agent process environment around the protocol plan.
func (d *Daemon) launchPlan(req agentparams.Request, mapped agentparams.Plan) (runtime.CommandPlan, Auth, error) {
	agent := d.agent.Option.Agent
	plan := runtime.CommandPlan{Executable: mapped.Executable, Args: mapped.Args, Cwd: req.Cwd, Env: map[string]string{}, Agent: agent}
	var auth Auth
	if os.Getenv("SM_CONFIG_DIR") == "" && d.agent.Option.EffectiveUnifiedConfig() && d.Home != "" {
		configDir := filepath.Join(d.Home, ".sessionman", "config")
		ad, err := nativedirs.Resolve(agent, nativedirs.Dirs{SessionDir: os.Getenv("SM_SESSION_DIR"), ConfigDir: configDir})
		if err != nil {
			return plan, auth, err
		}
		if err := nativedirs.Prepare(ad); err != nil {
			return plan, auth, err
		}
		for k, v := range nativedirs.MergeEnv(ad) {
			plan.Env[k] = v
		}
		plan.Env["SM_CONFIG_DIR"] = configDir
		if len(ad.Args) > 0 {
			plan.Args = append(append([]string(nil), ad.Args...), plan.Args...)
		}
	}
	switch agent {
	case "agy":
		for k, v := range d.agyEnvironment() {
			plan.Env[k] = v
		}
		plan.UnsetEnv = []string{hooks.SessionReportEnv}
	case "codex":
		plan.Env["RUST_LOG"] = codexTelemetryFilter
		// Codex colors its log lines even into a pipe, which breaks the telemetry parser.
		plan.Env["NO_COLOR"] = "1"
		codexHome := plan.Env["CODEX_HOME"]
		if codexHome == "" {
			codexHome = os.Getenv("CODEX_HOME")
		}
		auth = Auth{Key: "CODEX_HOME", Dir: codexHome}
	case "pi":
		if err := d.injectPiDelegation(&plan); err != nil {
			return plan, auth, err
		}
		if d.agent.Option.Effort != "" {
			observerArgs, observerEnv, err := preparePiObserver(d.agent)
			if err != nil {
				return plan, auth, err
			}
			plan.Args = append(plan.Args, observerArgs...)
			for key, value := range observerEnv {
				plan.Env[key] = value
			}
		}
		piDir := plan.Env["PI_CODING_AGENT_DIR"]
		if piDir == "" {
			piDir = os.Getenv("PI_CODING_AGENT_DIR")
		}
		auth = Auth{Key: "PI_CODING_AGENT_DIR", Dir: piDir}
	}
	return plan, auth, nil
}

func (d *Daemon) preparePiSessionLocked() error {
	a := d.agent
	if a.Session == "" {
		dir := filepath.Join(d.Home, ".local", "state", "sessionman", "subagents", a.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		a.Session = filepath.Join(dir, "session.jsonl")
	} else if _, err := os.Stat(a.Session); err != nil {
		return fmt.Errorf("original Pi session unavailable: %w; no replacement created", err)
	}
	return nil
}

func (d *Daemon) agyEnvironment() map[string]string {
	env := map[string]string{"SM_MCP_CWD": d.agent.Cwd, "SM_HOST_AGENT_DIR": d.Dir, "SM_DELEGATION_DEPTH": fmt.Sprint(d.meta.DelegationDepth + 1)}
	if home := os.Getenv("SM_SUBAGENT_HOME"); home != "" {
		env["SM_SUBAGENT_HOME"] = home
	}
	return env
}

// handleSession records the native session an agent reports when it only assigns one during
// a turn, and refuses a different one on a resumed session.
func (d *Daemon) handleSession(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.agent
	switch {
	case a.NativeID == "":
		a.NativeID = id
		if a.Option.Agent != "pi" {
			a.Session = id
		}
		d.persistMetaLocked()
	case a.NativeID != id:
		d.stopping = true
		d.running = false
		d.appendReportLocked(Event{Status: "failed", Failure: fmt.Sprintf("%s returned a different native session; no replacement accepted", a.Option.Agent)})
		if d.proc != nil {
			d.proc.stream.Kill()
		}
	}
}

func (d *Daemon) finishHeadlessTurn(state *headlessState, res headless.TurnResult, turnErr error) {
	if errors.Is(turnErr, headless.ErrExited) {
		return
	}
	remaining := d.worktreeState()
	d.mu.Lock()
	if d.exec != state || d.stopping {
		d.mu.Unlock()
		return
	}
	agent := d.agent.Option.Agent
	report := Event{Status: "completed", Report: res.Text, Usage: res.Usage, Worktree: remaining}
	failure := res.Failure
	switch agent {
	case "agy":
		report.Auth = "agy native credential resolution (no identity override)"
		if d.agent.Option.Effort != "" {
			report.Evidence = "WARNING: agy accepted the thought_level request=" + d.agent.Option.Effort + "; native provider request effort was not recorded and is unverified"
		}
	case "codex":
		stderr := d.proc.stream.Stderr()
		if state.stderrOffset <= len(stderr) {
			stderr = stderr[state.stderrOffset:]
		}
		report.Evidence, _ = verifyCodex(d.agent, stderr)
	case "pi":
		if d.agent.Option.Effort != "" {
			var err error
			report.Evidence, err = verifyPi(d.agent, state.piOffset)
			if err != nil {
				failure = err.Error()
			}
		}
	}
	switch {
	case turnErr != nil:
		report.Status, report.Report = "failed", ""
		report.Failure = "ExternalSubagent failed: " + runtime.RedactSecrets(turnErr.Error())
	case failure != "":
		report.Status = "failed"
		report.Failure = "ExternalSubagent failed: " + runtime.RedactSecrets(failure)
	case res.Cancelled:
		report.Status, report.Report, report.Failure = "cancelled", "", "Native turn cancelled"
	case !res.Completed || strings.TrimSpace(res.Text) == "":
		report.Status, report.Report = "failed", ""
		report.Failure = "ExternalSubagent exited without a complete report; no implicit retry"
	}
	d.running = false
	d.appendReportLocked(report)
	d.mu.Unlock()
	d.deliverQueue()
}
