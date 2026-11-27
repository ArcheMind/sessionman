package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ArcheMind/sessionman/internal/hooks"
)

func (a App) hook(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "__session_start" {
		return a.captureSessionStart(args[1:])
	}
	if len(args) > 0 && args[0] == "__dispatch" {
		return a.dispatchHook(ctx, args[1:])
	}
	if len(args) != 1 || args[0] != "list" {
		return fmt.Errorf("usage: sm [--json|--yaml] hook list")
	}
	result, err := a.Hooks.List()
	if err != nil {
		return err
	}
	if a.Output != OutputText {
		return writeStructured(a.Stdout, result, a.Output)
	}
	if len(result.Hooks) == 0 {
		fmt.Fprintln(a.Stdout, "No portable hooks configured.")
	} else {
		fmt.Fprintln(a.Stdout, "SCOPE    EVENT             COMMAND")
		for _, item := range result.Hooks {
			fmt.Fprintf(a.Stdout, "%-8s %-17s %s\n", item.Scope, item.Event, item.Command)
		}
	}
	if len(result.Warnings) > 0 && len(result.Hooks) > 0 {
		fmt.Fprintln(a.Stdout)
	}
	for _, warning := range result.Warnings {
		if warning.Event == "" {
			fmt.Fprintf(a.Stdout, "warning: %s\n", warning.Message)
		} else {
			fmt.Fprintf(a.Stdout, "warning: %s %s\n", warning.Event, warning.Message)
		}
	}
	return nil
}

func (a App) dispatchHook(ctx context.Context, args []string) error {
	agent := ""
	event := ""
	handler := hooks.Handler{}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--agent":
			index++
			if index >= len(args) {
				return errors.New("--agent requires a value")
			}
			agent = args[index]
		case "--event":
			index++
			if index >= len(args) {
				return errors.New("--event requires a value")
			}
			event = args[index]
		case "--handler":
			index++
			if index >= len(args) {
				return errors.New("--handler requires a value")
			}
			data, decodeErr := base64.RawURLEncoding.DecodeString(args[index])
			if decodeErr != nil {
				return errors.New("--handler must be a valid SessionMan hook payload")
			}
			if decodeErr := json.Unmarshal(data, &handler); decodeErr != nil {
				return errors.New("--handler must be a valid SessionMan hook payload")
			}
		default:
			return fmt.Errorf("unknown hook dispatch option %q", args[index])
		}
	}
	if agent == "" || event == "" || strings.TrimSpace(handler.Command) == "" || handler.Timeout <= 0 {
		return errors.New("hook dispatch requires --agent, --event, and a valid --handler")
	}
	payload, err := io.ReadAll(a.Stdin)
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	result, err := hooks.DispatchOne(ctx, a.Runner, agent, event, payload, handler)
	if err != nil {
		if result.ExitCode > 0 {
			message := strings.TrimSpace(result.Stderr)
			if message == "" {
				message = err.Error()
			}
			return &ExitError{Code: result.ExitCode, Err: errors.New(message)}
		}
		return err
	}
	if result.Stderr != "" {
		fmt.Fprintln(a.Stderr, result.Stderr)
	}
	if result.Stdout != "" {
		fmt.Fprint(a.Stdout, result.Stdout)
	}
	return nil
}
