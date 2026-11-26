package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ArcheMind/agentx/internal/runtime"
)

type usageEntry struct {
	Agent        string               `json:"agent" yaml:"agent"`
	Installed    bool                 `json:"installed" yaml:"installed"`
	Supported    bool                 `json:"supported" yaml:"supported"`
	Status       string               `json:"status" yaml:"status"`
	Subscription string               `json:"subscription,omitempty" yaml:"subscription,omitempty"`
	Groups       []runtime.UsageGroup `json:"groups,omitempty" yaml:"groups,omitempty"`
	ResetCredits *int                 `json:"reset_credits,omitempty" yaml:"reset_credits,omitempty"`
	Error        string               `json:"error,omitempty" yaml:"error,omitempty"`
}

func (a App) authUsage(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: sm [--json|--yaml] auth usage [agent]")
	}
	var targets []runtime.Agent
	if len(args) == 1 {
		agent, err := a.Registry.Get(args[0])
		if err != nil {
			return err
		}
		targets = []runtime.Agent{agent}
	} else {
		targets = a.Registry.All()
	}

	entries := make([]usageEntry, len(targets))
	var wg sync.WaitGroup
	for i, agent := range targets {
		wg.Add(1)
		go func(idx int, ag runtime.Agent) {
			defer wg.Done()
			entries[idx] = a.queryAgentUsageEntry(ctx, ag)
		}(i, agent)
	}
	wg.Wait()

	if len(args) == 1 {
		entry := entries[0]
		if a.Output != OutputText {
			return writeStructured(a.Stdout, entry, a.Output)
		}
		return writeUsageEntries(a.Stdout, entries, a.Color)
	}
	if a.Output != OutputText {
		return writeStructured(a.Stdout, entries, a.Output)
	}
	return writeUsageEntries(a.Stdout, entries, a.Color)
}

func (a App) queryAgentUsageEntry(ctx context.Context, agent runtime.Agent) usageEntry {
	entry := usageEntry{Agent: agent.ID}
	if _, err := a.agentPath(ctx, agent.Binary); err != nil {
		entry.Status = "not_installed"
		return entry
	}
	entry.Installed = true
	if agent.Usage == nil || !hasCapability(agent.Capabilities, runtime.CapabilityUsage) {
		entry.Status = "unsupported"
		return entry
	}
	entry.Supported = true
	report, err := agent.Usage.Usage(ctx, a.Runner)
	if err != nil {
		entry.Status = "error"
		entry.Error = err.Error()
		return entry
	}
	entry.Status = "ok"
	entry.Subscription = report.Subscription
	entry.Groups = report.Groups
	entry.ResetCredits = report.ResetCredits
	return entry
}

func writeUsageEntries(writer io.Writer, entries []usageEntry, color bool) error {
	for idx, entry := range entries {
		if idx > 0 {
			if _, err := fmt.Fprintln(writer); err != nil {
				return err
			}
		}
		header := colorize(color, ansiBold, entry.Agent)
		if entry.Subscription != "" {
			header += " " + colorize(color, ansiDim, "("+entry.Subscription+")")
		}
		switch entry.Status {
		case "not_installed":
			if _, err := fmt.Fprintf(writer, "%-10s %s\n", entry.Agent, colorize(color, ansiDim, "not installed")); err != nil {
				return err
			}
		case "unsupported":
			if _, err := fmt.Fprintf(writer, "%-10s %s\n", entry.Agent, colorize(color, ansiDim, "unsupported")); err != nil {
				return err
			}
		case "error":
			if _, err := fmt.Fprintf(writer, "%-10s %s\n", entry.Agent, colorize(color, ansiRed, "error: "+entry.Error)); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintln(writer, header); err != nil {
				return err
			}
			for _, g := range entry.Groups {
				showGroup := len(entry.Groups) > 1 || !strings.EqualFold(g.Name, entry.Agent)
				prefix := "  "
				if showGroup {
					if _, err := fmt.Fprintf(writer, "  %s\n", colorize(color, ansiCyan, g.Name)); err != nil {
						return err
					}
					prefix = "    "
				}
				for _, w := range g.Windows {
					line := formatUsageWindowLine(w, color)
					if _, err := fmt.Fprintf(writer, "%s%s\n", prefix, line); err != nil {
						return err
					}
				}
			}
			if entry.ResetCredits != nil {
				if _, err := fmt.Fprintf(writer, "  reset credits: %d available\n", *entry.ResetCredits); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func formatUsageWindowLine(w runtime.UsageWindow, color bool) string {
	label := w.Name
	if label == "" {
		label = w.Window
	}
	pct := fmt.Sprintf("%.0f%% left (%.0f%% used)", w.RemainingPercent, w.UsedPercent)
	pctColor := ansiGreen
	if w.RemainingPercent <= 15 {
		pctColor = ansiRed
	} else if w.RemainingPercent <= 40 {
		pctColor = ansiYellow
	}
	line := fmt.Sprintf("%-28s %s", label, colorize(color, pctColor, pct))
	if w.ResetsAt != "" {
		line += " " + colorize(color, ansiDim, "· resets "+w.ResetsAt)
	}
	return line
}
