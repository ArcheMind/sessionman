package hookmount

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

type Handler struct {
	Command string  `json:"command"`
	Matcher string  `json:"matcher,omitempty"`
	Timeout float64 `json:"timeout"`
}

// Handler is what the bridge executes; the default timeout is generous
// except for SessionEnd, whose portable window is short.
func (h Hook) Handler() Handler {
	timeout := h.timeout
	if timeout == 0 {
		timeout = 600
		if h.Event == "SessionEnd" {
			timeout = 3
		}
	}
	return Handler{Command: h.Command, Matcher: h.matcher, Timeout: timeout}
}

func IsPortableTarget(agent string) bool {
	switch agent {
	case "agy", "claude", "codex", "pi":
		return true
	default:
		return false
	}
}

// NativeEvent maps a portable (user configuration) event to an agent's own name.
func NativeEvent(agent, event string) (string, error) {
	mapping := map[string]map[string]string{
		"agy":    {"SessionStart": "SessionStart", "PreToolUse": "PreToolUse", "Stop": "Stop"},
		"claude": {"SessionStart": "SessionStart", "SessionEnd": "SessionEnd", "UserPromptSubmit": "UserPromptSubmit", "PreToolUse": "PreToolUse", "Stop": "Stop"},
		"codex":  {"SessionStart": "SessionStart", "SessionEnd": "SessionEnd", "UserPromptSubmit": "UserPromptSubmit", "PreToolUse": "PreToolUse", "Stop": "Stop"},
		"pi":     {"SessionStart": "session_start", "SessionEnd": "session_shutdown", "UserPromptSubmit": "input", "PreToolUse": "tool_call", "Stop": "agent_settled"},
	}
	agentEvents, ok := mapping[agent]
	if !ok {
		return "", fmt.Errorf("unsupported hook agent %q", agent)
	}
	native, ok := agentEvents[event]
	if !ok {
		return "", fmt.Errorf("unsupported portable hook event %q", event)
	}
	return native, nil
}

func BridgeCommand(executable, agent, event string, handler Handler) (string, error) {
	data, err := json.Marshal(handler)
	if err != nil {
		return "", fmt.Errorf("encode %s %s hook handler: %w", agent, event, err)
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	return shellExecutable(executable) + " hook __dispatch --agent " + agent + " --event " + event + " --handler " + payload, nil
}

func shellExecutable(value string) string {
	if runtime.GOOS != "windows" {
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func bridgeTimeout(handler Handler) int {
	seconds := int(handler.Timeout)
	if handler.Timeout > float64(seconds) {
		seconds++
	}
	return seconds
}

type nativeHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type nativeGroup struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   []nativeHandler `json:"hooks"`
}

// eventHandlers returns one bridge handler per configured hook, grouped by
// portable event in the fixed portable order.
func eventHandlers(executable, agent string, hooks []Hook) (map[string][]nativeHandler, error) {
	out := map[string][]nativeHandler{}
	for _, item := range hooks {
		if _, err := NativeEvent(agent, item.Event); err != nil {
			continue
		}
		handler := item.Handler()
		command, err := BridgeCommand(executable, agent, item.Event, handler)
		if err != nil {
			return nil, err
		}
		out[item.Event] = append(out[item.Event], nativeHandler{Type: "command", Command: command, Timeout: bridgeTimeout(handler)})
	}
	return out, nil
}

func nativeGroups(agent, executable string, hooks []Hook) (map[string][]nativeGroup, error) {
	handlers, err := eventHandlers(executable, agent, hooks)
	if err != nil {
		return nil, err
	}
	groups := map[string][]nativeGroup{}
	for event, list := range handlers {
		native, _ := NativeEvent(agent, event)
		for _, handler := range list {
			groups[native] = append(groups[native], nativeGroup{Hooks: []nativeHandler{handler}})
		}
	}
	return groups, nil
}

// CodexHooks is the content of a codex hooks.json.
func CodexHooks(executable string, hooks []Hook) ([]byte, error) {
	groups, err := nativeGroups("codex", executable, hooks)
	if err != nil {
		return nil, err
	}
	return encode(map[string]any{"description": "SessionMan portable hook bridge", "hooks": groups})
}

// ClaudeSettings is base (C's claude settings.json) with its hooks key
// replaced by the bridge hooks.
func ClaudeSettings(executable string, base []byte, hooks []Hook) ([]byte, error) {
	root := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(base))) > 0 {
		if err := json.Unmarshal(base, &root); err != nil || root == nil {
			return nil, fmt.Errorf("parse claude settings.json: not a JSON object: %v", err)
		}
	}
	groups, err := nativeGroups("claude", executable, hooks)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(groups)
	if err != nil {
		return nil, err
	}
	root["hooks"] = data
	return encode(root)
}

// AgyPlugin returns the files of sm's agy plugin. The MCP registration is
// always present; hooks.json only when an agy-supported hook is configured.
func AgyPlugin(executable string, hooks []Hook) (map[string][]byte, error) {
	handlers, err := eventHandlers(executable, "agy", hooks)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"plugin.json": []byte("{\"name\":\"sessionman\",\"version\":\"1.0.0\",\"description\":\"SessionMan native MCP and portable hooks\"}\n"),
	}
	mcp, err := encode(map[string]any{"mcpServers": map[string]any{"sessionman": map[string]any{"command": executable, "args": []string{"mcp", "__agy_serve"}}}})
	if err != nil {
		return nil, err
	}
	files["mcp_config.json"] = mcp
	if len(handlers) == 0 {
		return files, nil
	}
	events := map[string]any{}
	for event, list := range handlers {
		if event == "PreToolUse" {
			events[event] = []nativeGroup{{Matcher: "*", Hooks: list}}
		} else {
			events[event] = list
		}
	}
	data, err := encode(map[string]any{"sessionman": events})
	if err != nil {
		return nil, err
	}
	files["hooks.json"] = data
	return files, nil
}

func encode(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode hook configuration: %w", err)
	}
	return append(data, '\n'), nil
}

// PiExtension is the source of sm's generated pi extension.
func PiExtension(executable string, hooks []Hook) (string, error) {
	type spec struct {
		Payload string `json:"payload"`
	}
	configured := map[string][]spec{}
	for _, item := range hooks {
		data, err := json.Marshal(item.Handler())
		if err != nil {
			return "", err
		}
		configured[item.Event] = append(configured[item.Event], spec{Payload: base64.RawURLEncoding.EncodeToString(data)})
	}
	configuredData, err := json.Marshal(configured)
	if err != nil {
		return "", err
	}
	executableData, err := json.Marshal(executable)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(piTemplate, executableData, configuredData), nil
}

const piTemplate = `import { spawn } from "node:child_process";

const executable = %s;
const configured: Record<string, Array<{ payload: string }>> = %s;

export default function (pi: any) {
  let pendingContext: string[] = [];

  const visible = (ctx: any, message: string, kind = "info") => {
    if (!message) return;
    if (ctx.hasUI) ctx.ui.notify(message, kind);
    else console.error(message);
  };

  const invoke = async (event: string, payload: Record<string, unknown>, ctx: any, spec: { payload: string }) => {
    const result = await new Promise<{ code: number; stdout: string; stderr: string }>((resolve) => {
      const args = ["hook", "__dispatch", "--agent", "pi", "--event", event, "--handler", spec.payload];
      const child = spawn(executable, args, {
        cwd: ctx.cwd,
        stdio: ["pipe", "pipe", "pipe"],
      });
      let stdout = "";
      let stderr = "";
      child.stdout.on("data", (chunk) => stdout += chunk.toString());
      child.stderr.on("data", (chunk) => stderr += chunk.toString());
      child.on("error", (error) => resolve({ code: 1, stdout, stderr: String(error) }));
      child.on("close", (code) => resolve({ code: code ?? 1, stdout, stderr }));
      child.stdin.end(JSON.stringify(payload));
    });
    let output: any = {};
    if (result.stdout.trim()) {
      try { output = JSON.parse(result.stdout); }
      catch { output = { systemMessage: result.stdout.trim() }; }
    }
    if (result.stderr.trim()) console.error(result.stderr.trim());
    visible(ctx, output.systemMessage || "");
    return { ...result, output };
  };

  const isBlock = (result: any) => ["block", "deny"].includes(String(result.output?.decision || "").toLowerCase());

  const base = (nativeEvent: string, ctx: any) => ({
    hook_event_name: nativeEvent,
    session_id: ctx.sessionManager.getSessionId(),
    cwd: ctx.cwd,
    transcript_path: ctx.sessionManager.getSessionFile(),
    model: ctx.model?.id,
    agent: "pi",
  });

  if (configured.SessionStart?.length) {
    pi.on("session_start", async (event: any, ctx: any) => {
      const results = await Promise.all(configured.SessionStart.map((spec) => invoke("SessionStart", { ...base("session_start", ctx), reason: event.reason }, ctx, spec)));
      for (const result of results) {
        const context = result.output?.hookSpecificOutput?.additionalContext;
        if (context) pendingContext.push(context);
      }
    });
  }

  if (configured.SessionEnd?.length) {
    pi.on("session_shutdown", async (event: any, ctx: any) => {
      await Promise.all(configured.SessionEnd.map((spec) => invoke("SessionEnd", { ...base("session_shutdown", ctx), reason: event.reason }, ctx, spec)));
    });
  }

  if (configured.UserPromptSubmit?.length) {
    pi.on("input", async (event: any, ctx: any) => {
      const results = await Promise.all(configured.UserPromptSubmit.map((spec) => invoke("UserPromptSubmit", { ...base("input", ctx), text: event.text }, ctx, spec)));
      const blocked = results.find(isBlock);
      if (blocked) {
        visible(ctx, blocked.output?.reason || "Hook blocked the turn.", "error");
        return { action: "handled" };
      }
      if (results.some((result) => result.code !== 0)) visible(ctx, "Portable hook failed; the turn will continue.", "warning");
      for (const result of results) {
        const context = result.output?.hookSpecificOutput?.additionalContext;
        if (context) pendingContext.push(context);
      }
      return { action: "continue" };
    });
  }

  if (configured.PreToolUse?.length) {
    pi.on("tool_call", async (event: any, ctx: any) => {
      const results = await Promise.all(configured.PreToolUse.map((spec) => invoke("PreToolUse", { ...base("tool_call", ctx), tool_name: event.toolName, tool_input: event.input, tool_use_id: event.toolCallId }, ctx, spec)));
      const blocked = results.find(isBlock);
      if (blocked) return { block: true, reason: blocked.output?.reason || "Hook blocked the tool call." };
      if (results.some((result) => result.code !== 0)) visible(ctx, "Portable hook failed; the tool call will continue.", "warning");
    });
  }

  if (configured.SessionStart?.length || configured.UserPromptSubmit?.length) {
    pi.on("before_agent_start", async (_event: any, _ctx: any) => {
      if (pendingContext.length === 0) return;
      const content = pendingContext.join("\n");
      pendingContext = [];
      return { message: { customType: "sessionman-hooks", content, display: true } };
    });
  }

  if (configured.Stop?.length) {
    pi.on("agent_settled", async (_event: any, ctx: any) => {
      const branch = ctx.sessionManager.getBranch();
      const entry = [...branch].reverse().find((item: any) => item.type === "message" && item.message?.role === "assistant");
      const content = Array.isArray(entry?.message?.content)
        ? entry.message.content.filter((item: any) => item.type === "text").map((item: any) => item.text).join("\n")
        : "";
      await Promise.all(configured.Stop.map((spec) => invoke("Stop", { ...base("agent_settled", ctx), last_assistant_message: content, outcome: "completed" }, ctx, spec)));
    });
  }
}
`
