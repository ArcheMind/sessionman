package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
	"github.com/ArcheMind/sessionman/internal/presets"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

func TestAgyConvertSQLite(t *testing.T) {
	var binary bytes.Buffer
	application := App{Output: OutputJSON, Stdin: strings.NewReader(`{"id":"source","provider":"claude","workspace":"/tmp","messages":[{"id":"u","role":"user","content":[{"type":"text","text":"remember this"}]}]}`), Stdout: &binary}
	if err := application.convertTo("agy"); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(binary.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatal("convert output must be native SQLite")
	}
	var unified bytes.Buffer
	application.Stdin, application.Stdout = bytes.NewReader(binary.Bytes()), &unified
	if err := application.convertFrom("agy"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unified.String(), "remember this") {
		t.Fatal(unified.String())
	}
}

func TestAgyContinueNativePath(t *testing.T) {
	application := App{Sessions: sessions.Service{HomeDir: "/home/native"}}
	path, err := application.continueSessionFile("agy", "/workspace", "id")
	if err != nil || path != "/home/native/.gemini/antigravity-cli/conversations/id.db" {
		t.Fatalf("%s %v", path, err)
	}
}

func TestAgyMCPRequiresExplicitWorkspace(t *testing.T) {
	t.Setenv("SM_MCP_CWD", "")
	if err := (App{}).agyMCPServe(context.Background()); err == nil || !strings.Contains(err.Error(), "start agy through SessionMan") {
		t.Fatalf("%v", err)
	}
	plan := runtime.CommandPlan{Cwd: t.TempDir(), Env: map[string]string{"existing": "kept"}}
	prepareAgyNativePlan("agy", &plan)
	if plan.Env["SM_MCP_CWD"] != plan.Cwd || plan.Env["existing"] != "kept" {
		t.Fatalf("%+v", plan)
	}
}

func TestAgyCatalogPartialWarningKeepsUsableList(t *testing.T) {
	home := t.TempDir()
	valid := sessions.Session{Workspace: t.TempDir(), Messages: []sessions.Message{{ID: "u", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "usable transcript"}}}}}
	id, err := sessions.WriteAgySessionFile(valid, filepath.Join(home, ".gemini", "antigravity-cli"))
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", "broken.db")
	if err := os.WriteFile(bad, []byte("broken SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	application := New(false, strings.NewReader(""), &stdout, &stderr)
	application.Sessions = sessions.Service{HomeDir: home}
	if err := application.Run(context.Background(), []string{"--json", "session", "list", "--all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), id) || !strings.Contains(stderr.String(), "partial session catalog") || !strings.Contains(stderr.String(), "broken.db") {
		t.Fatalf("stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

type agyContinueRunner struct {
	home         string
	prompt       string
	plan         runtime.CommandPlan
	methods      []string
	modelSet     string
	beforeReturn func()
	failure      error
}

func (r *agyContinueRunner) Execute(_ context.Context, _ runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	return runtime.CommandResult{}, fmt.Errorf("agy continue must use ACP stream, not OneShot Execute")
}

func (r *agyContinueRunner) StartStream(plan runtime.CommandPlan) (*runtime.Stream, error) {
	r.plan = plan
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	send := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = io.WriteString(outW, string(raw)+"\n")
	}
	go func() {
		defer outW.Close()
		var sessionID string
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					SessionID string `json:"sessionId"`
					ModelID   string `json:"modelId"`
					Prompt    []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				} `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
			}
			if req.Method != "" {
				r.methods = append(r.methods, req.Method)
			}
			switch req.Method {
			case "initialize", "session/set_config_option":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			case "session/set_model":
				r.modelSet = req.Params.ModelID
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			case "session/new":
				detail := sessions.Session{Provider: "agy", Workspace: plan.Cwd}
				id, err := sessions.WriteAgySessionFile(detail, filepath.Join(r.home, ".gemini", "antigravity-cli"))
				if err != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
					continue
				}
				sessionID = id
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": sessionID}})
			case "session/load":
				sessionID = req.Params.SessionID
				// Replay historical chunk before load response to verify ACP discards it.
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "OLD"}}}})
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": sessionID}})
			case "session/prompt":
				if len(req.Params.Prompt) > 0 {
					r.prompt = req.Params.Prompt[0].Text
				}
				path := filepath.Join(r.home, ".gemini", "antigravity-cli", "conversations", sessionID+".db")
				detail, err := sessions.ParseAgySessionFile(path)
				if err != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
					continue
				}
				detail.Messages = append(detail.Messages,
					sessions.Message{ID: "echo", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: r.prompt}}},
					sessions.Message{ID: "answer", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "new answer"}}},
				)
				_ = os.Remove(path)
				if err := sessions.WriteAgyDatabaseFile(detail, path); err != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
					continue
				}
				if r.beforeReturn != nil {
					r.beforeReturn()
				}
				if r.failure != nil {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": r.failure.Error()}})
					continue
				}
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func TestAgyContinueReparsesProjectedHistoryPrefix(t *testing.T) {
	home, workspace := t.TempDir(), testWorkspace(t)
	source := sessions.Session{ID: "source", Provider: "claude", Workspace: workspace, Messages: []sessions.Message{
		{ID: "history", Role: "assistant", Content: []sessions.ContentBlock{{Type: "thinking", Text: "foreign reasoning", Signature: "foreign-signature"}, {Type: "text", Text: "historical answer"}}},
		{ID: "prompt", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "new prompt\nwith special \"quotes\""}}},
	}}
	input, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := New(false, bytes.NewReader(input), &stdout, &stderr)
	app.Sessions = sessions.Service{HomeDir: home}
	app.Hooks.Home = home
	runner := &agyContinueRunner{home: home}
	app.Runner = runner
	if err := app.Run(context.Background(), []string{"session", "continue", "agy", "--model", "native-future-model"}); err != nil {
		t.Fatal(err)
	}
	delta := decodeDelta(t, &stdout)
	if len(delta.Messages) != 1 || transcript.TextContent(delta.Messages[0]) != "new answer" || runner.prompt != "new prompt\nwith special \"quotes\"" || runner.plan.Env["SM_MCP_CWD"] != workspace {
		t.Fatalf("delta=%+v prompt=%q plan=%+v", delta, runner.prompt, runner.plan)
	}
	if runner.modelSet != "native-future-model" || len(runner.plan.Args) < 2 || runner.plan.Args[0] != "acp" || runner.plan.Args[1] != "--stdio" {
		t.Fatalf("expected ACP transport and set_model: model=%q args=%q", runner.modelSet, runner.plan.Args)
	}
	if !strings.Contains(strings.Join(runner.methods, ","), "session/load") {
		t.Fatalf("expected session/load on history continue, got %v", runner.methods)
	}
}

func TestAgyContinueACPUnlocksMCPAndUserConfigFalse(t *testing.T) {
	home, workspace := t.TempDir(), testWorkspace(t)
	source := sessions.Session{ID: "source", Provider: "harness", Workspace: workspace, Messages: []sessions.Message{
		{ID: "u1", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: "hello"}}},
	}}
	input, _ := json.Marshal(source)
	var stdout, stderr bytes.Buffer
	application := New(false, bytes.NewReader(input), &stdout, &stderr)
	application.Sessions = sessions.Service{HomeDir: home}
	application.Hooks.Home = home
	runner := &agyContinueRunner{home: home}
	application.Runner = runner
	mcpJSON := `{"mcpServers":{"tool":{"command":"/usr/bin/node","args":["s.mjs"]}}}`
	if err := application.Run(context.Background(), []string{"session", "continue", "agy", "--mcp-config", mcpJSON, "--user-config", "false", "--effort", "high"}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.plan.Args, " ")
	if !strings.Contains(args, "acp --stdio") || !strings.Contains(args, "--include-default-customizations=false") {
		t.Fatalf("unexpected ACP args: %q", args)
	}
	methods := strings.Join(runner.methods, ",")
	for _, want := range []string{"initialize", "session/new", "session/set_config_option", "session/prompt"} {
		if !strings.Contains(methods, want) {
			t.Fatalf("methods %q missing %q", methods, want)
		}
	}
}

func TestAgyWizardEffortIsAlwaysUnverifiedPerModel(t *testing.T) {
	draft := presetDraft{preset: presets.Preset{Agent: "agy", Model: "native-future-model"}, listed: &runtime.Model{ID: "native-future-model", Source: "agy models"}}
	note, choices := effortChoices(draft)
	if !strings.Contains(note, "unverified") || len(choices) != 6 {
		t.Fatalf("%s %+v", note, choices)
	}
	for _, choice := range choices[1:] {
		if choice.status != "unverified for this model" {
			t.Fatalf("%+v", choice)
		}
	}
}

func TestInjectAgySessionManLeasesAndUnlinksOnLastExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SM_CONFIG_DIR", "")
	linkPath := filepath.Join(home, ".gemini", "config", "plugins", "sessionman")

	release1, err := injectAgySessionMan(home, true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(linkPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at %s, got info=%v err=%v", linkPath, info, err)
	}
	mcpData, err := os.ReadFile(filepath.Join(linkPath, "mcp_config.json"))
	if err != nil || !strings.Contains(string(mcpData), `"__agy_serve"`) {
		t.Fatalf("mcp_config.json = %q, err=%v", mcpData, err)
	}
	if _, err := os.Stat(filepath.Join(linkPath, "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected hooks.json: %v", err)
	}

	// Second concurrent session keeps the link alive when the first exits.
	release2, err := injectAgySessionMan(home, true)
	if err != nil {
		t.Fatal(err)
	}
	// Stale dead-PID lease should be pruned on release.
	stale := filepath.Join(home, ".local", "state", "sessionman", "agy-plugin", "leases", "99999999-stale.lease")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	release1()
	if _, err := os.Lstat(linkPath); err != nil {
		t.Fatalf("link removed while second session still active: %v", err)
	}

	release2()
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("expected link unlinked after last session exited, got err=%v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("expected stale lease pruned, got err=%v", err)
	}
}

func TestInjectAgySessionManSkipsWhenConfigDirSetOrDryRun(t *testing.T) {
	home := t.TempDir()
	linkPath := filepath.Join(home, ".gemini", "config", "plugins", "sessionman")

	t.Setenv("SM_CONFIG_DIR", t.TempDir())
	release, err := injectAgySessionMan(home, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("expected no native link when SM_CONFIG_DIR is set, got %v", err)
	}

	t.Setenv("SM_CONFIG_DIR", "")
	release, err = injectAgySessionMan(home, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("expected no native link on dry-run, got %v", err)
	}
}

func TestInjectAgySessionManRefusesNonSessionManPlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SM_CONFIG_DIR", "")
	linkPath := filepath.Join(home, ".gemini", "config", "plugins", "sessionman")
	if err := os.MkdirAll(linkPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := injectAgySessionMan(home, true); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("expected refusal error, got %v", err)
	}
}
