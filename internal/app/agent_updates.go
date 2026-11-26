package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ArcheMind/agentx/internal/agentupdate"
	"github.com/ArcheMind/agentx/internal/clibin"
	"github.com/ArcheMind/agentx/internal/runtime"
)

func (a App) agentUpdates(ctx context.Context, args []string, update bool) error {
	usage := fmt.Errorf("usage: sm [--json|--yaml] agent updates <agent> [--install-root <dir>]; sm [--json|--yaml] agent update <agent> --install-root <dir>")
	if len(args) == 0 {
		return usage
	}
	agent, root := args[0], ""
	if len(args) > 1 {
		if len(args) != 3 || args[1] != "--install-root" || args[2] == "" {
			return usage
		}
		root = args[2]
	}
	service := agentupdate.Service{InstallRoot: root, Runner: a.Runner, Debug: a.debug, Log: a.Stderr}
	var report agentupdate.Report
	var err error
	if update {
		report, err = service.Update(ctx, agent)
	} else {
		report = service.Check(ctx, agent)
	}
	if a.Output != OutputText {
		if writeErr := writeStructured(a.Stdout, report, a.Output); writeErr != nil {
			return writeErr
		}
	} else {
		fmt.Fprintf(a.Stdout, "%s: %s (installed: %s; latest stable: %s)\n", agent, report.Status, report.CurrentVersion, report.LatestVersion)
		if report.Error != "" {
			fmt.Fprintln(a.Stdout, report.Error)
		}
	}
	return err
}

func (a App) agentPath(ctx context.Context, binary string) (string, error) {
	candidate, err := clibin.Resolve(ctx, binary, "", func(path, output string, err error) {
		if !a.debug {
			return
		}
		entry := map[string]any{"event": "external_call", "input": map[string]any{"executable": path, "args": []string{"--version"}}, "output": runtime.RedactSecrets(output)}
		if err != nil {
			entry["error"] = runtime.RedactSecrets(err.Error())
		}
		encoded, _ := json.Marshal(entry)
		fmt.Fprintln(a.Stderr, string(encoded))
	})
	return candidate.Path, err
}
