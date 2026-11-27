package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

// continueStreamWriter serializes NDJSON stdout writes across concurrent
// transcript streaming and duplex control acknowledgements, and ensures no
// event is ever emitted after the terminal result or stream closure.
type continueStreamWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func newContinueStreamWriter(w io.Writer) *continueStreamWriter {
	return &continueStreamWriter{w: w}
}

func (w *continueStreamWriter) WriteEvent(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	return json.NewEncoder(w.w).Encode(v)
}

func (w *continueStreamWriter) WriteTerminal(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return json.NewEncoder(w.w).Encode(v)
}

func (w *continueStreamWriter) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

type duplexControlFrame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

type duplexSteerAck struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Disposition string `json:"disposition"`
	Error       string `json:"error,omitempty"`
}

// readDuplexTranscript reads the single-line initial canonical session JSON
// from stdin without waiting for EOF.
func readDuplexTranscript(r *bufio.Reader) (sessions.Session, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return sessions.Session{}, fmt.Errorf("read stdin: %w", err)
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return sessions.Session{}, fmt.Errorf("parse unified transcript JSON: unexpected empty first line")
	}
	var detail sessions.Session
	if err := json.Unmarshal(trimmed, &detail); err != nil {
		return sessions.Session{}, fmt.Errorf("parse unified transcript JSON: %w", err)
	}
	return detail, nil
}

const (
	duplexStarting = iota
	duplexActive
	duplexSettled
)

type duplexController struct {
	mu              sync.Mutex
	state           int
	readyCh         chan struct{}
	readyClosed     bool
	closedCh        chan struct{}
	closed          bool
	cancelRequested bool

	turnCtx    context.Context
	turnCancel context.CancelFunc
	cancelExec func()
	steerFn    func(ctx context.Context, message string) (string, error)
	cancelFn   func() error
	writer     *continueStreamWriter
	inFlight   sync.WaitGroup
}

func newDuplexController(execCtx context.Context, cancelExec func(), writer *continueStreamWriter) *duplexController {
	turnCtx, turnCancel := context.WithCancel(execCtx)
	return &duplexController{
		state:      duplexStarting,
		readyCh:    make(chan struct{}),
		closedCh:   make(chan struct{}),
		turnCtx:    turnCtx,
		turnCancel: turnCancel,
		cancelExec: cancelExec,
		writer:     writer,
	}
}

func (c *duplexController) start(r *bufio.Reader) {
	steerCh := make(chan duplexControlFrame, 64)
	go c.steerLoop(steerCh)
	go c.readLoop(r, steerCh)
}

func (c *duplexController) readLoop(r *bufio.Reader, steerCh chan<- duplexControlFrame) {
	defer close(steerCh)
	for {
		line, err := r.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var frame duplexControlFrame
			if json.Unmarshal(trimmed, &frame) == nil {
				switch frame.Type {
				case "cancel":
					c.handleCancel()
				case "steer":
					select {
					case steerCh <- frame:
					case <-c.closedCh:
						return
					}
				}
			}
		}
		if err != nil {
			// Stdin EOF means no more control frames will arrive; it must NOT
			// abort or kill the running turn.
			return
		}
		select {
		case <-c.closedCh:
			return
		default:
		}
	}
}

func (c *duplexController) steerLoop(steerCh <-chan duplexControlFrame) {
	for {
		select {
		case <-c.closedCh:
			return
		case frame, ok := <-steerCh:
			if !ok {
				return
			}
			select {
			case <-c.readyCh:
			case <-c.closedCh:
				return
			}
			c.handleSteer(frame)
		}
	}
}

func (c *duplexController) activate(steerFn func(context.Context, string) (string, error), cancelFn func() error) {
	c.mu.Lock()
	if c.state != duplexStarting {
		c.mu.Unlock()
		return
	}
	c.state = duplexActive
	c.steerFn = steerFn
	c.cancelFn = cancelFn
	cancelNow := c.cancelRequested
	if !c.readyClosed {
		c.readyClosed = true
		close(c.readyCh)
	}
	c.mu.Unlock()

	if cancelNow && cancelFn != nil {
		if err := cancelFn(); err != nil && c.cancelExec != nil {
			c.cancelExec()
		}
	}
}

func (c *duplexController) deactivate() {
	c.mu.Lock()
	c.state = duplexSettled
	if !c.readyClosed {
		c.readyClosed = true
		close(c.readyCh)
	}
	c.mu.Unlock()

	c.turnCancel()
	c.inFlight.Wait()
}

func (c *duplexController) close() {
	c.deactivate()
	c.writer.Close()
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.closedCh)
	}
	c.mu.Unlock()
}

func (c *duplexController) handleCancel() {
	c.mu.Lock()
	switch c.state {
	case duplexStarting:
		c.cancelRequested = true
		cancelExec := c.cancelExec
		c.mu.Unlock()
		if cancelExec != nil {
			cancelExec()
		}
	case duplexActive:
		cancelFn := c.cancelFn
		cancelExec := c.cancelExec
		c.mu.Unlock()
		if cancelFn != nil {
			if err := cancelFn(); err != nil && cancelExec != nil {
				cancelExec()
			}
		} else if cancelExec != nil {
			cancelExec()
		}
	default:
		c.mu.Unlock()
	}
}

func (c *duplexController) handleSteer(frame duplexControlFrame) {
	if strings.TrimSpace(frame.Message) == "" {
		_ = c.writer.WriteEvent(duplexSteerAck{
			Type:        "steer_ack",
			ID:          frame.ID,
			Disposition: "unsupported",
			Error:       "steer message is empty",
		})
		return
	}

	c.mu.Lock()
	if c.state != duplexActive || c.steerFn == nil {
		c.mu.Unlock()
		_ = c.writer.WriteEvent(duplexSteerAck{
			Type:        "steer_ack",
			ID:          frame.ID,
			Disposition: "idle",
		})
		return
	}
	steerFn := c.steerFn
	turnCtx := c.turnCtx
	c.inFlight.Add(1)
	c.mu.Unlock()
	defer c.inFlight.Done()

	disp, err := steerFn(turnCtx, frame.Message)
	if err != nil {
		c.mu.Lock()
		settled := c.state != duplexActive
		c.mu.Unlock()
		if settled || turnCtx.Err() != nil {
			_ = c.writer.WriteEvent(duplexSteerAck{
				Type:        "steer_ack",
				ID:          frame.ID,
				Disposition: "idle",
			})
			return
		}
		_ = c.writer.WriteEvent(duplexSteerAck{
			Type:        "steer_ack",
			ID:          frame.ID,
			Disposition: "unsupported",
			Error:       runtime.RedactSecrets(err.Error()),
		})
		return
	}
	if disp == "" {
		disp = "steered"
	}
	_ = c.writer.WriteEvent(duplexSteerAck{
		Type:        "steer_ack",
		ID:          frame.ID,
		Disposition: disp,
	})
}
