package pirpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/agentx/internal/runtime"
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
	sendMu  sync.Mutex
	replies chan Reply
	closed  chan struct{}
}

func New(stream *runtime.Stream, onEvent func(string), onExit func()) *Client {
	c := &Client{
		stream:  stream,
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
			if onEvent != nil {
				onEvent(line)
			}
		}
		if onExit != nil {
			onExit()
		}
	}()
	return c
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) Send(ctx context.Context, command, message string) error {
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

// Steer sends a mid-turn steer command to Pi and maps its response to
// "steered" or "idle" without killing the stream on context cancellation.
func (c *Client) Steer(ctx context.Context, message string) (string, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.isClosed() {
		return "idle", nil
	}
	line, err := json.Marshal(map[string]any{"type": "steer", "message": message})
	if err != nil {
		return "", err
	}
	if err := c.stream.WriteLine(string(line)); err != nil {
		if c.isClosed() {
			return "idle", nil
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
			return "idle", nil
		}
		if rep.Command != "steer" {
			return "", fmt.Errorf("pi acknowledged %s while awaiting steer", rep.Command)
		}
		if !rep.Success {
			lower := strings.ToLower(rep.Error)
			if strings.Contains(lower, "idle") || strings.Contains(lower, "not running") || strings.Contains(lower, "no active") {
				return "idle", nil
			}
			return "", fmt.Errorf("pi steer failed: %s", runtime.RedactSecrets(rep.Error))
		}
		switch rep.Data.Disposition {
		case "queued":
			return "steered", nil
		case "idle", "promptRequired":
			return "idle", nil
		default:
			return "", fmt.Errorf("pi steer disposition %q did not steer a turn", rep.Data.Disposition)
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Abort sends an abort command to Pi to cancel the active turn.
func (c *Client) Abort() error {
	return c.stream.WriteLine(`{"type":"abort"}`)
}

type TurnControl struct {
	Steer  func(ctx context.Context, message string) (string, error)
	Cancel func() error
}

type TurnResult struct {
	SessionID string
	Completed bool
	Failure   string
}

func RunTurn(ctx context.Context, stream *runtime.Stream, prompt, expectedModel string, onLine func(string) error) (TurnResult, error) {
	return RunTurnWithControl(ctx, stream, prompt, expectedModel, onLine, nil)
}

func RunTurnWithControl(ctx context.Context, stream *runtime.Stream, prompt, expectedModel string, onLine func(string) error, onReady func(TurnControl)) (TurnResult, error) {
	done := make(chan struct{})
	events := make(chan string, 256)
	c := New(stream, func(line string) {
		select {
		case events <- line:
		case <-done:
		}
	}, func() { close(events) })
	defer close(done)

	if err := c.Send(ctx, "prompt", prompt); err != nil {
		return TurnResult{}, err
	}

	turnCtx, cancelTurn := context.WithCancel(ctx)
	var stateMu sync.Mutex
	active := true
	var inFlight sync.WaitGroup
	settle := func() {
		stateMu.Lock()
		active = false
		stateMu.Unlock()
		cancelTurn()
		inFlight.Wait()
	}
	defer settle()

	if onReady != nil {
		onReady(TurnControl{
			Steer: func(steerCtx context.Context, message string) (string, error) {
				stateMu.Lock()
				if !active {
					stateMu.Unlock()
					return "idle", nil
				}
				inFlight.Add(1)
				stateMu.Unlock()
				defer inFlight.Done()

				callCtx, cancelCall := context.WithCancel(steerCtx)
				defer cancelCall()
				go func() {
					select {
					case <-turnCtx.Done():
						cancelCall()
					case <-callCtx.Done():
					}
				}()

				disp, err := c.Steer(callCtx, message)
				if err != nil {
					stateMu.Lock()
					stillActive := active
					stateMu.Unlock()
					if !stillActive || turnCtx.Err() != nil {
						return "idle", nil
					}
					return "", err
				}
				return disp, nil
			},
			Cancel: func() error {
				return c.Abort()
			},
		})
	}

	var res TurnResult
	for {
		select {
		case <-ctx.Done():
			_ = c.Abort()
			stream.Kill()
			return res, ctx.Err()
		case line, ok := <-events:
			if !ok {
				return res, nil
			}
			if onLine != nil {
				if err := onLine(line); err != nil {
					stream.Kill()
					return res, err
				}
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Message json.RawMessage `json:"message"`
				Aborted bool            `json:"aborted"`
			}
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				stream.Kill()
				return res, fmt.Errorf("invalid pi event: %w", err)
			}
			switch ev.Type {
			case "session":
				if ev.ID != "" {
					res.SessionID = ev.ID
				}
			case "message_end":
				if msg, err := ParseAssistant(ev.Message, expectedModel); err == nil && msg.Failure != "" {
					res.Failure = msg.Failure
				}
			case "agent_end":
				res.Completed = true
			case "agent_settled":
				res.Completed = true
				if ev.Aborted && res.Failure == "" {
					res.Failure = "pi run aborted"
				}
				return res, nil
			}
		}
	}
}
