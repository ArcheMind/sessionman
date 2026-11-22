package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ArcheMind/agentx/internal/agentparams"
	"github.com/ArcheMind/agentx/internal/pirpc"
	"github.com/ArcheMind/agentx/internal/runtime"
)

// piState is the per-round scratch beside the live `pi --mode rpc` process.
type piState struct {
	client  *pirpc.Client
	text    string
	usage   json.RawMessage
	failure string
	// offset is the session file size at round start; the observer's provider-request
	// records after it are this round's effort evidence.
	offset int64
}

func (d *Daemon) sendPiLocked(message, delivery string) controlResponse {
	if d.proc == nil {
		if err := d.startPiLocked(); err != nil {
			return controlResponse{Error: err.Error()}
		}
	}
	seq := LastEventSeq(d.Dir)
	if d.running {
		if delivery == "queued" {
			d.queue = append(d.queue, message)
			return controlResponse{Disposition: "queued", Seq: seq}
		}
		if err := d.pi.client.Send(context.Background(), "steer", steerPrefix+message); err != nil {
			return controlResponse{Error: err.Error()}
		}
		return controlResponse{Disposition: "steered", Seq: seq}
	}
	d.pi.text, d.pi.usage, d.pi.failure, d.pi.offset = "", nil, "", 0
	if info, err := os.Stat(d.agent.Session); err == nil {
		d.pi.offset = info.Size()
	}
	if err := d.pi.client.Send(context.Background(), "prompt", message); err != nil {
		return controlResponse{Error: err.Error()}
	}
	d.running = true
	return controlResponse{Disposition: "started", Seq: seq}
}

func (d *Daemon) startPiLocked() error {
	a := d.agent
	roleInstructions, err := d.preStartLocked()
	if err != nil {
		return err
	}
	if a.Session == "" {
		dir := filepath.Join(d.Home, ".local", "state", "sessionman", "subagents", a.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		a.Session = filepath.Join(dir, "session.jsonl")
	} else if _, err := os.Stat(a.Session); err != nil {
		return fmt.Errorf("original Pi session unavailable: %w; no replacement created", err)
	}
	req, err := d.parameterRequest(roleInstructions)
	if err != nil {
		return err
	}
	mapped, err := agentparams.Build(a.Option.Agent, agentparams.RPC, req)
	if err != nil {
		return err
	}
	plan := runtime.CommandPlan{Executable: "pi", Args: mapped.Args, Cwd: req.Cwd, Env: map[string]string{}, Agent: "pi"}
	if err := d.injectPiDelegation(&plan); err != nil {
		return err
	}
	if a.Option.Effort != "" {
		observerArgs, observerEnv, err := preparePiObserver(a)
		if err != nil {
			return err
		}
		plan.Args = append(plan.Args, observerArgs...)
		for key, value := range observerEnv {
			plan.Env[key] = value
		}
	}
	stream, err := d.startStream(plan)
	if err != nil {
		return err
	}
	proc := &nativeProc{stream: stream, done: make(chan struct{})}
	state := &piState{}
	state.client = pirpc.New(stream, d.handlePiLine, func() {
		d.procExit(proc, "pi execution")
	})
	d.proc, d.pi, d.procAuth = proc, state, Auth{Key: "PI_CODING_AGENT_DIR", Dir: os.Getenv("PI_CODING_AGENT_DIR")}
	d.persistMetaLocked()
	return nil
}

func (d *Daemon) handlePiLine(line string) {
	var event struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Message json.RawMessage `json:"message"`
		Aborted bool            `json:"aborted"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		d.debugf("invalid pi event: %v; raw=%s", err, line)
		return
	}
	switch event.Type {
	case "session":
		d.mu.Lock()
		if event.ID != "" && d.agent.NativeID == "" {
			d.agent.NativeID = event.ID
			d.persistMetaLocked()
		} else if event.ID != "" && d.agent.NativeID != event.ID {
			d.stopping = true
			d.running = false
			d.appendReportLocked(Event{Status: "failed", Failure: "Pi returned a different native session; no replacement accepted"})
			if d.proc != nil {
				d.proc.stream.Kill()
			}
		}
		d.mu.Unlock()
	case "message_end":
		msg, err := pirpc.ParseAssistant(event.Message, d.agent.Option.Model)
		if err != nil {
			return
		}
		d.mu.Lock()
		if d.pi != nil {
			d.pi.text, d.pi.usage, d.pi.failure = msg.Text, msg.Usage, msg.Failure
		}
		d.running = true
		d.mu.Unlock()
	case "agent_settled":
		remaining := d.worktreeState()
		d.mu.Lock()
		pi := d.pi
		if pi == nil {
			d.mu.Unlock()
			return
		}
		report := Event{Status: "completed", Report: pi.text, Usage: pi.usage, Worktree: remaining}
		if d.agent.Option.Effort != "" {
			var err error
			report.Evidence, err = verifyPi(d.agent, pi.offset)
			if err != nil {
				pi.failure = err.Error()
			}
		}
		failure := pi.failure
		if event.Aborted && failure == "" {
			failure = "pi run aborted"
		}
		switch {
		case failure != "":
			report.Status, report.Report = "failed", ""
			report.Failure = "ExternalSubagent failed: " + runtime.RedactSecrets(failure)
		case pi.text == "":
			report.Status, report.Report = "failed", ""
			report.Failure = "ExternalSubagent exited without a complete report and native session; no implicit retry"
		}
		pi.text, pi.usage, pi.failure = "", nil, ""
		d.running = false
		d.appendReportLocked(report)
		d.mu.Unlock()
		d.deliverQueue()
	}
}
