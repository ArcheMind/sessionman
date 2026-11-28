// Package sdk exposes SessionMan's headless agent execution contract for
// external tools (such as nl-test) without leaking internal packages.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/execution"
	"github.com/ArcheMind/sessionman/internal/headless"
	"github.com/ArcheMind/sessionman/internal/hooks"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

// ModeNormal and ModeYolo match SessionMan's `--mode` values.
const (
	ModeNormal = agentparams.ModeNormal
	ModeYolo   = agentparams.ModeYolo
)

// TurnRequest aligns with SessionMan's CLI run/continue options for executing
// a single headless turn against a supported agent.
type TurnRequest struct {
	Agent        string
	Cwd          string
	Model        string
	Effort       string
	Mode         string
	Session      string
	SystemPrompt string
	UserConfig   *bool
	Flags        []string
	Prompt       string
	OnLine       func(line string)
}

// TurnResult aligns with SessionMan's internal headless.TurnResult.
type TurnResult struct {
	SessionID string
	Completed bool
	Cancelled bool
	Failure   string
	Text      string
	Usage     json.RawMessage
}

// RunTurn launches the agent over its native headless transport (ACP for agy,
// stream-json for claude, app-server for codex, rpc for pi) and executes one turn.
func RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if strings.TrimSpace(req.Agent) == "" {
		return TurnResult{}, fmt.Errorf("agent is required")
	}
	transport, ok := agentparams.HeadlessTransport(req.Agent)
	if !ok {
		return TurnResult{}, fmt.Errorf("unsupported agent %q", req.Agent)
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "."
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return TurnResult{}, fmt.Errorf("resolve cwd: %w", err)
	}

	mapped, err := agentparams.Build(req.Agent, transport, agentparams.Request{
		Cwd:          absCwd,
		Model:        req.Model,
		Effort:       req.Effort,
		Mode:         req.Mode,
		Session:      req.Session,
		SystemPrompt: req.SystemPrompt,
		UserConfig:   req.UserConfig,
		Flags:        req.Flags,
	})
	if err != nil {
		return TurnResult{}, err
	}

	plan := runtime.CommandPlan{
		Executable: mapped.Executable,
		Args:       mapped.Args,
		Cwd:        absCwd,
		Agent:      req.Agent,
	}
	if req.Agent == "agy" {
		plan.UnsetEnv = []string{hooks.SessionReportEnv}
		plan.Env = map[string]string{"SM_MCP_CWD": absCwd}
	}

	stream, err := (runtime.ExecRunner{}).StartStream(plan)
	if err != nil {
		return TurnResult{}, fmt.Errorf("%s execution failed: %s", req.Agent, runtime.RedactSecrets(err.Error()))
	}
	defer shutdownStream(stream)

	session, err := execution.Open(req.Agent, stream, mapped, headless.Handlers{
		OnLine: req.OnLine,
	})
	if err != nil {
		return TurnResult{}, err
	}

	wrapErr := func(err error) error {
		if ctx.Err() != nil {
			return err
		}
		msg := fmt.Sprintf("%s execution failed: %s", req.Agent, runtime.RedactSecrets(err.Error()))
		if stderr := runtime.RedactSecrets(strings.TrimSpace(stream.Stderr())); stderr != "" && !strings.Contains(err.Error(), "stderr:") {
			msg += "; stderr: " + stderr
		}
		return fmt.Errorf("%s", msg)
	}

	if _, err := session.Start(ctx); err != nil {
		return TurnResult{}, wrapErr(err)
	}
	res, err := session.Turn(ctx, req.Prompt, nil)
	if err != nil {
		return TurnResult{}, wrapErr(err)
	}
	return TurnResult{
		SessionID: res.SessionID,
		Completed: res.Completed,
		Cancelled: res.Cancelled,
		Failure:   res.Failure,
		Text:      res.Text,
		Usage:     res.Usage,
	}, nil
}

func shutdownStream(stream *runtime.Stream) {
	if stream == nil {
		return
	}
	_ = stream.CloseStdin()
	done := make(chan struct{})
	go func() {
		_ = stream.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		stream.Kill()
		<-done
	}
}
