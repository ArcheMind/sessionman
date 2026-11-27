package acp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ArcheMind/sessionman/internal/headless"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

// Session adapts Client to headless.Session for AGY.
type Session struct {
	client *Client
	opts   StartOptions

	mu     sync.Mutex
	id     string
	prefix string
	text   strings.Builder
}

func NewSession(stream *runtime.Stream, opts StartOptions, h headless.Handlers) *Session {
	s := &Session{opts: opts}
	s.client = New(stream, Handlers{
		Agent: "agy",
		OnLine: func(line string) {
			s.collect(line)
			if h.OnLine != nil {
				h.OnLine(line)
			}
		},
		OnExit:  h.OnExit,
		OnDebug: h.OnDebug,
	})
	return s
}

func (s *Session) collect(line string) {
	sid, chunk, _, ok := ParseLine(line)
	if !ok || chunk == "" {
		return
	}
	s.mu.Lock()
	if sid == s.id {
		s.text.WriteString(chunk)
	}
	s.mu.Unlock()
}

func (s *Session) Start(ctx context.Context) (string, error) {
	info, err := s.client.Start(ctx, s.opts)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.id, s.prefix = info.SessionID, info.PromptPrefix
	s.mu.Unlock()
	return info.SessionID, nil
}

// withPrefix keeps the subagent marker on the first line, where session discovery looks for it.
func withPrefix(prefix, prompt string) string {
	if prefix == "" {
		return prompt
	}
	framed := "<system-prompt>\n" + prefix + "\n</system-prompt>\n\n"
	if body, marked := strings.CutPrefix(prompt, sessions.SubagentMarker+"\n"); marked {
		return sessions.SubagentMarker + "\n" + framed + body
	}
	return framed + prompt
}

func (s *Session) Turn(ctx context.Context, prompt string, onReady func()) (headless.TurnResult, error) {
	s.mu.Lock()
	s.text.Reset()
	id := s.id
	prompt = withPrefix(s.prefix, prompt)
	s.mu.Unlock()
	res, err := s.client.Prompt(ctx, id, prompt, onReady)
	out := headless.TurnResult{SessionID: id, Usage: res.Usage}
	if err != nil {
		if res.Err != nil {
			out.Failure = err.Error()
			return out, nil
		}
		return headless.TurnResult{}, err
	}
	s.mu.Lock()
	out.Text = s.text.String()
	s.mu.Unlock()
	switch res.StopReason {
	case "end_turn":
		out.Completed = true
	case "cancelled":
		out.Cancelled = true
	default:
		out.Failure = fmt.Sprintf("agy prompt stopped with native stopReason=%q; no implicit retry", res.StopReason)
	}
	return out, nil
}

func (s *Session) Steer(ctx context.Context, message string) (string, error) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	return s.client.SteerActive(ctx, id, message)
}

func (s *Session) Cancel() error {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	return s.client.Cancel(id)
}
