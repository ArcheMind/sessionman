package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/drivers"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

type stubUsageRunner struct {
	results map[string]runtime.CommandResult
	rpcRaw  json.RawMessage
}

func (r *stubUsageRunner) Execute(_ context.Context, plan runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	if res, ok := r.results[plan.Executable+" "+strings.Join(plan.Args, " ")]; ok {
		return res, nil
	}
	if res, ok := r.results[plan.Executable]; ok {
		return res, nil
	}
	return runtime.CommandResult{}, nil
}

func (r *stubUsageRunner) ExecuteJSONRPC(_ context.Context, _ runtime.CommandPlan, reqs []json.RawMessage, _ json.RawMessage) (json.RawMessage, error) {
	for _, req := range reqs {
		if strings.Contains(string(req), "account/rateLimits/read") {
			return r.rpcRaw, nil
		}
	}
	return json.RawMessage(`{"id":2,"result":{"data":[]}}`), nil
}

func (r *stubUsageRunner) ExecuteClaudeControl(_ context.Context, _ runtime.CommandPlan, _ json.RawMessage, _ string) (json.RawMessage, error) {
	return json.RawMessage(`{"response":{"subtype":"success","response":{"models":[]}}}`), nil
}

func installStubBinaries(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestAuthUsageSingleAgentAndStatusUsage(t *testing.T) {
	installStubBinaries(t, "agy", "claude", "codex")

	runner := &stubUsageRunner{
		results: map[string]runtime.CommandResult{
			"claude -p --output-format json --max-turns 1 /usage": {
				Stdout: `{"is_error":false,"subtype":"success","result":"Current session: 10% used\nCurrent week (all models): 30% used · resets Nov 26 at 4:59am"}`,
			},
			"claude auth status --json": {
				Stdout: `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`,
			},
			"agy -p=/usage --output-format json": {
				Stdout: `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.95,"reset_time":"2026-11-25T18:08:53Z"}]}]}}}`,
			},
			"agy models": {
				Stdout: "gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n",
			},
			"codex login status": {
				Stdout: "Logged in using ChatGPT\n",
			},
		},
		rpcRaw: json.RawMessage(`{"id":2,"result":{"rateLimits":{"planType":"pro","primary":{"usedPercent":15,"windowDurationMins":10080,"resetsAt":1795813577}},"rateLimitResetCredits":{"availableCount":4}}}`),
	}

	var out bytes.Buffer
	application := App{Registry: drivers.NewRegistry(), Runner: runner, Stdout: &out, Stderr: &bytes.Buffer{}}
	if err := application.Run(context.Background(), []string{"--json", "auth", "usage", "claude"}); err != nil {
		t.Fatal(err)
	}
	var entry usageEntry
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal auth usage: %v\n%s", err, out.String())
	}
	if entry.Agent != "claude" || entry.Status != "ok" || len(entry.Groups) != 1 || entry.Groups[0].Windows[0].RemainingPercent != 90 {
		t.Fatalf("entry = %#v", entry)
	}

	out.Reset()
	if err := application.Run(context.Background(), []string{"status", "--usage"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"usage", "Five Hour Limit Remaining", "95% left (5% used)", "Current week (all models)", "70% left (30% used)", "reset credits: 4 available"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status --usage output missing %q:\n%s", want, text)
		}
	}
}
