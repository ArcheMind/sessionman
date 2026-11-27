package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

type usageTestRunner struct {
	result runtime.CommandResult
	err    error
	plan   runtime.CommandPlan

	rpcRaw  json.RawMessage
	rpcErr  error
	rpcReqs []json.RawMessage
}

func (r *usageTestRunner) Execute(_ context.Context, plan runtime.CommandPlan, _ runtime.ExecuteOptions) (runtime.CommandResult, error) {
	r.plan = plan
	return r.result, r.err
}

func (r *usageTestRunner) ExecuteJSONRPC(_ context.Context, plan runtime.CommandPlan, reqs []json.RawMessage, _ json.RawMessage) (json.RawMessage, error) {
	r.plan = plan
	r.rpcReqs = reqs
	return r.rpcRaw, r.rpcErr
}

func TestClaudeUsageParsesNativeJSON(t *testing.T) {
	stdout := `{"is_error":false,"subtype":"success","local_command":"usage","result":"You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 0% used\nCurrent week (all models): 30% used · resets Nov 26 at 4:59am (America/Los_Angeles)\nCurrent week (Fable): 28% used · resets Nov 26 at 4:59am (America/Los_Angeles)"}`
	runner := &usageTestRunner{result: runtime.CommandResult{Stdout: stdout}}

	report, err := (ClaudeUsage{}).Usage(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.plan.Executable != "claude" || !reflect.DeepEqual(runner.plan.Args, []string{"-p", "--output-format", "json", "--max-turns", "1", "/usage"}) {
		t.Fatalf("plan = %#v", runner.plan)
	}
	if !report.Supported || report.Agent != "claude" || len(report.Groups) != 1 {
		t.Fatalf("report = %#v", report)
	}
	windows := report.Groups[0].Windows
	if len(windows) != 3 {
		t.Fatalf("windows = %#v", windows)
	}
	if windows[0].Name != "Current session" || windows[0].Window != "5h" || windows[0].UsedPercent != 0 || windows[0].RemainingPercent != 100 {
		t.Fatalf("window[0] = %#v", windows[0])
	}
	if windows[1].Window != "weekly" || windows[1].UsedPercent != 30 || windows[1].RemainingPercent != 70 || windows[1].ResetsAt != "Nov 26 at 4:59am (America/Los_Angeles)" {
		t.Fatalf("window[1] = %#v", windows[1])
	}
}

func TestAGYUsageParsesNativeJSON(t *testing.T) {
	stdout := `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.61802,"reset_time":"2026-11-30T20:03:11Z"},{"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.97572,"reset_time":"2026-11-25T18:08:53Z"}]},{"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":1,"reset_time":"2026-12-02T18:04:54Z"}]}]}}}`
	runner := &usageTestRunner{result: runtime.CommandResult{Stdout: stdout}}

	report, err := (AGYUsage{}).Usage(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.plan.Executable != "agy" || !reflect.DeepEqual(runner.plan.Args, []string{"-p=/usage", "--output-format", "json"}) {
		t.Fatalf("plan = %#v", runner.plan)
	}
	if !report.Supported || report.Agent != "agy" || len(report.Groups) != 2 {
		t.Fatalf("report = %#v", report)
	}
	first := report.Groups[0].Windows[0]
	if first.ID != "gemini-weekly" || first.Window != "weekly" || first.RemainingPercent != 61.8 || first.UsedPercent != 38.2 || first.ResetsAt != "2026-11-30T20:03:11Z" {
		t.Fatalf("first window = %#v", first)
	}
}

func TestCodexUsageParsesRateLimitsRPC(t *testing.T) {
	raw := `{"id":2,"result":{"rateLimits":{"limitId":"codex","planType":"self_serve_business_prolite","primary":{"usedPercent":15,"windowDurationMins":10080,"resetsAt":1795813577},"secondary":null},"rateLimitResetCredits":{"availableCount":4}}}`
	runner := &usageTestRunner{rpcRaw: json.RawMessage(raw)}

	report, err := (CodexUsage{}).Usage(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.plan.Executable != "codex" || !reflect.DeepEqual(runner.plan.Args, []string{"app-server", "--stdio"}) {
		t.Fatalf("plan = %#v", runner.plan)
	}
	if len(runner.rpcReqs) != 3 {
		t.Fatalf("rpcReqs = %d", len(runner.rpcReqs))
	}
	if !report.Supported || report.Subscription != "self_serve_business_prolite" || report.ResetCredits == nil || *report.ResetCredits != 4 {
		t.Fatalf("report = %#v", report)
	}
	w := report.Groups[0].Windows[0]
	if w.Window != "weekly" || w.UsedPercent != 15 || w.RemainingPercent != 85 || w.ResetsAt != "2026-11-27T21:06:17Z" {
		t.Fatalf("window = %#v", w)
	}
}

func TestUsageErrorsAreSurfaced(t *testing.T) {
	nativeErr := errors.New("boom")
	if _, err := (ClaudeUsage{}).Usage(context.Background(), &usageTestRunner{err: nativeErr}); !errors.Is(err, nativeErr) {
		t.Fatalf("claude err = %v", err)
	}
	if _, err := (AGYUsage{}).Usage(context.Background(), &usageTestRunner{result: runtime.CommandResult{Stdout: `{"status":"ERROR"}`}}); err == nil {
		t.Fatal("expected agy error")
	}
	if _, err := (CodexUsage{}).Usage(context.Background(), &usageTestRunner{rpcErr: nativeErr}); !errors.Is(err, nativeErr) {
		t.Fatalf("codex err = %v", err)
	}
}
