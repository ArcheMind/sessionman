package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/ArcheMind/sessionman/internal/hooks"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/tailscale/hujson"
)

const (
	sessionReportEnv    = hooks.SessionReportEnv
	openCodeRouteReport = "opencode-route-session-id"
)

func (a App) executeTrackedSession(ctx context.Context, plan runtime.CommandPlan, options runtime.ExecuteOptions, sessionID string) (runtime.CommandResult, error) {
	switch plan.Agent {
	case "agy", "claude", "codex", "opencode", "pi":
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
	if plan.Agent == "opencode" {
		routeReport := filepath.Join(dir, openCodeRouteReport)
		if _, err := os.Stat(routeReport); err == nil {
			report = routeReport
		}
	}
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
	case "opencode":
		path := filepath.Join(filepath.Dir(report), "sessionman-session.js")
		if err := os.WriteFile(path, []byte(sessionObserverModule(report)+openCodeSessionObserver), 0600); err != nil {
			return plan, err
		}
		if err := addOpenCodeSessionObserver(&plan, path); err != nil {
			return plan, err
		}
		if err := prepareOpenCodeTUIObserver(&plan, filepath.Dir(report)); err != nil {
			return plan, err
		}
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

func addOpenCodeSessionObserver(plan *runtime.CommandPlan, path string) error {
	const configEnv = "OPENCODE_CONFIG_CONTENT"
	config := make(map[string]json.RawMessage)
	var plugins []json.RawMessage
	data, set := plan.Env[configEnv]
	if !set {
		data = os.Getenv(configEnv)
	}
	if data != "" {
		standard, err := hujson.Standardize([]byte(data))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(standard, &config); err != nil {
			return err
		}
		if raw := config["plugin"]; raw != nil {
			if err := json.Unmarshal(raw, &plugins); err != nil {
				return err
			}
		}
	}
	plugins = append(plugins, json.RawMessage(jsonString(localPluginURL(path))))
	if config == nil {
		config = make(map[string]json.RawMessage)
	}
	config["plugin"], _ = json.Marshal(plugins)
	content, err := json.Marshal(config)
	if err != nil {
		return err
	}
	plan.Env = withPlanEnv(plan.Env, configEnv, string(content))
	return nil
}

func prepareOpenCodeTUIObserver(plan *runtime.CommandPlan, root string) error {
	const env = "OPENCODE_TUI_CONFIG"
	original, set := plan.Env[env]
	if !set {
		original = os.Getenv(env)
	}
	config := make(map[string]any)
	if original != "" {
		data, err := os.ReadFile(original)
		if err != nil {
			return err
		}
		standard, err := hujson.Standardize(data)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(standard, &config); err != nil {
			return err
		}
		if config == nil {
			config = make(map[string]any)
		}
		base, err := filepath.Abs(filepath.Dir(original))
		if err != nil {
			return err
		}
		preserveConfigFileReferences(config, base)
		if plugins, ok := config["plugin"].([]any); ok {
			for index, plugin := range plugins {
				if spec, ok := plugin.(string); ok && strings.HasPrefix(spec, ".") {
					plugins[index] = localPluginURL(filepath.Join(base, spec))
				}
			}
		}
	}
	plugin := filepath.Join(root, "sessionman-tui-session.js")
	if err := os.WriteFile(plugin, []byte(sessionObserverModule(filepath.Join(root, openCodeRouteReport))+openCodeTUIObserver), 0600); err != nil {
		return err
	}
	plugins, _ := config["plugin"].([]any)
	config["plugin"] = append(plugins, localPluginURL(plugin))
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "tui.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	plan.Env = withPlanEnv(plan.Env, env, path)
	return nil
}

var configFileReference = regexp.MustCompile(`\{file:([^}]+)\}`)

func preserveConfigFileReferences(config map[string]any, base string) {
	var rewrite func(any) any
	rewrite = func(value any) any {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				value[key] = rewrite(item)
			}
		case []any:
			for index, item := range value {
				value[index] = rewrite(item)
			}
		case string:
			return configFileReference.ReplaceAllStringFunc(value, func(reference string) string {
				path := strings.TrimSuffix(strings.TrimPrefix(reference, "{file:"), "}")
				if filepath.IsAbs(path) || strings.HasPrefix(path, "~/") {
					return reference
				}
				return "{file:" + filepath.ToSlash(filepath.Join(base, path)) + "}"
			})
		}
		return value
	}
	rewrite(config)
}

func localPluginURL(path string) string {
	filePath := filepath.ToSlash(path)
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	return (&url.URL{Scheme: "file", Path: filePath}).String()
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

const openCodeSessionObserver = `
export const SessionMan = async ({ client }) => {
  let generation = 0;
  return {
    event: async ({ event }) => {
      if (event.type !== "session.created") return;
      const session = event.properties.info;
      if (session.parentID) return;
      generation++;
      rememberSession(session.id);
    },
    "chat.message": async ({ sessionID }) => {
      const request = ++generation;
      try {
        const { data: session } = await client.session.get({ path: { id: sessionID } });
        if (request === generation && session && !session.parentID) rememberSession(session.id);
      } catch {
      }
    },
  };
};
`

const openCodeTUIObserver = `
export default {
  id: "sessionman-session",
  tui: async (api) => {
    api.lifecycle.onDispose(() => {
      const route = api.route.current;
      if (route.name !== "session") {
        rememberSession("");
        return;
      }
      const session = api.state.session.get(route.params?.sessionID);
      if (session && !session.parentID) rememberSession(session.id);
    });
  },
};
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
