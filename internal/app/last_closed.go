package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/ArcheMind/sessionman/internal/hooks"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

const (
	sessionReportEnv = hooks.SessionReportEnv
)

func (a App) executeTrackedSession(ctx context.Context, plan runtime.CommandPlan, options runtime.ExecuteOptions, sessionID string) (runtime.CommandResult, error) {
	switch plan.Agent {
	case "agy", "claude", "codex", "pi":
	default:
		return a.Runner.Execute(ctx, plan, options)
	}
	sessionID = resumeSessionID(plan.Agent, sessionID)
	dir, err := os.MkdirTemp("", "sessionman-session-")
	if err != nil {
		a.logSessionTrackingError(err)
		return a.Runner.Execute(ctx, plan, options)
	}
	defer os.RemoveAll(dir)
	report := filepath.Join(dir, "session-id")
	if observed, err := observeSession(plan, report); err == nil {
		plan = observed
	} else {
		a.logSessionTrackingError(err)
	}
	result, runErr := a.Runner.Execute(ctx, plan, options)
	closedAt := time.Now()
	if data, err := os.ReadFile(report); err == nil {
		sessionID = strings.TrimSpace(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		a.logSessionTrackingError(err)
		sessionID = ""
	} else if runErr != nil {
		sessionID = ""
	}
	if sessionID != "" {
		a.logSessionTrackingError(a.Sessions.SaveClosed(plan.Agent, sessionID, closedAt))
	}
	return result, runErr
}

func resumeSessionID(agent, token string) string {
	if agent != "pi" || !strings.HasSuffix(token, ".jsonl") {
		return token
	}
	file, err := os.Open(token)
	if err != nil {
		return ""
	}
	defer file.Close()
	line := bufio.NewScanner(file)
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if !line.Scan() || json.Unmarshal(line.Bytes(), &header) != nil || header.Type != "session" {
		return ""
	}
	return header.ID
}

func (a App) logSessionTrackingError(err error) {
	if err != nil && (a.debug || os.Getenv("SM_LOG") == "debug") {
		fmt.Fprintf(a.Stderr, "sm: track interactive session: %v\n", err)
	}
}

func observeSession(plan runtime.CommandPlan, report string) (runtime.CommandPlan, error) {
	switch plan.Agent {
	case "agy":
		plan.Env = withPlanEnv(plan.Env, sessionReportEnv, report)
	case "claude", "codex":
		executable, err := os.Executable()
		if err != nil {
			return plan, err
		}
		command := shellHookArg(executable) + " hook __session_start " + plan.Agent + " " + shellHookArg(report)
		if plan.Agent == "codex" {
			plan.Args, err = addCodexSessionObserver(plan.Args, command)
			return plan, err
		}
		root := filepath.Dir(report)
		if err := writeClaudeSessionObserver(root, command); err != nil {
			return plan, err
		}
		plan.Args = append([]string{"--plugin-dir", root}, plan.Args...)
	case "pi":
		path := filepath.Join(filepath.Dir(report), "sessionman-session.ts")
		if err := os.WriteFile(path, []byte(sessionObserverModule(report)+piSessionObserver), 0600); err != nil {
			return plan, err
		}
		plan.Args = append([]string{"--extension", path}, plan.Args...)
	}
	return plan, nil
}

func withPlanEnv(env map[string]string, name, value string) map[string]string {
	merged := make(map[string]string, len(env)+1)
	for key, item := range env {
		merged[key] = item
	}
	merged[name] = value
	return merged
}

func shellHookArg(value string) string {
	if goruntime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeClaudeSessionObserver(root, command string) error {
	for _, subdir := range []string{".claude-plugin", "hooks"} {
		if err := os.Mkdir(filepath.Join(root, subdir), 0700); err != nil {
			return err
		}
	}
	manifest := filepath.Join(root, ".claude-plugin", "plugin.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"sessionman-session"}`), 0600); err != nil {
		return err
	}
	config := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%s}]}]}}`, jsonString(command))
	return os.WriteFile(filepath.Join(root, "hooks", "hooks.json"), []byte(config), 0600)
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func sessionObserverModule(report string) string {
	return `import { randomUUID } from "node:crypto";
import { renameSync, rmSync, writeFileSync } from "node:fs";
const report = ` + jsonString(report) + `;
function rememberSession(id) {
  if (typeof id !== "string") return;
  const staged = report + "." + randomUUID();
  try {
    writeFileSync(staged, id, { mode: 0o600, flag: "wx" });
    renameSync(staged, report);
  } catch {
    rmSync(staged, { force: true });
  }
}
`
}

const piSessionObserver = `
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    rememberSession(ctx.sessionManager.getSessionId());
  });
}
`

func (a App) captureSessionStart(args []string) error {
	if len(args) != 2 || (args[0] != "claude" && args[0] != "codex") {
		return fmt.Errorf("invalid session start callback")
	}
	payload, err := io.ReadAll(a.Stdin)
	if err != nil {
		return err
	}
	return recordSessionStart(args[0], args[1], payload)
}

func recordSessionStart(agent, report string, payload []byte) error {
	event, err := hooks.Decode(agent, "SessionStart", payload)
	if err != nil {
		return err
	}
	if event.SessionID == "" || event.ParentSessionID != "" {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(report), "session-id-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := io.WriteString(file, event.SessionID)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), report)
}
