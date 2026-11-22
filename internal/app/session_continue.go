package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ArcheMind/agentx/internal/acp"
	"github.com/ArcheMind/agentx/internal/agentparams"
	"github.com/ArcheMind/agentx/internal/drivers"
	"github.com/ArcheMind/agentx/internal/hooks"
	"github.com/ArcheMind/agentx/internal/nativedirs"
	"github.com/ArcheMind/agentx/internal/pirpc"
	"github.com/ArcheMind/agentx/internal/runtime"
	"github.com/ArcheMind/agentx/internal/sessions"
)

const sessionContinueUsage = "usage: sm [--json|--yaml] session continue <target-agent> [file] [--stream] [--model <model>] [--effort <level>] [--append-system-prompt <text>] [--mcp-config <file|json>] [--mode <normal|yolo>] [--user-config <true|false>] [--workspace <path>] [-- <native args...>]"

// sessionContinue runs one headless turn on a unified transcript: import the
// history into the target agent's native store, resume it with the trailing
// user message as the prompt, and print the newly recorded messages as a
// transcript delta. The caller stays the owner of the full transcript.
func (a App) sessionContinue(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(sessionContinueUsage)
	}
	agent, err := a.Registry.Get(args[0])
	if err != nil {
		return err
	}
	var file, workspace string
	var stream bool
	request := runtime.RunRequest{}
	for index := 1; index < len(args); index++ {
		if handled, err := parseRunOption(args, &index, &request); handled || err != nil {
			if err != nil {
				return err
			}
			continue
		}
		switch args[index] {
		case "--stream":
			stream = true
		case "--workspace":
			index++
			if index >= len(args) {
				return fmt.Errorf("--workspace requires a value")
			}
			workspace = args[index]
		default:
			if file == "" && !strings.HasPrefix(args[index], "--") {
				file = args[index]
				continue
			}
			return fmt.Errorf("unknown session continue option %q", args[index])
		}
	}
	model := request.Model

	if agent.ID != "agy" && agent.ID != "claude" && agent.ID != "codex" && agent.ID != "pi" {
		return fmt.Errorf("%s has no headless continue adapter", agent.ID)
	}
	if _, err := agentparams.Build(agent.ID, continueTransport(agent.ID), request); err != nil {
		return err
	}
	// The caller owns this process (a host harness); SIGTERM/SIGINT must cancel
	// the native turn's whole process group instead of orphaning it.
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if agent.ID == "pi" && model == "" {
		return fmt.Errorf("--model is required for pi (provider/model)")
	}
	detail, err := a.readTranscript(file)
	if err != nil {
		return err
	}
	prompt, history, err := splitContinuePrompt(detail.Messages)
	if err != nil {
		return err
	}
	workspace, err = resolveContinueWorkspace(workspace, detail.Workspace)
	if err != nil {
		return err
	}
	detail.Workspace = workspace
	if agent.ID == "pi" {
		var imported sessions.Session
		imported, request.SystemPrompt = sessions.PreparePiTranscript(sessions.Session{Messages: history}, request.SystemPrompt)
		history = imported.Messages
	}

	token := ""
	prefix := 0
	// Pi always gets a session file (even an empty one): --session is how pi
	// both resumes and records, and the recorded file is the delta source.
	if len(history) > 0 || agent.ID == "pi" {
		imported := detail
		// Claude's session scanner rejects records without a parseable
		// timestamp ("No conversation found"), so empty ones are filled; the
		// parent chain is rewritten positionally so delta-appended history
		// stays on one native ancestor chain; tool ids the native APIs would
		// reject are mapped to deterministic safe ids, and message ids over
		// the Codex item id cap are shortened.
		imported.Messages = sessions.SanitizeMessageIDs(sessions.SanitizeToolIDs(sessions.RechainMessages(fillTimestamps(history))))
		imported.MessageCount = len(history)
		token, err = a.writeResumeSession(ctx, imported, agent.ID, request)
		if err != nil {
			return err
		}
		importPath, err := a.continueSessionFile(agent.ID, workspace, token)
		if err != nil {
			return err
		}
		before, err := sessions.ParseNativeSessionFile(agent.ID, importPath)
		if err != nil {
			return err
		}
		prefix = len(before.Messages)
	}

	request.Cwd, request.Session = workspace, token
	execCtx, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()
	var transcriptStream *continueTranscriptStream
	if stream && agent.ID != "agy" {
		transcriptStream = newContinueTranscriptStream(a, agent.ID, model, workspace, token, prefix, cancelExecution)
	}

	var run continueRun
	switch agent.ID {
	case "pi":
		run, err = a.runContinuePiRPC(execCtx, request, model, prompt, transcriptStream)
	default:
		run, err = a.runContinueACP(execCtx, agent.ID, request, prompt, transcriptStream)
	}
	if transcriptStream != nil {
		if stopErr := transcriptStream.stop(); stopErr != nil {
			return stopErr
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("session continue cancelled: %w", ctx.Err())
	}
	if run.failure != "" {
		return fmt.Errorf("%s turn failed: %s", agent.ID, runtime.RedactSecrets(run.failure))
	}
	if err != nil {
		return err
	}
	if !run.completed {
		return fmt.Errorf("%s exited without completing the turn", agent.ID)
	}
	if run.sessionID == "" && agent.ID != "pi" {
		return fmt.Errorf("%s reported no native session for the turn", agent.ID)
	}

	resultToken := token
	if agent.ID != "pi" {
		resultToken = run.sessionID
	}
	resultPath, err := a.continueSessionFile(agent.ID, workspace, resultToken)
	if err != nil {
		return err
	}
	full, err := sessions.ParseNativeSessionFile(agent.ID, resultPath)
	if err != nil {
		return err
	}
	if len(full.Messages) < prefix {
		return fmt.Errorf("%s recorded fewer messages (%d) than the imported history (%d)", agent.ID, len(full.Messages), prefix)
	}
	delta := continueDelta(full.Messages, prefix)
	if agent.ID == "pi" {
		delta = sessions.RechainMessages(delta)
	}
	if len(delta) == 0 {
		return fmt.Errorf("%s completed the turn but recorded no new messages", agent.ID)
	}
	delta = namespaceDelta(delta, continueRunID(agent.ID, token, run.sessionID))

	out := sessions.Session{
		ID:           run.sessionID,
		Provider:     agent.ID,
		Workspace:    workspace,
		StartedAt:    full.StartedAt,
		UpdatedAt:    full.UpdatedAt,
		MessageCount: len(delta),
		Source:       "continue",
		Messages:     delta,
	}
	if out.ID == "" {
		out.ID = full.ID
	}
	if stream {
		if transcriptStream == nil {
			// AGY messages are emitted only after the final native transcript is available.
			transcriptStream = &continueTranscriptStream{app: a, agent: agent.ID}
		}
		return transcriptStream.finish(out)
	}
	return writeStructured(a.Stdout, out, a.structuredDefault())
}

func continueDelta(messages []sessions.Message, prefix int) []sessions.Message {
	if len(messages) < prefix {
		return nil
	}
	delta := messages[prefix:]
	// The caller owns the prompt and history; per-turn native setup before
	// the first assistant must not snowball into its accumulated transcript.
	for len(delta) > 0 && delta[0].Role != "assistant" {
		delta = delta[1:]
	}
	return delta
}

// writeResumeSession materializes a transcript as a native session file.
// Codex sessions go to the auth-discovered home (CODEX_HOME when set),
// because that is the store the codex CLI reads.
func (a App) writeResumeSession(ctx context.Context, detail sessions.Session, agentID string, request runtime.RunRequest) (string, error) {
	if agentID == "codex" {
		codexHome := nativedirs.CodexHome()
		provider, err := drivers.CodexProvider(ctx, a.Runner, runtime.CommandPlan{Cwd: detail.Workspace, Agent: "codex"}, request)
		if err != nil {
			return "", err
		}
		return sessions.WriteCodexSessionFile(detail, codexHome, provider)
	}
	if agentID == "agy" {
		return sessions.WriteAgySessionFile(detail, nativedirs.AgyAppDataDir(a.Sessions.HomeDir))
	}
	return sessions.WriteSessionFile(detail, agentID, resumeStateDir(a.Sessions.HomeDir), a.Sessions.HomeDir)
}

func resumeStateDir(home string) string {
	return filepath.Join(home, ".local", "state", "sessionman", "resume")
}

func fillTimestamps(messages []sessions.Message) []sessions.Message {
	now := time.Now().UTC().Format(time.RFC3339)
	out := make([]sessions.Message, len(messages))
	for i, message := range messages {
		if message.Timestamp == "" {
			message.Timestamp = message.RecordedAt
		}
		if message.Timestamp == "" {
			message.Timestamp = now
		}
		out[i] = message
	}
	return out
}

// splitContinuePrompt peels the trailing user message off the transcript; its
// text is the native prompt and everything before it is the imported history.
func splitContinuePrompt(messages []sessions.Message) (string, []sessions.Message, error) {
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("transcript has no messages; nothing to continue")
	}
	last := messages[len(messages)-1]
	if last.Role != "user" {
		return "", nil, fmt.Errorf("transcript must end with a user message; got role %q", last.Role)
	}
	var parts []string
	for _, block := range last.Content {
		if block.Type != "text" {
			return "", nil, fmt.Errorf("the trailing user message allows only text blocks; got %q", block.Type)
		}
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	prompt := strings.Join(parts, "\n")
	if strings.TrimSpace(prompt) == "" {
		return "", nil, fmt.Errorf("the trailing user message has no text to send")
	}
	return prompt, messages[:len(messages)-1], nil
}

// resolveContinueWorkspace resolves symlinks because Claude keys its project
// store by the resolved working directory.
func resolveContinueWorkspace(flagValue, transcriptValue string) (string, error) {
	workspace := flagValue
	if workspace == "" {
		workspace = transcriptValue
	}
	if workspace == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		workspace = cwd
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("workspace %s is not usable: %w", absolute, err)
	}
	return resolved, nil
}

// continueSessionFile maps a session token to the native file that records it.
func (a App) continueSessionFile(agentID, workspace, token string) (string, error) {
	switch agentID {
	case "agy":
		return filepath.Join(nativedirs.AgyAppDataDir(a.Sessions.HomeDir), "conversations", token+".db"), nil
	case "claude":
		return sessions.ClaudeProjectFile(a.Sessions.HomeDir, workspace, token), nil
	case "codex":
		return sessions.FindCodexRollout(nativedirs.CodexHome(), token)
	default:
		return token, nil
	}
}

// continueRunID is a fresh-per-call namespace that keeps delta message ids
// unique in the caller's accumulated transcript across turns.
func continueRunID(agentID, token, sessionID string) string {
	if agentID == "pi" {
		return strings.TrimSuffix(filepath.Base(token), ".jsonl")
	}
	if token != "" {
		return token
	}
	return sessionID
}

func namespaceDelta(messages []sessions.Message, runID string) []sessions.Message {
	local := make(map[string]string, len(messages))
	for _, message := range messages {
		if message.ID != "" {
			local[message.ID] = runID + ":" + message.ID
		}
	}
	out := make([]sessions.Message, len(messages))
	for i, message := range messages {
		if mapped, ok := local[message.ID]; ok {
			message.ID = mapped
		}
		if mapped, ok := local[message.ParentID]; ok {
			message.ParentID = mapped
		}
		out[i] = message
	}
	return out
}

func readMCPConfig(value string) (agentparams.MCPConfig, error) {
	data := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		loaded, err := os.ReadFile(value)
		if err != nil {
			return agentparams.MCPConfig{}, fmt.Errorf("read MCP config: %w", err)
		}
		data = loaded
	}
	var config agentparams.MCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return agentparams.MCPConfig{}, fmt.Errorf("parse MCP config JSON: %w", err)
	}
	if len(config.Servers) == 0 {
		return agentparams.MCPConfig{}, fmt.Errorf("MCP config must declare at least one server under mcpServers")
	}
	for name, server := range config.Servers {
		if server.Command == "" {
			return agentparams.MCPConfig{}, fmt.Errorf("MCP server %q must declare a command", name)
		}
	}
	return config, nil
}

type continueRun struct {
	sessionID string
	completed bool
	failure   string
}

func continueTransport(agentID string) agentparams.Transport {
	if agentID == "pi" {
		return agentparams.RPC
	}
	return agentparams.ACP
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

func (a App) runContinueACP(ctx context.Context, agentID string, request runtime.RunRequest, prompt string, transcriptStream *continueTranscriptStream) (continueRun, error) {
	if err := nativedirs.CachedError(agentID); err != nil {
		return continueRun{}, err
	}
	mapped, err := agentparams.Build(agentID, agentparams.ACP, request)
	if err != nil {
		return continueRun{}, err
	}
	starter, ok := a.Runner.(runtime.StreamStarter)
	if !ok {
		return continueRun{}, fmt.Errorf("%s continue requires a stream runner", agentID)
	}
	env := map[string]string{}
	for k, v := range mapped.Env {
		env[k] = v
	}
	var unset []string
	if agentID == "agy" {
		unset = []string{hooks.SessionReportEnv}
	}
	plan := runtime.CommandPlan{
		Executable: mapped.Executable,
		Args:       mapped.Args,
		Cwd:        request.Cwd,
		Env:        env,
		UnsetEnv:   unset,
		Agent:      agentID,
	}
	if agentID == "agy" {
		prepareAgyNativePlan("agy", &plan)
	}
	stream, err := starter.StartStream(plan)
	if err != nil {
		return continueRun{}, fmt.Errorf("%s execution failed: %s", agentID, runtime.RedactSecrets(err.Error()))
	}
	defer shutdownStream(stream)

	var onLine func(string)
	if transcriptStream != nil {
		onLine = func(line string) {
			if sid, _, _, ok := acp.ParseLine(line); ok {
				transcriptStream.Wake(sid)
			}
		}
	}
	client := acp.New(stream, acp.Handlers{Agent: agentID, OnLine: onLine})
	clientVer := ""
	if agentID != "agy" {
		clientVer = "1.0"
	}
	info, err := client.Start(ctx, acp.StartOptions{
		ClientVersion:   clientVer,
		SessionID:       request.Session,
		Params:          mapped.Params,
		SystemPrompt:    mapped.SystemPrompt,
		Model:           mapped.Model,
		Effort:          mapped.Effort,
		Mode:            mapped.Mode,
		TrustCodexHooks: agentID == "codex",
		Cwd:             request.Cwd,
	})
	if err != nil {
		return continueRun{}, fmt.Errorf("%s execution failed: %s; stderr: %s", agentID, runtime.RedactSecrets(err.Error()), runtime.RedactSecrets(strings.TrimSpace(stream.Stderr())))
	}
	if transcriptStream != nil {
		transcriptStream.Wake(info.SessionID)
	}
	if info.PromptPrefix != "" {
		prompt = "<system-prompt>\n" + info.PromptPrefix + "\n</system-prompt>\n\n" + prompt
	}
	result, err := client.PromptSync(ctx, info.SessionID, prompt)
	if err != nil {
		if result.Err != nil {
			return continueRun{sessionID: info.SessionID, failure: err.Error()}, nil
		}
		return continueRun{sessionID: info.SessionID}, err
	}
	switch result.StopReason {
	case "end_turn":
		return continueRun{sessionID: info.SessionID, completed: true}, nil
	case "cancelled":
		return continueRun{sessionID: info.SessionID, failure: "native turn cancelled"}, nil
	default:
		return continueRun{sessionID: info.SessionID, failure: fmt.Sprintf("stopReason=%s", result.StopReason)}, nil
	}
}

func (a App) runContinuePiRPC(ctx context.Context, request runtime.RunRequest, model, prompt string, transcriptStream *continueTranscriptStream) (continueRun, error) {
	plan, err := drivers.Plan("pi", agentparams.RPC, request)
	if err != nil {
		return continueRun{}, err
	}
	starter, ok := a.Runner.(runtime.StreamStarter)
	if !ok {
		return continueRun{}, fmt.Errorf("pi continue requires a stream runner")
	}
	stream, err := starter.StartStream(plan)
	if err != nil {
		return continueRun{}, fmt.Errorf("pi execution failed: %s", runtime.RedactSecrets(err.Error()))
	}
	defer shutdownStream(stream)

	var onLine func(string) error
	if transcriptStream != nil {
		onLine = func(line string) error {
			_, writeErr := transcriptStream.Write([]byte(line + "\n"))
			return writeErr
		}
	}
	turn, err := pirpc.RunTurn(ctx, stream, prompt, model, onLine)
	if err != nil {
		if ctx.Err() != nil {
			return continueRun{}, err
		}
		stderr := runtime.RedactSecrets(strings.TrimSpace(stream.Stderr()))
		if stderr != "" {
			return continueRun{}, fmt.Errorf("pi execution failed: %s; stderr: %s", runtime.RedactSecrets(err.Error()), stderr)
		}
		return continueRun{}, fmt.Errorf("pi execution failed: %s", runtime.RedactSecrets(err.Error()))
	}
	return continueRun{sessionID: turn.SessionID, completed: turn.Completed, failure: turn.Failure}, nil
}
