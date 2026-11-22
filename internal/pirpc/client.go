package pirpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	replies chan Reply
}

func New(stream *runtime.Stream, onEvent func(string), onExit func()) *Client {
	c := &Client{stream: stream, replies: make(chan Reply, 4)}
	notes := make(chan string, 256)
	go func() {
		for line := range stream.Lines() {
			var r Reply
			if json.Unmarshal([]byte(line), &r) == nil && r.Type == "response" {
				c.replies <- r
			} else {
				notes <- line
			}
		}
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

func (c *Client) Send(ctx context.Context, command, message string) error {
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

type TurnResult struct {
	SessionID string
	Completed bool
	Failure   string
}

func RunTurn(ctx context.Context, stream *runtime.Stream, prompt, expectedModel string, onLine func(string) error) (TurnResult, error) {
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
	var res TurnResult
	for {
		select {
		case <-ctx.Done():
			_ = stream.WriteLine(`{"type":"abort"}`)
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
