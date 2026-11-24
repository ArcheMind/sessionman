// Package headless is the one execution contract shared by every agent's native
// headless protocol, so `session continue` and ExternalSubagents run turns the same way.
package headless

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrExited marks a turn that ended because the native process died; the owner
// of the process reports that, not the turn.
var ErrExited = errors.New("exited")

const (
	Steered = "steered"
	Idle    = "idle"
)

type TurnResult struct {
	SessionID string
	Completed bool
	Cancelled bool
	Failure   string
	Text      string
	Usage     json.RawMessage
}

// Handlers receive native events from the client's reader goroutine, in order.
type Handlers struct {
	OnLine    func(line string)
	OnSession func(id string)
	OnExit    func()
	OnDebug   func(format string, args ...any)
}

type Session interface {
	// Start opens or resumes the native session; the returned ID is empty
	// when the native CLI only assigns one during the first turn.
	Start(ctx context.Context) (string, error)
	// Turn blocks until the turn has fully settled, including any native
	// background work the agent still owes a follow-up turn for. onReady runs
	// once the prompt is accepted, after which Steer and Cancel may be used.
	Turn(ctx context.Context, prompt string, onReady func()) (TurnResult, error)
	// Steer returns Steered when the message lands in the running turn and Idle
	// when no turn is running or it has already settled.
	Steer(ctx context.Context, message string) (string, error)
	Cancel() error
}
