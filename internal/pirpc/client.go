package pirpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/sessionman/internal/headless"
	"github.com/ArcheMind/sessionman/internal/runtime"
)

type Reply struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Disposition string `json:"disposition"`
	} `json:"data"`
}

type AssistantMessage struct {
	Text    string
	Usage   json.RawMessage
	Failure string
}

func ParseAssistant(raw json.RawMessage, expectedModel string) (AssistantMessage, error) {
	var msg struct {
		Role         string          `json:"role"`
		Provider     string          `json:"provider"`
		Model        string          `json:"model"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		Usage        json.RawMessage `json:"usage"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return AssistantMessage{}, err
	}
	if msg.Role != "assistant" {
		return AssistantMessage{}, fmt.Errorf("not an assistant message")
	}
	var parts []string
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	out := AssistantMessage{Text: strings.Join(parts, "\n"), Usage: msg.Usage}
	if expectedModel != "" && msg.Provider+"/"+msg.Model != expectedModel {
		out.Failure = fmt.Sprintf("Pi returned model %s/%s instead of %s", msg.Provider, msg.Model, expectedModel)
	}
	if out.Failure == "" && (msg.StopReason == "error" || msg.StopReason == "aborted") {
		out.Failure = msg.ErrorMessage
		if out.Failure == "" {
			out.Failure = msg.StopReason
		}
	}
	return out, nil
}

type Client struct {
	stream  *runtime.Stream
	model   string
	h       headless.Handlers
	sendMu  sync.Mutex
	replies chan Reply
	closed  chan struct{}

	mu   sync.Mutex
	turn *turn
}

type turn struct {
	done chan struct{}
	res  headless.TurnResult
}

// model is the provider/model the turn must come back from; "" skips the check.
func New(stream *runtime.Stream, model string, h headless.Handlers) *Client {
	c := &Client{
		stream:  stream,
		model:   model,
		h:       h,
		replies: make(chan Reply, 4),
		closed:  make(chan struct{}),
	}
	notes := make(chan string, 256)
	go func() {
		for line := range stream.Lines() {
			var r Reply
			if json.Unmarshal([]byte(line), &r) == nil && r.Type == "response" {
				if r.Command == "abort" {
					continue
				}
				c.replies <- r
			} else {
				notes <- line
			}
		}
		close(c.closed)
		close(c.replies)
		close(notes)
	}()
	go func() {
		for line := range notes {
			if c.h.OnLine != nil {
				c.h.OnLine(line)
			}
			c.observe(line)
		}
		if c.h.OnExit != nil {
			c.h.OnExit()
		}
	}()
	return c
}

func (c *Client) Start(context.Context) (string, error) { return "", nil }

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) observe(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	var ev struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Message json.RawMessage `json:"message"`
		Aborted bool            `json:"aborted"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		if c.h.OnDebug != nil {
			c.h.OnDebug("invalid pi event: %v; raw=%s", err, line)
		}
		return
	}
	if ev.Type == "session" && ev.ID != "" && c.h.OnSession != nil {
		c.h.OnSession(ev.ID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.turn
	if t == nil {
		return
	}
	switch ev.Type {
	case "session":
		if ev.ID != "" {
			t.res.SessionID = ev.ID
		}
	case "message_end":
		if msg, err := ParseAssistant(ev.Message, c.model); err == nil {
			t.res.Text, t.res.Usage, t.res.Failure = msg.Text, msg.Usage, msg.Failure
		}
	case "agent_end":
		t.res.Completed = true
	case "agent_settled":
		t.res.Completed = true
		if ev.Aborted && t.res.Failure == "" {
			t.res.Failure = "pi run aborted"
		}
		close(t.done)
		c.turn = nil
	}
}

func (c *Client) send(ctx context.Context, command, message string) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	line, err := json.Marshal(map[string]any{"type": command, "message": message})
	if err != nil {
		return err
	}
	if err := c.stream.WriteLine(string(line)); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	select {
	case rep, ok := <-c.replies:
		if !ok {
			return fmt.Errorf("pi exited while awaiting %s", command)
		}
		if rep.Command != command {
			return fmt.Errorf("pi acknowledged %s while awaiting %s", rep.Command, command)
		}
		if !rep.Success {
			return fmt.Errorf("pi %s failed: %s", command, runtime.RedactSecrets(rep.Error))
		}
		want := "started"
		if command == "steer" {
			want = "queued"
		}
		if rep.Data.Disposition != want {
			return fmt.Errorf("pi %s disposition %q did not start or steer a turn", command, rep.Data.Disposition)
		}
		return nil
	case <-ctx.Done():
		c.stream.Kill()
		return fmt.Errorf("pi did not acknowledge %s: %w", command, ctx.Err())
	}
}

// Steer maps Pi's steer response to Steered or Idle without killing the stream on
// context cancellation.
func (c *Client) Steer(ctx context.Context, message string) (string, error) {
	c.mu.Lock()
	running := c.turn != nil
	c.mu.Unlock()
	if !running {
		return headless.Idle, nil
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.isClosed() {
		return headless.Idle, nil
	}
	line, err := json.Marshal(map[string]any{"type": "steer", "message": message})
	if err != nil {
		return "", err
	}
	if err := c.stream.WriteLine(string(line)); err != nil {
		if c.isClosed() {
			return headless.Idle, nil
		}
		return "", err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	select {
	case rep, ok := <-c.replies:
		if !ok {
			return headless.Idle, nil
		}
		if rep.Command != "steer" {
			return "", fmt.Errorf("pi acknowledged %s while awaiting steer", rep.Command)
		}
		if !rep.Success {
			lower := strings.ToLower(rep.Error)
			if strings.Contains(lower, "idle") || strings.Contains(lower, "not running") || strings.Contains(lower, "no active") {
				return headless.Idle, nil
			}
			return "", fmt.Errorf("pi steer failed: %s", runtime.RedactSecrets(rep.Error))
		}
		switch rep.Data.Disposition {
		case "queued":
			return headless.Steered, nil
		case "idle", "promptRequired":
			return headless.Idle, nil
		default:
			return "", fmt.Errorf("pi steer disposition %q did not steer a turn", rep.Data.Disposition)
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Client) Cancel() error {
	return c.stream.WriteLine(`{"type":"abort"}`)
}

func (c *Client) Turn(ctx context.Context, prompt string, onReady func()) (headless.TurnResult, error) {
	c.mu.Lock()
	if c.turn != nil {
		c.mu.Unlock()
		return headless.TurnResult{}, fmt.Errorf("pi already has a running turn")
	}
	t := &turn{done: make(chan struct{})}
	c.turn = t
	c.mu.Unlock()
	fail := func(err error) (headless.TurnResult, error) {
		c.mu.Lock()
		if c.turn == t {
			c.turn = nil
		}
		c.mu.Unlock()
		return headless.TurnResult{}, err
	}

	if err := c.send(ctx, "prompt", prompt); err != nil {
		return fail(err)
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
		res, settled := t.res, c.turn != t
		c.mu.Unlock()
		if settled {
			return res, nil
		}
		return fail(fmt.Errorf("pi %w without completing the turn", headless.ErrExited))
	case <-ctx.Done():
		_ = c.Cancel()
		c.stream.Kill()
		return fail(ctx.Err())
	}
}
