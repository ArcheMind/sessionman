// Package codexrpc drives one long-lived `codex app-server` process (line-delimited
// JSON-RPC over stdio) as a headless.Session.
package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/agentx/internal/headless"
	"github.com/ArcheMind/agentx/internal/runtime"
)

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("(%d): %s", e.Code, runtime.RedactSecrets(e.Message))
}

type reply struct {
	result json.RawMessage
	err    *Error
}

type Client struct {
	stream *runtime.Stream
	h      headless.Handlers
	params map[string]any
	resume string
	closed chan struct{}

	rpcMu   sync.Mutex
	nextID  int
	pending map[int]chan reply

	mu       sync.Mutex
	threadID string
	turn     *turn
}

type turn struct {
	id   string
	done chan struct{}
	res  headless.TurnResult
}

// params are the thread/start fields (cwd, model, config, sandbox...); resume is the
// thread to reopen instead of starting one.
func New(stream *runtime.Stream, params map[string]any, resume string, h headless.Handlers) *Client {
	c := &Client{
		stream:  stream,
		h:       h,
		params:  params,
		resume:  resume,
		closed:  make(chan struct{}),
		pending: map[int]chan reply{},
	}
	go c.readLoop()
	return c
}

func (c *Client) debugf(format string, args ...any) {
	if c.h.OnDebug != nil {
		c.h.OnDebug(format, args...)
	}
}

func (c *Client) readLoop() {
	for line := range c.stream.Lines() {
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			c.debugf("invalid codex event: %v; raw=%s", err, line)
			continue
		}
		switch {
		case len(env.ID) > 0 && env.Method != "":
			c.answerRequest(env.ID, env.Method)
		case len(env.ID) > 0:
			var id int
			if json.Unmarshal(env.ID, &id) == nil {
				c.rpcMu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.rpcMu.Unlock()
				if ch != nil {
					ch <- reply{result: env.Result, err: env.Error}
				}
			}
		default:
			if c.h.OnLine != nil {
				c.h.OnLine(line)
			}
			c.observe(env.Method, env.Params)
		}
	}
	c.mu.Lock()
	close(c.closed)
	c.mu.Unlock()
	if c.h.OnExit != nil {
		c.h.OnExit()
	}
}

// Approvals cannot be answered interactively in a headless run; declining keeps the turn alive.
func (c *Client) answerRequest(id json.RawMessage, method string) {
	resp := map[string]any{"id": id}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		resp["result"] = map[string]any{"decision": "decline"}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "interactive client request unavailable in a headless run: " + method}
	}
	raw, _ := json.Marshal(resp)
	if err := c.stream.WriteLine(string(raw)); err != nil {
		c.debugf("codex client response failed: %v", err)
	}
}

func (c *Client) observe(method string, raw json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		TokenUsage struct {
			Last json.RawMessage `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.turn
	if t == nil || (p.ThreadID != "" && p.ThreadID != c.threadID) {
		return
	}
	switch method {
	case "turn/started":
		if t.id == "" {
			t.id = p.Turn.ID
		}
	case "item/completed":
		if p.Item.Type == "agentMessage" {
			t.res.Text = p.Item.Text
		}
	case "thread/tokenUsage/updated":
		if len(p.TokenUsage.Last) > 0 {
			t.res.Usage = p.TokenUsage.Last
		}
	case "turn/completed":
		if t.id != "" && p.Turn.ID != t.id {
			return
		}
		switch p.Turn.Status {
		case "completed":
			t.res.Completed = true
		case "interrupted":
			t.res.Cancelled = true
		default:
			t.res.Failure = p.Turn.Error.Message
			if t.res.Failure == "" {
				t.res.Failure = "turn " + p.Turn.Status
			}
		}
		close(t.done)
		c.turn = nil
	}
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.rpcMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.rpcMu.Unlock()
	defer func() { c.rpcMu.Lock(); delete(c.pending, id); c.rpcMu.Unlock() }()
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := c.stream.WriteLine(string(raw)); err != nil {
		return nil, fmt.Errorf("codex app-server write: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	select {
	case rep := <-ch:
		if rep.err != nil {
			return nil, fmt.Errorf("codex %s failed %w", method, rep.err)
		}
		return rep.result, nil
	case <-c.closed:
		return nil, fmt.Errorf("codex app-server exited while awaiting %s", method)
	case <-ctx.Done():
		return nil, fmt.Errorf("codex app-server did not answer %s: %w", method, ctx.Err())
	}
}

func (c *Client) Start(ctx context.Context) (string, error) {
	if _, err := c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "sessionman", "title": "SessionMan", "version": "1.0"}}); err != nil {
		return "", err
	}
	method, params := "thread/start", c.params
	if c.resume != "" {
		method = "thread/resume"
		params = map[string]any{"threadId": c.resume, "excludeTurns": true}
		for k, v := range c.params {
			params[k] = v
		}
	}
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return "", err
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("invalid codex %s response: %w", method, err)
	}
	if res.Thread.ID == "" || (c.resume != "" && res.Thread.ID != c.resume) {
		return "", fmt.Errorf("codex returned an unexpected native session; no replacement accepted")
	}
	c.mu.Lock()
	c.threadID = res.Thread.ID
	c.mu.Unlock()
	return res.Thread.ID, nil
}

func input(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

func (c *Client) Turn(ctx context.Context, prompt string, onReady func()) (headless.TurnResult, error) {
	c.mu.Lock()
	if c.turn != nil {
		c.mu.Unlock()
		return headless.TurnResult{}, fmt.Errorf("codex already has a running turn")
	}
	thread := c.threadID
	t := &turn{done: make(chan struct{}), res: headless.TurnResult{SessionID: thread}}
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

	raw, err := c.call(ctx, "turn/start", map[string]any{"threadId": thread, "input": input(prompt)})
	if err != nil {
		return fail(err)
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &started)
	c.mu.Lock()
	if t.id == "" {
		t.id = started.Turn.ID
	}
	c.mu.Unlock()
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
		return fail(fmt.Errorf("codex app-server %w without completing the turn; stderr: %s", headless.ErrExited, runtime.RedactSecrets(strings.TrimSpace(c.stream.Stderr()))))
	case <-ctx.Done():
		c.stream.Kill()
		return fail(ctx.Err())
	}
}

func (c *Client) active() (thread, turnID string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turn == nil || c.turn.id == "" {
		return "", "", false
	}
	return c.threadID, c.turn.id, true
}

func (c *Client) Steer(ctx context.Context, message string) (string, error) {
	thread, id, ok := c.active()
	if !ok {
		return headless.Idle, nil
	}
	_, err := c.call(ctx, "turn/steer", map[string]any{"threadId": thread, "expectedTurnId": id, "input": input(message)})
	if err == nil {
		return headless.Steered, nil
	}
	var rpcErr *Error
	if errors.As(err, &rpcErr) && rpcErr.Code == -32600 && (strings.Contains(rpcErr.Message, "no active turn") || strings.Contains(rpcErr.Message, "expected active turn")) {
		return headless.Idle, nil
	}
	if _, _, still := c.active(); !still {
		return headless.Idle, nil
	}
	return "", err
}

func (c *Client) Cancel() error {
	thread, id, ok := c.active()
	if !ok {
		return nil
	}
	_, err := c.call(context.Background(), "turn/interrupt", map[string]any{"threadId": thread, "turnId": id})
	return err
}
