// Package claudestream drives one long-lived `claude -p --input-format stream-json
// --output-format stream-json` process as a headless.Session.
package claudestream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/agentx/internal/headless"
	"github.com/ArcheMind/agentx/internal/runtime"
)

const (
	// A user message written just before the CLI emitted its result starts a new
	// turn whose init arrives right after; settling inside this window would drop it.
	defaultSettleGrace = 400 * time.Millisecond
	// A finished background task makes the CLI open a follow-up turn by itself;
	// tasks that end without one (killed) must not hold the turn open forever.
	defaultNotifyWait = 5 * time.Second
)

type Client struct {
	stream  *runtime.Stream
	h       headless.Handlers
	resume  string
	grace   time.Duration
	notify  time.Duration
	closed  chan struct{}
	writeMu sync.Mutex

	mu        sync.Mutex
	sessionID string
	turn      *turn
	tasks     map[string]bool
	notified  bool
	timer     *time.Timer
	timerGen  uint64
	requests  int
}

type turn struct {
	done       chan struct{}
	finished   bool
	resultSeen bool
	cancelled  bool
	failed     bool
	res        headless.TurnResult
}

func New(stream *runtime.Stream, resume string, h headless.Handlers) *Client {
	c := &Client{
		stream: stream,
		h:      h,
		resume: resume,
		grace:  defaultSettleGrace,
		notify: defaultNotifyWait,
		closed: make(chan struct{}),
		tasks:  map[string]bool{},
	}
	go c.readLoop()
	return c
}

func (c *Client) Start(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = c.resume
	return c.resume, nil
}

func (c *Client) debugf(format string, args ...any) {
	if c.h.OnDebug != nil {
		c.h.OnDebug(format, args...)
	}
}

func (c *Client) readLoop() {
	for line := range c.stream.Lines() {
		if c.h.OnLine != nil {
			c.h.OnLine(line)
		}
		c.observe(line)
	}
	c.mu.Lock()
	c.stopTimerLocked()
	close(c.closed)
	c.mu.Unlock()
	if c.h.OnExit != nil {
		c.h.OnExit()
	}
}

type event struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Usage     json.RawMessage `json:"usage"`
	Tasks     []struct {
		ID string `json:"task_id"`
	} `json:"tasks"`
	TaskID string `json:"task_id"`
}

func (c *Client) observe(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	var ev event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		c.debugf("invalid claude event: %v; raw=%s", err, line)
		return
	}
	switch ev.Type {
	case "system":
		switch ev.Subtype {
		case "init":
			c.onInit(ev.SessionID)
		case "background_tasks_changed":
			c.mu.Lock()
			c.tasks = map[string]bool{}
			for _, t := range ev.Tasks {
				c.tasks[t.ID] = true
			}
			c.rearmLocked()
			c.mu.Unlock()
		case "task_started":
			c.mu.Lock()
			if ev.TaskID != "" {
				c.tasks[ev.TaskID] = true
			}
			c.mu.Unlock()
		case "task_notification":
			c.mu.Lock()
			if c.turn != nil && c.turn.resultSeen {
				c.notified = true
				c.rearmLocked()
			}
			c.mu.Unlock()
		}
	case "result":
		c.mu.Lock()
		if t := c.turn; t != nil {
			t.resultSeen = true
			t.res.Text, t.res.Usage = ev.Result, ev.Usage
			t.res.Completed = ev.Subtype == "success" && !ev.IsError
			switch {
			case t.cancelled:
				t.res.Cancelled, t.res.Completed = true, false
			case !t.res.Completed:
				t.failed = true
				t.res.Failure = ev.Result
				if t.res.Failure == "" {
					t.res.Failure = ev.Subtype
				}
			}
			c.rearmLocked()
		}
		c.mu.Unlock()
	}
}

func (c *Client) onInit(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	t := c.turn
	if t != nil {
		t.resultSeen = false
		c.notified = false
		c.stopTimerLocked()
	}
	mismatch := c.resume != "" && id != c.resume
	changed := id != c.sessionID
	c.sessionID = id
	if t != nil {
		t.res.SessionID = id
		if mismatch && !t.failed {
			t.failed = true
			t.res.Failure = "unexpected Claude session; no continuation accepted"
		}
	}
	c.mu.Unlock()
	if changed && c.h.OnSession != nil {
		c.h.OnSession(id)
	}
	if mismatch {
		c.stream.Kill()
	}
}

func (c *Client) stopTimerLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.timerGen++
}

// rearmLocked schedules settlement once the turn has a result and nothing native is
// still owed: no background task and no follow-up turn pending.
func (c *Client) rearmLocked() {
	t := c.turn
	c.stopTimerLocked()
	if t == nil || t.finished || !t.resultSeen {
		return
	}
	var delay time.Duration
	switch {
	case t.cancelled || t.failed:
		delay = c.grace
	case len(c.tasks) > 0:
		return
	case c.notified:
		delay = c.notify
	default:
		delay = c.grace
	}
	gen := c.timerGen
	c.timer = time.AfterFunc(delay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.timerGen != gen || c.turn != t || t.finished || !t.resultSeen {
			return
		}
		if len(c.tasks) > 0 && !t.cancelled && !t.failed {
			return
		}
		c.notified = false
		t.finished = true
		close(t.done)
	})
}

func userLine(text string) (string, error) {
	raw, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}}})
	return string(raw), err
}

func (c *Client) write(line string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.stream.WriteLine(line)
}

func (c *Client) Turn(ctx context.Context, prompt string, onReady func()) (headless.TurnResult, error) {
	line, err := userLine(prompt)
	if err != nil {
		return headless.TurnResult{}, err
	}
	c.mu.Lock()
	if c.turn != nil {
		c.mu.Unlock()
		return headless.TurnResult{}, fmt.Errorf("claude already has a running turn")
	}
	t := &turn{done: make(chan struct{}), res: headless.TurnResult{SessionID: c.sessionID}}
	c.turn = t
	c.notified = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.stopTimerLocked()
		c.turn = nil
		c.mu.Unlock()
	}()

	if err := c.write(line); err != nil {
		return headless.TurnResult{}, err
	}
	if onReady != nil {
		onReady()
	}
	select {
	case <-t.done:
		c.mu.Lock()
		res := t.res
		c.mu.Unlock()
		return res, nil
	case <-c.closed:
		c.mu.Lock()
		res, finished := t.res, t.finished
		c.mu.Unlock()
		if finished {
			return res, nil
		}
		return res, fmt.Errorf("claude %w without completing the turn; stderr: %s", headless.ErrExited, runtime.RedactSecrets(strings.TrimSpace(c.stream.Stderr())))
	case <-ctx.Done():
		_ = c.Cancel()
		c.stream.Kill()
		return headless.TurnResult{}, ctx.Err()
	}
}

func (c *Client) Steer(ctx context.Context, message string) (string, error) {
	line, err := userLine(message)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	t := c.turn
	// A result with background tasks still running means the CLI is between turns:
	// the message opens a new one this Turn call then waits for.
	if t == nil || t.finished || t.cancelled || (t.resultSeen && len(c.tasks) == 0) {
		c.mu.Unlock()
		return headless.Idle, nil
	}
	if t.resultSeen {
		t.resultSeen = false
		c.stopTimerLocked()
	}
	c.mu.Unlock()
	if err := c.write(line); err != nil {
		select {
		case <-c.closed:
			return headless.Idle, nil
		default:
		}
		return "", err
	}
	return headless.Steered, nil
}

func (c *Client) Cancel() error {
	c.mu.Lock()
	t := c.turn
	if t == nil || t.finished {
		c.mu.Unlock()
		return nil
	}
	t.cancelled = true
	c.requests++
	id := fmt.Sprintf("sm-interrupt-%d", c.requests)
	c.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "interrupt"}})
	if err != nil {
		return err
	}
	return c.write(string(raw))
}
