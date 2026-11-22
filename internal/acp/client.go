package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/agentx/internal/runtime"
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
	ClientVersion   string // omitted when empty (agy rejects fabricated versions; claude-agent-acp requires string)
	SessionID       string
	Params          map[string]any // cwd, mcpServers, etc.
	SystemPrompt    string
	Model           string
	Effort          string
	Mode            string
	TrustCodexHooks bool
	Cwd             string
}

type SessionInfo struct {
	SessionID    string
	Capabilities Capabilities
	PromptPrefix string // non-empty when agent does not advertise systemPrompt capability
	ConfigByCat  map[string]string
}

type Handlers struct {
	Agent     string
	OnRequest func(id json.RawMessage, method string) (any, *Error)
	OnLine    func(line string)
	OnExit    func()
	OnDebug   func(format string, args ...any)
}

type reply struct {
	result json.RawMessage
	err    *Error
}

// Client speaks ACP v1 JSON-RPC 2.0 over a runtime.Stream.
type Client struct {
	agent    string
	stream   *runtime.Stream
	handlers Handlers
	caps     Capabilities

	rpcMu   sync.Mutex
	nextID  int
	pending map[int]chan reply
	live    bool
	closed  chan struct{}
}

func New(stream *runtime.Stream, h Handlers) *Client {
	if h.Agent == "" {
		h.Agent = "acp"
	}
	c := &Client{agent: h.Agent, stream: stream, handlers: h, pending: map[int]chan reply{}, closed: make(chan struct{})}
	notes := make(chan string, 256)
	go c.readLoop(notes)
	go func() {
		for line := range notes {
			if c.handlers.OnLine != nil {
				c.handlers.OnLine(line)
			}
		}
		if c.handlers.OnExit != nil {
			c.handlers.OnExit()
		}
	}()
	return c
}

func (c *Client) readLoop(notes chan<- string) {
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
				c.rpcMu.Unlock()
				if ch != nil {
					ch <- reply{result: env.Result, err: env.Error}
					continue
				}
			}
		}
		c.rpcMu.Lock()
		live := c.live
		c.rpcMu.Unlock()
		if live {
			notes <- line
		}
	}
	close(c.closed)
	close(notes)
}

func (c *Client) answerRequest(id json.RawMessage, method string) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if c.handlers.OnRequest != nil {
		res, rpcErr := c.handlers.OnRequest(id, method)
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = res
		}
	} else if method == "session/request_permission" {
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

// Start performs initialize, opens/resumes/loads the session, applies
// model/effort/mode via negotiated configOptions, and trusts Codex hooks when requested.
func (c *Client) Start(ctx context.Context, opts StartOptions) (SessionInfo, error) {
	clientInfo := map[string]any{"name": "sessionman"}
	if opts.ClientVersion != "" {
		clientInfo["version"] = opts.ClientVersion
	}
	initRaw, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": clientInfo,
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
		Resume:       len(initResp.AgentCapabilities.SessionCapabilities.Resume) > 0 && string(initResp.AgentCapabilities.SessionCapabilities.Resume) != "null",
		SystemPrompt: len(initResp.AgentCapabilities.SessionCapabilities.SystemPrompt) > 0 && string(initResp.AgentCapabilities.SessionCapabilities.SystemPrompt) != "null",
		Steering:     initResp.Meta.Steering.Supported,
	}
	c.caps = caps

	if opts.TrustCodexHooks && opts.Cwd != "" {
		if err := c.trustCodexHooks(ctx, opts.Cwd); err != nil {
			return SessionInfo{}, err
		}
	}

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
		if opt.Category != "" && opt.ID != "" {
			if _, exists := byCat[opt.Category]; !exists {
				byCat[opt.Category] = opt.ID
			}
		}
	}
	if opts.Mode != "" {
		id := byCat["mode"]
		if id == "" {
			id = "mode"
		}
		if _, err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sid, "configId": id, "value": opts.Mode}); err != nil {
			return SessionInfo{}, err
		}
	}
	if opts.Model != "" {
		if id := byCat["model"]; id != "" {
			if _, err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sid, "configId": id, "value": opts.Model}); err != nil {
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
		if _, err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sid, "configId": id, "value": opts.Effort}); err != nil {
			return SessionInfo{}, err
		}
	}
	return SessionInfo{SessionID: sid, Capabilities: caps, PromptPrefix: fallbackPrefix, ConfigByCat: byCat}, nil
}

func (c *Client) trustCodexHooks(ctx context.Context, cwd string) error {
	raw, err := c.Call(ctx, "_codex/hooks/list", map[string]any{"cwd": cwd})
	if err != nil {
		return nil // older adapter or no startup hooks
	}
	var list struct {
		Hooks []struct {
			Key         string `json:"key"`
			CurrentHash string `json:"currentHash"`
			TrustStatus string `json:"trustStatus"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var toTrust []map[string]string
	for _, h := range list.Hooks {
		if h.TrustStatus != "trusted" && h.Key != "" && h.CurrentHash != "" {
			toTrust = append(toTrust, map[string]string{"key": h.Key, "currentHash": h.CurrentHash})
		}
	}
	if len(toTrust) == 0 {
		return nil
	}
	_, err = c.Call(ctx, "_codex/hooks/trust", map[string]any{"cwd": cwd, "hooks": toTrust})
	return err
}

func (c *Client) setLive(v bool) { c.rpcMu.Lock(); c.live = v; c.rpcMu.Unlock() }

func (c *Client) SendPrompt(sessionID, text string) (int, error) {
	c.setLive(true)
	id, _, err := c.request("session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []map[string]any{{"type": "text", "text": text}},
	}, false)
	return id, err
}

// Steer delivers mid-turn input using _session/steering when advertised, falling
// back to concurrent session/prompt otherwise.
func (c *Client) Steer(ctx context.Context, sessionID, text string) (disposition string, promptID int, err error) {
	if c.caps.Steering {
		raw, err := c.Call(ctx, "_session/steering", map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": text}},
			"_meta":     map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}},
		})
		if err != nil {
			return "", 0, err
		}
		var res struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(raw, &res)
		if res.Outcome == "promptRequired" {
			return "queued", 0, nil
		}
		return "steered", 0, nil
	}
	id, err := c.SendPrompt(sessionID, text)
	if err != nil {
		return "", 0, err
	}
	return "steered", id, nil
}

func (c *Client) PromptSync(ctx context.Context, sessionID, text string) (PromptResult, error) {
	c.setLive(true)
	id, ch, err := c.request("session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": []map[string]any{{"type": "text", "text": text}},
	}, true)
	if err != nil {
		return PromptResult{}, err
	}
	defer func() { c.rpcMu.Lock(); delete(c.pending, id); c.rpcMu.Unlock() }()
	select {
	case rep := <-ch:
		if rep.err != nil {
			return PromptResult{ID: id, Err: rep.err}, fmt.Errorf("%s session/prompt failed %s", c.agent, rep.err.Error())
		}
		var body struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(rep.result, &body); err != nil {
			return PromptResult{}, fmt.Errorf("invalid %s prompt result: %w", c.agent, err)
		}
		return PromptResult{ID: id, StopReason: body.StopReason, Usage: body.Usage}, nil
	case <-c.closed:
		return PromptResult{}, fmt.Errorf("%s exited without completing the turn; stderr: %s", c.agent, runtime.RedactSecrets(strings.TrimSpace(c.stream.Stderr())))
	case <-ctx.Done():
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}})
		_ = c.stream.WriteLine(string(raw))
		c.stream.Kill()
		return PromptResult{}, ctx.Err()
	}
}

// ParseLine extracts either an agent text chunk from session/update or a prompt response.
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
