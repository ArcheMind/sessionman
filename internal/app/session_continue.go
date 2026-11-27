package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
	"github.com/ArcheMind/sessionman/internal/agentparams"
	"github.com/ArcheMind/sessionman/internal/drivers"
	"github.com/ArcheMind/sessionman/internal/execution"
	"github.com/ArcheMind/sessionman/internal/headless"
	"github.com/ArcheMind/sessionman/internal/hooks"
	"github.com/ArcheMind/sessionman/internal/nativedirs"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

const sessionContinueUsage = "usage: sm [--json|--yaml] session continue <target-agent> [file] [--stream] [--duplex] [--model <model>] [--effort <level>] [--append-system-prompt <text>] [--mcp-config <file|json>] [--mode <normal|yolo>] [--user-config <true|false>] [--workspace <path>] [-- <native args...>]"

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
	var stream, duplex bool
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
		case "--duplex":
			duplex = true
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
	if duplex {
		if !stream {
			return fmt.Errorf("--duplex requires --stream")
		}
		if file != "" && file != "-" {
			return fmt.Errorf("--duplex cannot be combined with a transcript file argument")
		}
	}
	model := request.Model

	transport, ok := agentparams.HeadlessTransport(agent.ID)
	if !ok {
		return fmt.Errorf("%s has no headless continue adapter", agent.ID)
	}
	if _, err := agentparams.Build(agent.ID, transport, request); err != nil {
		return err
	}
	// The caller owns this process (a host harness); SIGTERM/SIGINT must cancel
	// the native turn's whole process group instead of orphaning it.
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if agent.ID == "pi" && model == "" {
		return fmt.Errorf("--model is required for pi (provider/model)")
	}
	var detail sessions.ProtocolSession
	var duplexReader *bufio.Reader
	if duplex {
		duplexReader = bufio.NewReader(a.Stdin)
		detail, err = readDuplexProtocolTranscript(duplexReader)
	} else {
		detail, err = a.readProtocolTranscript(file)
	}
	if err != nil {
		return err
	}
	prompt, history, err := splitContinuePromptMessages(detail.Messages)
	if err != nil {
		return err
	}
	workspace, err = resolveContinueWorkspace(workspace, detail.Workspace)
	if err != nil {
		return err
	}
	detail.Workspace = workspace
	if agent.ID == "pi" {
		var imported sessions.ProtocolSession
		imported, request.SystemPrompt = sessions.PreparePiProtocolTranscript(sessions.ProtocolSession{Messages: history}, request.SystemPrompt)
		history = imported.Messages
	}

	token := ""
	prefix := 0
	runtimePrefix := 0
	// Pi always gets a session file (even an empty one): --session is how pi
	// both resumes and records, and the recorded file is the delta source.
	if len(history) > 0 || agent.ID == "pi" {
		imported := detail
		imported.Messages = sessions.SanitizeProtocolIDs(sessions.SanitizeProtocolToolIDs(sessions.RechainProtocolMessages(fillProtocolMessageTimestamps(history))))
		imported.MessageCount = len(imported.Messages)
		token, err = a.writeProtocolResumeSession(ctx, imported, agent.ID, request)
		if err != nil {
			return err
		}
		importPath, err := a.continueSessionFile(agent.ID, workspace, token)
		if err != nil {
			return err
		}
		before, err := sessions.ParseNativeProtocolFile(agent.ID, importPath)
		if err != nil {
			return err
		}
		prefix = len(before.Messages)
		runtimePrefix = len(before.RuntimeEvents)
	}

	request.Cwd, request.Session = workspace, token
	execCtx, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()

	var streamWriter *continueStreamWriter
	var duplexCtrl *duplexController
	if stream {
		streamWriter = newContinueStreamWriter(a.Stdout)
		defer streamWriter.Close()
	}
	if duplex {
		duplexCtrl = newDuplexController(execCtx, cancelExecution, streamWriter)
		defer duplexCtrl.close()
		duplexCtrl.start(duplexReader)
	}

	var transcriptStream *continueTranscriptStream
	if stream && agent.ID != "agy" {
		transcriptStream = newContinueTranscriptStream(a, streamWriter, agent.ID, model, workspace, token, prefix, cancelExecution)
	}

	run, err := a.runContinue(execCtx, agent.ID, transport, request, prompt, transcriptStream, duplexCtrl)
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
	full, err := sessions.ParseNativeProtocolFile(agent.ID, resultPath)
	if err != nil {
		return err
	}
	if len(full.Messages) < prefix {
		return fmt.Errorf("%s recorded fewer entries (%d) than the imported history (%d)", agent.ID, len(full.Messages), prefix)
	}
	delta := continueDeltaMessages(full.Messages, prefix)
	if agent.ID == "pi" {
		delta = sessions.RechainProtocolMessages(delta)
	}
	if len(delta) == 0 {
		return fmt.Errorf("%s completed the turn but recorded no new entries", agent.ID)
	}
	runID := continueRunID(agent.ID, token, run.sessionID)
	delta, runtimeEvents := namespaceProtocolDelta(delta, full.RuntimeEvents, runtimePrefix, runID)

	out := sessions.ProtocolSession{
		ProtocolVersion: transcript.ProtocolVersion,
		ID:              run.sessionID,
		Provider:        agent.ID,
		Workspace:       workspace,
		StartedAt:       full.StartedAt,
		UpdatedAt:       full.UpdatedAt,
		MessageCount:    len(delta),
		Source:          "continue",
		Messages:        delta,
		RuntimeEvents:   runtimeEvents,
	}
	if out.ID == "" {
		out.ID = full.ID
	}
	if stream {
		if transcriptStream == nil {
			// AGY entries are emitted only after the final native transcript is available.
			transcriptStream = &continueTranscriptStream{app: a, writer: streamWriter, agent: agent.ID}
		}
		return transcriptStream.finish(out)
	}
	return writeStructured(a.Stdout, out, a.structuredDefault())
}

func continueDeltaMessages(entries []sessions.ProtocolMessage, prefix int) []sessions.ProtocolMessage {
	if len(entries) < prefix {
		return nil
	}
	delta := entries[prefix:]
	// The caller owns the prompt and history; per-turn native setup before
	// the first assistant must not snowball into its accumulated transcript.
	for len(delta) > 0 && delta[0].Source != transcript.SourceS2 {
		delta = delta[1:]
	}
	return delta
}

func (a App) writeProtocolResumeSession(ctx context.Context, detail sessions.ProtocolSession, agentID string, request runtime.RunRequest) (string, error) {
	if agentID == "codex" {
		codexHome := nativedirs.CodexHome()
		provider, err := drivers.CodexProvider(ctx, a.Runner, runtime.CommandPlan{Cwd: detail.Workspace, Agent: "codex"}, request)
		if err != nil {
			return "", err
		}
		return sessions.WriteProtocolCodexSessionFile(detail, codexHome, provider)
	}
	if agentID == "agy" {
		return sessions.WriteProtocolAgySessionFile(detail, nativedirs.AgyAppDataDir(a.Sessions.HomeDir))
	}
	return sessions.WriteProtocolSessionFile(detail, agentID, resumeStateDir(a.Sessions.HomeDir), a.Sessions.HomeDir)
}

func fillProtocolMessageTimestamps(entries []sessions.ProtocolMessage) []sessions.ProtocolMessage {
	now := time.Now().UTC().Format(time.RFC3339)
	out := make([]sessions.ProtocolMessage, len(entries))
	for i, entry := range entries {
		if entry.Timestamp == "" {
			entry.Timestamp = entry.RecordedAt
		}
		if entry.Timestamp == "" {
			entry.Timestamp = now
		}
		out[i] = entry
	}
	return out
}

func splitContinuePromptMessages(entries []sessions.ProtocolMessage) (string, []sessions.ProtocolMessage, error) {
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("transcript has no messages; nothing to continue")
	}
	last := entries[len(entries)-1]
	if last.Source != transcript.SourceUser || last.Mutability != transcript.MutabilityDelta {
		return "", nil, fmt.Errorf("transcript must end with a user message (user×delta); got %s×%s", last.Source, last.Mutability)
	}
	var parts []string
	for _, block := range last.Content {
		if block.Type != transcript.BlockText {
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
	return prompt, entries[:len(entries)-1], nil
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

func namespaceProtocolMessageDelta(entries []sessions.ProtocolMessage, runID string) ([]sessions.ProtocolMessage, map[string]string) {
	local := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.ID != "" {
			local[entry.ID] = runID + ":" + entry.ID
		}
	}
	out := make([]sessions.ProtocolMessage, len(entries))
	for i, entry := range entries {
		if mapped, ok := local[entry.ID]; ok {
			entry.ID = mapped
		}
		if entry.ParentID != nil {
			if mapped, ok := local[*entry.ParentID]; ok {
				p := mapped
				entry.ParentID = &p
			} else {
				p := *entry.ParentID
				entry.ParentID = &p
			}
		}
		out[i] = entry
	}
	return out, local
}

func namespaceProtocolDelta(entries []sessions.ProtocolMessage, events []sessions.RuntimeEvent, runtimePrefix int, runID string) ([]sessions.ProtocolMessage, []sessions.RuntimeEvent) {
	outEntries, local := namespaceProtocolMessageDelta(entries, runID)
	if len(events) <= runtimePrefix {
		return outEntries, nil
	}
	tail := events[runtimePrefix:]
	outEvents := make([]sessions.RuntimeEvent, len(tail))
	for i, ev := range tail {
		if ev.AfterMessageID != "" {
			if mapped, ok := local[ev.AfterMessageID]; ok {
				ev.AfterMessageID = mapped
			}
		}
		outEvents[i] = ev
	}
	return outEntries, outEvents
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

func (a App) runContinue(ctx context.Context, agentID string, transport agentparams.Transport, request runtime.RunRequest, prompt string, transcriptStream *continueTranscriptStream, duplexCtrl *duplexController) (continueRun, error) {
	if err := nativedirs.CachedError(agentID); err != nil {
		return continueRun{}, err
	}
	mapped, err := agentparams.Build(agentID, transport, request)
	if err != nil {
		return continueRun{}, err
	}
	starter, ok := a.Runner.(runtime.StreamStarter)
	if !ok {
		return continueRun{}, fmt.Errorf("%s continue requires a stream runner", agentID)
	}
	plan := runtime.CommandPlan{Executable: mapped.Executable, Args: mapped.Args, Cwd: request.Cwd, Agent: agentID}
	if agentID == "agy" {
		plan.UnsetEnv = []string{hooks.SessionReportEnv}
		prepareAgyNativePlan("agy", &plan)
	}
	stream, err := starter.StartStream(plan)
	if err != nil {
		return continueRun{}, fmt.Errorf("%s execution failed: %s", agentID, runtime.RedactSecrets(err.Error()))
	}
	defer shutdownStream(stream)

	var handlers headless.Handlers
	if transcriptStream != nil {
		handlers.OnLine = func(line string) { _, _ = transcriptStream.Write([]byte(line + "\n")) }
		handlers.OnSession = transcriptStream.Wake
	}
	session, err := execution.Open(agentID, stream, mapped, handlers)
	if err != nil {
		return continueRun{}, err
	}
	failed := func(err error) error {
		if ctx.Err() != nil {
			return err
		}
		message := fmt.Sprintf("%s execution failed: %s", agentID, runtime.RedactSecrets(err.Error()))
		if stderr := runtime.RedactSecrets(strings.TrimSpace(stream.Stderr())); stderr != "" && !strings.Contains(err.Error(), "stderr:") {
			message += "; stderr: " + stderr
		}
		return errors.New(message)
	}
	sessionID, err := session.Start(ctx)
	if err != nil {
		return continueRun{}, failed(err)
	}
	if transcriptStream != nil && sessionID != "" {
		transcriptStream.Wake(sessionID)
	}
	var onReady func()
	if duplexCtrl != nil {
		onReady = func() { duplexCtrl.activate(session.Steer, session.Cancel) }
	}
	res, err := session.Turn(ctx, prompt, onReady)
	if duplexCtrl != nil {
		duplexCtrl.deactivate()
	}
	if err != nil {
		return continueRun{}, failed(err)
	}
	run := continueRun{sessionID: res.SessionID, completed: res.Completed, failure: res.Failure}
	if res.Cancelled {
		run.failure = "native turn cancelled"
	}
	return run, nil
}
