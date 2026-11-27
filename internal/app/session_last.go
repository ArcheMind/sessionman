package app

import (
	"bufio"
	"context"
	"fmt"

	"github.com/ArcheMind/sessionman/internal/sessions"
)

func (a App) lastSession(ctx context.Context, args []string) error {
	if err := a.rejectStructured("last session"); err != nil {
		return err
	}
	var manual, fresh bool
	for _, arg := range args {
		switch arg {
		case "-m", "--manual":
			manual = true
		case "-t", "--trim":
			fresh = true
		default:
			return fmt.Errorf("usage: sm last [-m|--manual|-t|--trim]")
		}
	}
	if manual && fresh {
		return fmt.Errorf("--manual and --trim are mutually exclusive")
	}
	closed, ok, err := a.Sessions.LatestClosed()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no closed session recorded yet")
	}
	items, err := a.Sessions.List(sessions.ListOptions{
		Agent:            closed.Agent,
		All:              true,
		IncludeSubagents: true,
	})
	if err != nil {
		return err
	}
	var match *sessions.Summary
	for i := range items {
		if items[i].ID == closed.ID {
			match = &items[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf("last closed session %s/%s not found", closed.Agent, closed.ID)
	}
	detail, err := a.Sessions.Read(*match)
	if err != nil {
		return err
	}
	return a.resumeSessionInteractiveMode(ctx, bufio.NewReader(a.Stdin), detail, fresh, manual)
}
