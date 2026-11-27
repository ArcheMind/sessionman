package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArcheMind/sessionman/internal/sessions"
)

func TestLastSessionValidation(t *testing.T) {
	home := t.TempDir()
	application := App{
		Sessions: sessions.Service{HomeDir: home},
		Stdin:    strings.NewReader(""),
		Stdout:   &strings.Builder{},
		Stderr:   &strings.Builder{},
	}
	if err := application.Run(context.Background(), []string{"last", "-m", "-t"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
	if err := application.Run(context.Background(), []string{"last", "--bogus"}); err == nil || !strings.Contains(err.Error(), "usage: sm last") {
		t.Fatalf("expected usage error, got %v", err)
	}
	if err := application.Run(context.Background(), []string{"--json", "last"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected structured output error, got %v", err)
	}
	if err := application.Run(context.Background(), []string{"last"}); err == nil || !strings.Contains(err.Error(), "no closed session") {
		t.Fatalf("expected no closed session error, got %v", err)
	}
}

func TestLastSessionManualAndTrim(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		stdin      string
		wantPrompt string
	}{
		{
			name:       "manual",
			args:       []string{"last", "-m"},
			stdin:      "\r\r\r\r",
			wantPrompt: "Manual session load · 8 turns",
		},
		{
			name:       "trim",
			args:       []string{"last", "-t"},
			stdin:      "\r\r\r",
			wantPrompt: "Resume session as:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application, stdout, runner := presetApp(t, tc.stdin, "codex")
			runner.response = `{"id":2,"result":{"data":[{"id":"gpt-5","model":"gpt-5"}]}}`
			application.Output = OutputText
			home := t.TempDir()
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			application.Sessions = sessions.Service{HomeDir: home, LookPath: func(string) (string, error) { return "/usr/bin/codex", nil }}
			workspace := t.TempDir()

			var msgs []sessions.Message
			for i := 1; i <= 8; i++ {
				msgs = append(msgs,
					sessions.Message{ID: fmt.Sprintf("u%d", i), Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: fmt.Sprintf("turn-%d", i)}}},
					sessions.Message{ID: fmt.Sprintf("a%d", i), Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: fmt.Sprintf("reply-%d", i)}}},
				)
			}
			id, err := sessions.WriteCodexSessionFile(sessions.Session{
				ID:        "orig",
				Provider:  "codex",
				Workspace: workspace,
				StartedAt: "2026-11-27T00:00:00Z",
				UpdatedAt: "2026-11-27T00:01:00Z",
				Messages:  msgs,
			}, codexHome, "openai")
			if err != nil {
				t.Fatal(err)
			}
			if err := application.Sessions.SaveClosed("codex", id, time.Now()); err != nil {
				t.Fatal(err)
			}

			if err := application.Run(context.Background(), tc.args); err != nil {
				t.Fatalf("sm %v failed: %v\nstdout: %s", tc.args, err, stdout.String())
			}
			if !strings.Contains(stdout.String(), tc.wantPrompt) {
				t.Fatalf("stdout missing %q:\n%s", tc.wantPrompt, stdout.String())
			}
			if len(runner.plans) != 1 {
				t.Fatalf("expected 1 launched plan, got %d", len(runner.plans))
			}
			if tc.name == "trim" {
				files, _ := filepath.Glob(filepath.Join(home, ".local", "state", "sessionman", "history", "offload-*.yaml"))
				if len(files) != 1 {
					t.Fatalf("expected 1 offload yaml file, got %v", files)
				}
			}
		})
	}
}
