package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/runtime"
	"github.com/ArcheMind/agentx/internal/sessions"
)

type trackedSessionRunner func(runtime.CommandPlan) (runtime.CommandResult, error)

func (run trackedSessionRunner) Execute(_ context.Context, plan runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	return run(plan)
}

func TestInteractiveCloseReportsStayWithTheirLaunch(t *testing.T) {
	a := App{Sessions: sessions.Service{HomeDir: t.TempDir()}, Stderr: io.Discard}
	leftReport := make(chan string, 1)
	releaseLeft := make(chan struct{})
	leftDone := make(chan error, 1)
	run := func(name, resumeID string) (runtime.CommandResult, error) {
		return a.executeTrackedSession(context.Background(), runtime.CommandPlan{Agent: "agy", Cwd: name}, runtime.ExecuteOptions{Interactive: true}, resumeID)
	}
	agentFailure := errors.New("native process exited with an error")
	var rightPath string
	a.Runner = trackedSessionRunner(func(plan runtime.CommandPlan) (runtime.CommandResult, error) {
		report := plan.Env[sessionReportEnv]
		if plan.Cwd == "left" {
			leftReport <- report
			<-releaseLeft
			for _, payload := range []string{
				`{"conversationId":"left-initial"}`,
				`{"conversationId":"left-selected"}`,
				`{"conversationId":"subagent","parentConversationId":"left-selected"}`,
			} {
				if err := recordSessionStart("agy", report, []byte(payload)); err != nil {
					return runtime.CommandResult{}, err
				}
			}
			return runtime.CommandResult{ExitCode: 8}, agentFailure
		}
		rightPath = report
		err := recordSessionStart("agy", report, []byte(`{"conversationId":"right-selected"}`))
		return runtime.CommandResult{}, err
	})

	go func() { _, err := run("left", "fallback"); leftDone <- err }()
	leftPath := <-leftReport
	defer func() {
		close(releaseLeft)
		if err := <-leftDone; !errors.Is(err, agentFailure) {
			t.Errorf("left run error = %v", err)
		}
		if closed, ok, err := a.Sessions.LatestClosed(); err != nil || !ok || closed.ID != "left-selected" {
			t.Errorf("last close after failed native exit = %#v, %t, %v", closed, ok, err)
		}
	}()
	if _, err := run("right", ""); err != nil {
		t.Fatal(err)
	}
	if leftPath == "" || rightPath == "" || leftPath == rightPath {
		t.Fatalf("launches share a report path: left %q, right %q", leftPath, rightPath)
	}
	if closed, ok, err := a.Sessions.LatestClosed(); err != nil || !ok || closed.ID != "right-selected" {
		t.Fatalf("last close before left exits = %#v, %t, %v", closed, ok, err)
	}
}

func TestInteractiveCloseUsesPiHeaderForResumeFallback(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "not-the-session-id.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session\",\"id\":\"native-id\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := App{Sessions: sessions.Service{HomeDir: home}, Stderr: io.Discard}
	plan := runtime.CommandPlan{Agent: "pi", Args: []string{"--session", path}}
	options := runtime.ExecuteOptions{Interactive: true}
	a.Runner = trackedSessionRunner(func(runtime.CommandPlan) (runtime.CommandResult, error) {
		return runtime.CommandResult{ExitCode: -1}, errors.New("startup failed")
	})
	_, _ = a.executeTrackedSession(context.Background(), plan, options, path)
	if closed, ok, err := a.Sessions.LatestClosed(); err != nil || ok {
		t.Fatalf("last close after startup failed = %#v, %t, %v", closed, ok, err)
	}
	a.Runner = trackedSessionRunner(func(plan runtime.CommandPlan) (runtime.CommandResult, error) {
		if len(plan.Args) < 3 || plan.Args[0] != "--extension" || plan.Args[2] != "--session" {
			t.Fatalf("Pi launch args = %v", plan.Args)
		}
		return runtime.CommandResult{}, nil
	})
	if _, err := a.executeTrackedSession(context.Background(), plan, options, path); err != nil {
		t.Fatal(err)
	}
	if closed, ok, err := a.Sessions.LatestClosed(); err != nil || !ok || closed.Agent != "pi" || closed.ID != "native-id" {
		t.Fatalf("last Pi close = %#v, %t, %v", closed, ok, err)
	}
}

func TestCodexSessionObserverPreservesExistingSessionStartHooks(t *testing.T) {
	original := `hooks={SessionStart=[{hooks=[{type="command",command="run user hook"}]}],state={"original-hook"={trusted_hash="sha256:user"}}}`
	args := []string{"resume", "existing-id", "--config=" + original, "--", "prompt"}
	projected, err := addCodexSessionObserver(args, "record close")
	if err != nil {
		t.Fatal(err)
	}
	config, end, err := codexHookCLIConfig(projected)
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := config["SessionStart"].([]any)
	if len(groups) != 2 {
		t.Fatalf("SessionStart groups = %#v", groups)
	}
	for index, want := range []string{"run user hook", "record close"} {
		group, _ := groups[index].(map[string]any)
		handlers, _ := group["hooks"].([]any)
		if len(handlers) != 1 {
			t.Fatalf("group %d handlers = %#v", index, handlers)
		}
		handler, _ := handlers[0].(map[string]any)
		if handler["command"] != want {
			t.Fatalf("group %d command = %q, want %q", index, handler["command"], want)
		}
	}
	states, _ := config["state"].(map[string]any)
	observer, _ := states[codexSessionObserverKey(1)].(map[string]any)
	originalState, _ := states["original-hook"].(map[string]any)
	const hash = "sha256:b071b1bc4d7f9138ed1f792168c37276b59b6c0fb05ec987c65c76707005ad2e"
	if len(states) != 2 || observer["trusted_hash"] != hash || originalState["trusted_hash"] != "sha256:user" {
		t.Fatalf("targeted hook trust = %#v", states)
	}
	if args[2] != "--config="+original || projected[end] != "--" || projected[end+1] != "prompt" {
		t.Fatalf("projection changed original or passthrough args: original %v, projected %v", args, projected)
	}
}

func TestOpenCodeInteractiveClosePrefersTUIRoute(t *testing.T) {
	t.Setenv("OPENCODE_TUI_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugin":["user-plugin"],"theme":"user-theme"}`)
	a := App{Sessions: sessions.Service{HomeDir: t.TempDir()}, Stderr: io.Discard}
	a.Runner = trackedSessionRunner(func(plan runtime.CommandPlan) (runtime.CommandResult, error) {
		var config struct {
			Plugin []string `json:"plugin"`
			Theme  string   `json:"theme"`
		}
		if err := json.Unmarshal([]byte(plan.Env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil || len(config.Plugin) != 2 || config.Plugin[0] != "user-plugin" || config.Theme != "user-theme" {
			t.Fatalf("OpenCode config = %#v, %v", config, err)
		}
		manifest := plan.Env["OPENCODE_TUI_CONFIG"]
		data, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &config); err != nil || len(config.Plugin) != 1 {
			t.Fatalf("OpenCode TUI config = %#v, %v", config, err)
		}
		pluginURL, err := url.Parse(config.Plugin[0])
		if err != nil || pluginURL.Scheme != "file" || !strings.HasSuffix(pluginURL.Path, "sessionman-tui-session.js") {
			t.Fatalf("OpenCode TUI plugin URL = %#v, %v", pluginURL, err)
		}
		root := filepath.Dir(manifest)
		if err := os.WriteFile(filepath.Join(root, "session-id"), []byte("server-session"), 0600); err != nil {
			t.Fatal(err)
		}
		return runtime.CommandResult{}, os.WriteFile(filepath.Join(root, openCodeRouteReport), []byte("tui-session"), 0600)
	})
	if _, err := a.executeTrackedSession(context.Background(), runtime.CommandPlan{Agent: "opencode"}, runtime.ExecuteOptions{Interactive: true}, "fallback"); err != nil {
		t.Fatal(err)
	}
	if closed, ok, err := a.Sessions.LatestClosed(); err != nil || !ok || closed.ID != "tui-session" {
		t.Fatalf("last OpenCode close = %#v, %t, %v", closed, ok, err)
	}
}
