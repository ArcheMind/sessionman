package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

const usageQueryTimeout = 15 * time.Second

// ClaudeUsage reads subscription usage via `claude -p --output-format json --max-turns 1 /usage`.
type ClaudeUsage struct{}

func (ClaudeUsage) Usage(ctx context.Context, runner runtime.Runner) (runtime.UsageReport, error) {
	commandCtx, cancel := context.WithTimeout(ctx, usageQueryTimeout)
	defer cancel()
	plan := runtime.CommandPlan{
		Executable: "claude",
		Args:       []string{"-p", "--output-format", "json", "--max-turns", "1", "/usage"},
		UnsetEnv:   []string{"CLAUDECODE"},
		Agent:      "claude",
	}
	res, execErr := runner.Execute(commandCtx, plan, runtime.ExecuteOptions{})
	if execErr != nil && strings.TrimSpace(res.Stdout) == "" {
		return runtime.UsageReport{}, fmt.Errorf("read Claude usage: %w", execErr)
	}
	return parseClaudeUsageOutput(res.Stdout, execErr)
}

var claudeUsageLinePattern = regexp.MustCompile(`^(.+?):\s*(\d+(?:\.\d+)?)%\s+used(?:\s*·\s*resets\s+(.+))?$`)

func parseClaudeUsageOutput(stdout string, execErr error) (runtime.UsageReport, error) {
	var envelope struct {
		IsError      bool   `json:"is_error"`
		Subtype      string `json:"subtype"`
		Result       string `json:"result"`
		LocalCommand string `json:"local_command"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		if execErr != nil {
			return runtime.UsageReport{}, fmt.Errorf("read Claude usage: %w", execErr)
		}
		return runtime.UsageReport{}, fmt.Errorf("parse Claude /usage JSON: %w", err)
	}
	if envelope.IsError || (envelope.Subtype != "" && envelope.Subtype != "success") {
		msg := strings.TrimSpace(envelope.Result)
		if msg == "" {
			msg = envelope.Subtype
		}
		return runtime.UsageReport{}, fmt.Errorf("Claude /usage failed: %s", msg)
	}

	var windows []runtime.UsageWindow
	for _, rawLine := range strings.Split(envelope.Result, "\n") {
		line := strings.TrimSpace(stripANSI(rawLine))
		if line == "" {
			continue
		}
		match := claudeUsageLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		used, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			continue
		}
		used = clampPercent(roundPercent(used))
		remaining := clampPercent(roundPercent(100 - used))
		name := strings.TrimSpace(match[1])
		resetsAt := strings.TrimSpace(match[3])
		windows = append(windows, runtime.UsageWindow{
			Name:             name,
			Window:           inferClaudeWindow(name),
			UsedPercent:      used,
			RemainingPercent: remaining,
			ResetsAt:         resetsAt,
		})
	}
	if len(windows) == 0 {
		return runtime.UsageReport{}, fmt.Errorf("parse Claude /usage: no usage windows found in native output")
	}
	return runtime.UsageReport{
		Agent:     "claude",
		Supported: true,
		Groups:    []runtime.UsageGroup{{Name: "Claude", Windows: windows}},
	}, nil
}

func inferClaudeWindow(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "session"):
		return "5h"
	case strings.Contains(lower, "week"):
		return "weekly"
	default:
		return ""
	}
}

// AGYUsage reads Antigravity usage via `agy -p=/usage --output-format json`.
type AGYUsage struct{}

func (AGYUsage) Usage(ctx context.Context, runner runtime.Runner) (runtime.UsageReport, error) {
	commandCtx, cancel := context.WithTimeout(ctx, usageQueryTimeout)
	defer cancel()
	plan := runtime.CommandPlan{
		Executable: "agy",
		Args:       []string{"-p=/usage", "--output-format", "json"},
		Agent:      "agy",
	}
	res, execErr := runner.Execute(commandCtx, plan, runtime.ExecuteOptions{})
	if execErr != nil && strings.TrimSpace(res.Stdout) == "" {
		return runtime.UsageReport{}, fmt.Errorf("read Antigravity usage: %w", execErr)
	}
	return parseAGYUsageOutput(res.Stdout, execErr)
}

func parseAGYUsageOutput(stdout string, execErr error) (runtime.UsageReport, error) {
	var payload struct {
		Status  string `json:"status"`
		Command struct {
			Name string `json:"name"`
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						ID                string  `json:"id"`
						Name              string  `json:"name"`
						Window            string  `json:"window"`
						RemainingFraction float64 `json:"remaining_fraction"`
						ResetTime         string  `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		if execErr != nil {
			return runtime.UsageReport{}, fmt.Errorf("read Antigravity usage: %w", execErr)
		}
		return runtime.UsageReport{}, fmt.Errorf("parse Antigravity /usage JSON: %w", err)
	}
	if payload.Status != "" && payload.Status != "SUCCESS" {
		return runtime.UsageReport{}, fmt.Errorf("Antigravity /usage failed with status %q", payload.Status)
	}
	groups := make([]runtime.UsageGroup, 0, len(payload.Command.Data.Groups))
	for _, g := range payload.Command.Data.Groups {
		windows := make([]runtime.UsageWindow, 0, len(g.Buckets))
		for _, b := range g.Buckets {
			remaining := clampPercent(roundPercent(b.RemainingFraction * 100))
			used := clampPercent(roundPercent(100 - remaining))
			windows = append(windows, runtime.UsageWindow{
				ID:               b.ID,
				Name:             b.Name,
				Window:           b.Window,
				UsedPercent:      used,
				RemainingPercent: remaining,
				ResetsAt:         b.ResetTime,
			})
		}
		if len(windows) > 0 {
			groups = append(groups, runtime.UsageGroup{Name: g.Name, Windows: windows})
		}
	}
	if len(groups) == 0 {
		return runtime.UsageReport{}, fmt.Errorf("parse Antigravity /usage: no usage groups found")
	}
	return runtime.UsageReport{
		Agent:     "agy",
		Supported: true,
		Groups:    groups,
	}, nil
}

// CodexUsage reads Codex rate limits via `codex app-server --stdio` JSON-RPC `account/rateLimits/read`.
type CodexUsage struct{}

func (CodexUsage) Usage(ctx context.Context, runner runtime.Runner) (runtime.UsageReport, error) {
	rpc, ok := runner.(runtime.JSONRPCRunner)
	if !ok {
		return runtime.UsageReport{}, fmt.Errorf("Codex usage discovery requires JSON-RPC command support")
	}
	commandCtx, cancel := context.WithTimeout(ctx, usageQueryTimeout)
	defer cancel()
	requests := []json.RawMessage{
		json.RawMessage(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"sessionman","title":"SessionMan","version":"0"},"capabilities":{"experimentalApi":false}}}`),
		json.RawMessage(`{"method":"initialized"}`),
		json.RawMessage(`{"id":2,"method":"account/rateLimits/read"}`),
	}
	raw, err := rpc.ExecuteJSONRPC(commandCtx, runtime.CommandPlan{Executable: "codex", Args: []string{"app-server", "--stdio"}, Agent: "codex"}, requests, json.RawMessage(`2`))
	if err != nil {
		return runtime.UsageReport{}, fmt.Errorf("read Codex rate limits: %w", err)
	}
	return parseCodexRateLimitsResponse(raw)
}

type codexRateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

func parseCodexRateLimitsResponse(raw []byte) (runtime.UsageReport, error) {
	var response struct {
		Result *struct {
			RateLimits *struct {
				LimitID   string                `json:"limitId"`
				PlanType  string                `json:"planType"`
				Primary   *codexRateLimitWindow `json:"primary"`
				Secondary *codexRateLimitWindow `json:"secondary"`
			} `json:"rateLimits"`
			RateLimitResetCredits *struct {
				AvailableCount int `json:"availableCount"`
			} `json:"rateLimitResetCredits"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return runtime.UsageReport{}, fmt.Errorf("parse Codex account/rateLimits/read response: %w", err)
	}
	if response.Error != nil {
		return runtime.UsageReport{}, fmt.Errorf("Codex account/rateLimits/read failed (%d): %s", response.Error.Code, response.Error.Message)
	}
	if response.Result == nil || response.Result.RateLimits == nil {
		return runtime.UsageReport{}, fmt.Errorf("Codex account/rateLimits/read returned no rateLimits")
	}

	var windows []runtime.UsageWindow
	for _, w := range []*codexRateLimitWindow{response.Result.RateLimits.Primary, response.Result.RateLimits.Secondary} {
		if w == nil {
			continue
		}
		used := clampPercent(roundPercent(w.UsedPercent))
		remaining := clampPercent(roundPercent(100 - used))
		windowTag, displayName := formatCodexWindowName(w.WindowDurationMins)
		var resetsAt string
		if w.ResetsAt > 0 {
			resetsAt = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		windows = append(windows, runtime.UsageWindow{
			Name:             displayName,
			Window:           windowTag,
			UsedPercent:      used,
			RemainingPercent: remaining,
			ResetsAt:         resetsAt,
		})
	}
	if len(windows) == 0 {
		return runtime.UsageReport{}, fmt.Errorf("Codex account/rateLimits/read returned no usage windows")
	}
	var resetCredits *int
	if response.Result.RateLimitResetCredits != nil {
		count := response.Result.RateLimitResetCredits.AvailableCount
		resetCredits = &count
	}
	return runtime.UsageReport{
		Agent:        "codex",
		Supported:    true,
		Subscription: response.Result.RateLimits.PlanType,
		Groups:       []runtime.UsageGroup{{Name: "Codex", Windows: windows}},
		ResetCredits: resetCredits,
	}, nil
}

func formatCodexWindowName(mins int) (string, string) {
	switch mins {
	case 300:
		return "5h", "5-Hour Limit"
	case 10080:
		return "weekly", "Weekly Limit"
	default:
		if mins > 0 && mins%60 == 0 {
			h := mins / 60
			return fmt.Sprintf("%dh", h), fmt.Sprintf("%d-Hour Limit", h)
		}
		return fmt.Sprintf("%dm", mins), fmt.Sprintf("%d-Min Limit", mins)
	}
}

func roundPercent(v float64) float64 {
	return math.Round(v*10) / 10
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
