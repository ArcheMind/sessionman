package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ArcheMind/agentx/internal/app"
	"github.com/ArcheMind/agentx/internal/nativedirs"
	"github.com/ArcheMind/agentx/internal/update"
)

func main() {
	args := os.Args[1:]
	if handled, err := update.RunHelper(args, os.Stdout, os.Stderr); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	bare := len(args) == 0
	debug := os.Getenv("SM_LOG") == "debug"
	appArgs := make([]string, 0, len(args))
	for len(args) > 0 {
		switch args[0] {
		case "--verbose":
			debug = true
		case "--json", "--yaml":
			appArgs = append(appArgs, args[0])
		case "--session-dir":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "--session-dir requires an absolute path")
				os.Exit(1)
			}
			if !filepath.IsAbs(args[1]) {
				fmt.Fprintf(os.Stderr, "--session-dir must be an absolute path, got %q\n", args[1])
				os.Exit(1)
			}
			os.Setenv("SM_SESSION_DIR", args[1])
			args = args[1:]
		case "--config-dir":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "--config-dir requires an absolute path")
				os.Exit(1)
			}
			if !filepath.IsAbs(args[1]) {
				fmt.Fprintf(os.Stderr, "--config-dir must be an absolute path, got %q\n", args[1])
				os.Exit(1)
			}
			os.Setenv("SM_CONFIG_DIR", args[1])
			args = args[1:]
		default:
			appArgs = append(appArgs, args...)
			args = nil
			continue
		}
		args = args[1:]
	}
	args = appArgs
	if err := nativedirs.ApplyToProcess(nativedirs.FromEnv()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	application := app.New(debug, os.Stdin, os.Stdout, os.Stderr)
	if bare {
		stop, err := application.CheckStartupUpdate(context.Background(), debug)
		if err != nil {
			_ = app.WriteError(os.Stderr, args, err)
			os.Exit(app.ExitCode(err))
		}
		if stop {
			return
		}
	}
	if err := application.Run(context.Background(), args); err != nil {
		_ = app.WriteError(os.Stderr, args, err)
		os.Exit(app.ExitCode(err))
	}
}
