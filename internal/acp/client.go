package acp

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

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("(%d): %s", e.Code, runtime.RedactSecrets(e.Message))
}

type PromptResult struct {
	ID         int             `json:"id"`
	StopReason string          `json:"stopReason"`
	Usage      json.RawMessage `json:"usage"`
	Err        *Error          `json:"-"`
}

type Capabilities struct {
	Resume       bool
	Load         bool
	SystemPrompt bool
	Steering     bool
}

type StartOptions struct {
	SessionID    string
	Params       map[string]any // cwd, mcpServers, etc.
	SystemPrompt string
	Model        string
	Effort       string
}

type SessionInfo struct {
	SessionID    string
	Capabilities Capabilities
	PromptPrefix string // non-empty when agent does not advertise systemPrompt capability
}

type Handlers struct {
	Agent                  string
	OnLine                 func(line string)
	OnExit                 func()
	OnDebug                func(format string, args ...any)
	BackgroundSettleWindow time.Duration
}

type reply struct {
	result json.RawMessage
	err    *Error
}

type noteItem struct {
	line string
	fn   func()
}

// Client speaks ACP v1 (AGY) over a runtime.Stream.
type Client struct {
	agent    string
	stream   *runtime.Stream
	handlers Handlers
	caps     Capabilities

	turn *v1Turn

	rpcMu   sync.Mutex
	nextID  int
	pending map[int]chan reply
	live    bool
	closed  chan struct{}

	notesMu     sync.Mutex
	notes       chan noteItem
	notesClosed bool
}

func New(stream *runtime.Stream, h Handlers) *Client {
	if h.Agent == "" {
		h.Agent = "acp"
	}
	notes := make(chan noteItem, 256)
	c := &Client{
		agent:    h.Agent,
		stream:   stream,
		handlers: h,
		turn:     newV1Turn(h.BackgroundSettleWindow),
		pending:  map[int]chan reply{},
		closed:   make(chan struct{}),
		notes:    notes,
	}
	go c.readLoop()
	go func() {
		for item := range notes {
			if item.fn != nil {
				item.fn()
			} else if c.handlers.OnLine != nil {
				c.handlers.OnLine(item.line)
			}
		}
		if c.handlers.OnExit != nil {
			c.handlers.OnExit()
		}
	}()
	return c
}

func (c *Client) enqueueNote(item noteItem) bool {
	c.notesMu.Lock()
	defer c.notesMu.Unlock()
	if c.notesClosed {
		return false
	}
	c.notes <- item
	return true
}

func (c *Client) drainNotes() {
	done := make(chan struct{})
	if c.enqueueNote(noteItem{fn: func() { close(done) }}) {
		<-done
	}
}

func (c *Client) readLoop() {
	for line := range c.stream.Lines() {
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			if c.handlers.OnDebug != nil {
				c.handlers.OnDebug("invalid %s event: %v; raw=%s", c.agent, err, line)
			}
			continue
		}
		if len(env.ID) > 0 && env.Method != "" {
			c.answerRequest(env.ID, env.Method)
			continue
		}
		if len(env.ID) > 0 {
			var id int
			if json.Unmarshal(env.ID, &id) == nil {
				c.rpcMu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				live := c.live
				c.rpcMu.Unlock()

				if ch != nil {
					ch <- reply{result: env.Result, err: env.Error}
					continue
				}
				if live {
					c.enqueueNote(noteItem{line: line})
				}
				c.turn.OnPromptResponse(id, env.Result, env.Error)
				continue
			}
		}
		c.rpcMu.Lock()
		live := c.live
		c.rpcMu.Unlock()
		if live {
			c.enqueueNote(noteItem{line: line})
			c.turn.Observe(line)
		}
	}
	c.turn.Close()
	close(c.closed)
	c.notesMu.Lock()
	c.notesClosed = true
	close(c.notes)
	c.notesMu.Unlock()
}

func (c *Client) answerRequest(id json.RawMessage, method string) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "session/request_permission" {
		resp["result"] = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	} else {
		resp["error"] = map[string]any{"code": -32601, "message": "interactive client request unavailable: " + method}
	}
	raw, _ := json.Marshal(resp)
	if err := c.stream.WriteLine(string(raw)); err != nil && c.handlers.OnDebug != nil {
		c.handlers.OnDebug("%s client response failed: %v", c.agent, err)
	}
}

func (c *Client) request(method string, params any, wait bool) (int, <-chan reply, error) {
	c.rpcMu.Lock()
	c.nextID++
	id := c.nextID
	var ch chan reply
	if wait {
		ch = make(chan reply, 1)
		c.pending[id] = ch
	} else {
		// Register before writing: a fast agent can reply before WriteLine returns.
		c.turn.OnPromptSent(id)
	}
	c.rpcMu.Unlock()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil {
		err = c.stream.WriteLine(string(raw))
	}
	if err != nil {
		c.rpcMu.Lock()
		delete(c.pending, id)
		c.rpcMu.Unlock()
		if !wait {
			c.turn.CancelPrompt(id)
		}
	}
	return id, ch, err
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch, err := c.request(method, params, true)
	if err != nil {
		return nil, err
	}
	defer func() { c.rpcMu.Lock(); delete(c.pending, id); c.rpcMu.Unlock() }()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	select {
	case rep := <-ch:
		if rep.err != nil {
			return nil, fmt.Errorf("%s %s failed %s", c.agent, method, rep.err.Error())
		}
		return rep.result, nil
	case <-c.closed:
		return nil, fmt.Errorf("%s exited while awaiting %s", c.agent, method)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s did not answer %s: %w", c.agent, method, ctx.Err())
	}
}

// Start initializes, opens/resumes/loads the session and applies model/effort via
// the session's configOptions.
func (c *Client) Start(ctx context.Context, opts StartOptions) (SessionInfo, error) {
	initRaw, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "sessionman"},
	})
	if err != nil {
		return SessionInfo{}, err
	}
	var initResp struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Resume       json.RawMessage `json:"resume"`
				SystemPrompt json.RawMessage `json:"systemPrompt"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
		Meta struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(initRaw, &initResp)
	caps := Capabilities{
		Load:         initResp.AgentCapabilities.LoadSession,
		Resume:       hasJSONObject(initResp.AgentCapabilities.SessionCapabilities.Resume),
		SystemPrompt: hasJSONObject(initResp.AgentCapabilities.SessionCapabilities.SystemPrompt),
		Steering:     initResp.Meta.Steering.Supported,
	}
	c.caps = caps

	params := make(map[string]any, len(opts.Params)+2)
	for k, v := range opts.Params {
		params[k] = v
	}
	var fallbackPrefix string
	if opts.SystemPrompt != "" {
		if caps.SystemPrompt {
			params["systemPrompt"] = map[string]any{
				"mode":    "append",
				"content": []map[string]any{{"type": "text", "text": opts.SystemPrompt}},
			}
		} else {
			fallbackPrefix = opts.SystemPrompt
		}
	}
	method := "session/new"
	if opts.SessionID != "" {
		params["sessionId"] = opts.SessionID
		if caps.Resume {
			method = "session/resume"
		} else {
			method = "session/load"
		}
	}
	sessRaw, err := c.Call(ctx, method, params)
	if err != nil {
		return SessionInfo{}, err
	}
	var sess struct {
		ID            string `json:"sessionId"`
		ConfigOptions []struct {
			ID       string `json:"id"`
			ConfigID string `json:"configId"`
			Category string `json:"category"`
		} `json:"configOptions"`
	}
	if err := json.Unmarshal(sessRaw, &sess); err != nil {
		return SessionInfo{}, fmt.Errorf("invalid %s session response: %w", c.agent, err)
	}
	sid := opts.SessionID
	if sid == "" {
		if sess.ID == "" {
			return SessionInfo{}, fmt.Errorf("%s returned no native session; no replacement accepted", c.agent)
		}
		sid = sess.ID
	} else if sess.ID != "" && sess.ID != sid {
		return SessionInfo{}, fmt.Errorf("%s returned a different native session; no replacement accepted", c.agent)
	}
	byCat := map[string]string{}
	for _, opt := range sess.ConfigOptions {
		optID := opt.ConfigID
		if optID == "" {
			optID = opt.ID
		}
		if opt.Category != "" && optID != "" {
			if _, exists := byCat[opt.Category]; !exists {
				byCat[opt.Category] = optID
			}
		}
	}
	if opts.Model != "" {
		if id := byCat["model"]; id != "" {
			if err := c.setConfigOption(ctx, sid, id, opts.Model); err != nil {
				return SessionInfo{}, err
			}
		} else if _, err := c.Call(ctx, "session/set_model", map[string]any{"sessionId": sid, "modelId": opts.Model}); err != nil {
			return SessionInfo{}, err
		}
	}
	if opts.Effort != "" {
		id := byCat["thought_level"]
		if id == "" {
			id = "thought_level"
		}
		if err := c.setConfigOption(ctx, sid, id, opts.Effort); err != nil {
			return SessionInfo{}, err
		}
	}
	return SessionInfo{SessionID: sid, Capabilities: caps, PromptPrefix: fallbackPrefix}, nil
}

func hasJSONObject(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func (c *Client) setConfigOption(ctx context.Context, sessionID, configID, value string) error {
	_, err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": configID, "value": value})
	return err
}

func (c *Client) setLive(v bool) { c.rpcMu.Lock(); c.live = v; c.rpcMu.Unlock() }

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Cancel sends a session/cancel notification to abort the active turn.
func (c *Client) Cancel(sessionID string) error {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/cancel",
		"params":  map[string]any{"sessionId": sessionID},
	})
	if err != nil {
		return err
	}
	err = c.stream.WriteLine(string(raw))
	c.turn.OnCancel()
	return err
}

// SteerActive delivers mid-turn input only while a turn is active, returning
// disposition "steered" or "idle" without starting a new turn after settlement.
func (c *Client) SteerActive(ctx context.Context, sessionID, text string) (disposition string, err error) {
	if c.caps.Steering {
		if !c.turn.IsActive() || c.isClosed() {
			return "idle", nil
		}
		raw, err := c.Call(ctx, "_session/steering", map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": text}},
			"_meta":     map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}},
		})
		if err != nil {
			if !c.turn.IsActive() || c.isClosed() {
				return "idle", nil
			}
			return "", err
		}
		var res struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(raw, &res)
		if res.Outcome == "promptRequired" {
			return "idle", nil
		}
		return "steered", nil
	}
	c.rpcMu.Lock()
	c.nextID++
	id := c.nextID
	c.rpcMu.Unlock()

	turn := c.turn
	if !turn.OnSteerPrompt(id) {
		return "idle", nil
	}
	c.setLive(true)
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": text}},
		},
	})
	if err == nil {
		err = c.stream.WriteLine(string(raw))
	}
	if err != nil {
		turn.CancelPrompt(id)
		if !turn.IsActive() || c.isClosed() {
			return "idle", nil
		}
		return "", err
	}
	return "steered", nil
}

// Prompt runs one turn to completion. onReady runs once the prompt is on the wire.
func (c *Client) Prompt(ctx context.Context, sessionID, text string, onReady func()) (PromptResult, error) {
	turn := c.turn
	doneCh := turn.Reset(sessionID, nil)
	c.setLive(true)
	_, _, err := c.request("session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []map[string]any{{"type": "text", "text": text}},
	}, false)
	if err != nil {
		turn.Close()
		return PromptResult{}, err
	}
	if onReady != nil {
		onReady()
	}

	select {
	case <-doneCh:
		c.drainNotes()
		res := turn.Result()
		if res.Err != nil {
			return res, fmt.Errorf("%s session/prompt failed %s", c.agent, res.Err.Error())
		}
		return res, nil
	case <-c.closed:
		return PromptResult{}, fmt.Errorf("%s %w without completing the turn; stderr: %s", c.agent, headless.ErrExited, runtime.RedactSecrets(strings.TrimSpace(c.stream.Stderr())))
	case <-ctx.Done():
		turn.Close()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}})
		_ = c.stream.WriteLine(string(raw))
		c.stream.Kill()
		return PromptResult{}, ctx.Err()
	}
}

// ParseLine extracts either an agent text chunk from session/update or a v1 prompt response.
func ParseLine(line string) (sessionID, textChunk string, prompt *PromptResult, ok bool) {
	var ev struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Error  *Error `json:"error"`
		Result struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		} `json:"result"`
		Params struct {
			Session string `json:"sessionId"`
			Update  struct {
				Type    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return "", "", nil, false
	}
	if ev.Method == "session/update" {
		if ev.Params.Update.Type == "agent_message_chunk" && ev.Params.Update.Content.Type == "text" {
			return ev.Params.Session, ev.Params.Update.Content.Text, nil, true
		}
		return ev.Params.Session, "", nil, true
	}
	if ev.ID != 0 {
		return "", "", &PromptResult{ID: ev.ID, StopReason: ev.Result.StopReason, Usage: ev.Result.Usage, Err: ev.Error}, true
	}
	return "", "", nil, false
}
