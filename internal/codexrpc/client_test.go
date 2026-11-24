package codexrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArcheMind/agentx/internal/headless"
	"github.com/ArcheMind/agentx/internal/runtime"
)

// fakeServer answers app-server requests through handle and lets tests inject notifications.
type fakeServer struct {
	out    *io.PipeWriter
	mu     sync.Mutex
	seen   []map[string]any
	closed bool
}

func startServer(t *testing.T, stderr string, handle func(s *fakeServer, req map[string]any)) (*runtime.Stream, *fakeServer) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &fakeServer{out: outW}
	go func() {
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var req map[string]any
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
			}
			s.mu.Lock()
			s.seen = append(s.seen, req)
			s.mu.Unlock()
			handle(s, req)
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return stderr }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })
	t.Cleanup(stream.Kill)
	return stream, s
}

func (s *fakeServer) send(v any) {
	raw, _ := json.Marshal(v)
	_, _ = io.WriteString(s.out, string(raw)+"\n")
}

func (s *fakeServer) result(req map[string]any, result any) {
	s.send(map[string]any{"id": req["id"], "result": result})
}

func (s *fakeServer) notify(method string, params any) {
	s.send(map[string]any{"method": method, "params": params})
}

func (s *fakeServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, req := range s.seen {
		if m, ok := req["method"].(string); ok {
			out = append(out, m)
		}
	}
	return out
}

const thread = "01a15000-1111-7222-8333-944455556666"

func baseHandle(onTurn func(s *fakeServer, req map[string]any)) func(*fakeServer, map[string]any) {
	return func(s *fakeServer, req map[string]any) {
		switch req["method"] {
		case "initialize":
			s.result(req, map[string]any{"userAgent": "fake"})
		case "thread/start", "thread/resume":
			s.result(req, map[string]any{"thread": map[string]any{"id": thread}})
		default:
			if onTurn != nil {
				onTurn(s, req)
			}
		}
	}
}

func startTurn(c *Client, prompt string) (<-chan headless.TurnResult, <-chan error, <-chan struct{}) {
	resCh := make(chan headless.TurnResult, 1)
	errCh := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		res, err := c.Turn(context.Background(), prompt, func() { close(ready) })
		resCh <- res
		errCh <- err
	}()
	return resCh, errCh, ready
}

func TestSteerRacingSettlementIsIdle(t *testing.T) {
	stream, srv := startServer(t, "", baseHandle(func(s *fakeServer, req map[string]any) {
		switch req["method"] {
		case "turn/start":
			s.result(req, map[string]any{"turn": map[string]any{"id": "t1"}})
			s.notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": "t1"}})
		case "turn/steer":
			s.send(map[string]any{"id": req["id"], "error": map[string]any{"code": -32600, "message": "no active turn to steer"}})
		}
	}))
	c := New(stream, nil, "", headless.Handlers{})
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	resCh, errCh, ready := startTurn(c, "go")
	<-ready
	// The server settled the turn just before our steer reached it: its rejection must read as idle.
	if disp, err := c.Steer(context.Background(), "late"); err != nil || disp != headless.Idle {
		t.Fatalf("steer=%q err=%v", disp, err)
	}
	srv.notify("item/completed", map[string]any{"threadId": thread, "turnId": "t1", "item": map[string]any{"type": "agentMessage", "text": "done"}})
	srv.notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": "t1", "status": "completed"}})
	res := <-resCh
	if err := <-errCh; err != nil || !res.Completed || res.Text != "done" || res.SessionID != thread {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	before := len(srv.methods())
	if disp, err := c.Steer(context.Background(), "after"); err != nil || disp != headless.Idle || len(srv.methods()) != before {
		t.Fatalf("steer after settle=%q err=%v sent=%d", disp, err, len(srv.methods())-before)
	}
}

func TestInterruptedAndFailedTurnsMapToResults(t *testing.T) {
	for _, tc := range []struct {
		status    string
		cancelled bool
		failure   string
	}{{"interrupted", true, ""}, {"failed", false, "model unavailable"}} {
		t.Run(tc.status, func(t *testing.T) {
			stream, srv := startServer(t, "", baseHandle(func(s *fakeServer, req map[string]any) {
				if req["method"] == "turn/start" {
					s.result(req, map[string]any{"turn": map[string]any{"id": "t1"}})
				}
			}))
			c := New(stream, nil, "", headless.Handlers{})
			if _, err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			resCh, errCh, ready := startTurn(c, "go")
			<-ready
			turn := map[string]any{"id": "t1", "status": tc.status}
			if tc.failure != "" {
				turn["error"] = map[string]any{"message": tc.failure}
			}
			srv.notify("turn/completed", map[string]any{"threadId": thread, "turn": turn})
			res := <-resCh
			if err := <-errCh; err != nil || res.Cancelled != tc.cancelled || res.Failure != tc.failure || res.Completed {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
}

func TestApprovalRequestsAreDeclined(t *testing.T) {
	answered := make(chan map[string]any, 1)
	stream, srv := startServer(t, "", baseHandle(func(s *fakeServer, req map[string]any) {
		switch {
		case req["method"] == "turn/start":
			s.result(req, map[string]any{"turn": map[string]any{"id": "t1"}})
			s.send(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
		case req["id"] == float64(900):
			answered <- req
		}
	}))
	c := New(stream, nil, "", headless.Handlers{})
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, ready := startTurn(c, "go")
	<-ready
	select {
	case reply := <-answered:
		if fmt.Sprint(reply["result"]) != "map[decision:decline]" {
			t.Fatalf("reply=%v", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval request was never answered")
	}
	_ = srv
}

func TestProcessCrashMidTurnReportsExit(t *testing.T) {
	stream, srv := startServer(t, "panic: token sk-proj-abcdefghijklmnopqrstuvwxyz", baseHandle(func(s *fakeServer, req map[string]any) {
		if req["method"] == "turn/start" {
			s.result(req, map[string]any{"turn": map[string]any{"id": "t1"}})
		}
	}))
	c := New(stream, nil, "", headless.Handlers{})
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, errCh, ready := startTurn(c, "go")
	<-ready
	_ = srv.out.Close()
	err := <-errCh
	if !errors.Is(err, headless.ErrExited) || !strings.Contains(err.Error(), "panic") || strings.Contains(err.Error(), "sk-proj") {
		t.Fatalf("err=%v", err)
	}
}

func TestStartupFailureAndMalformedOutput(t *testing.T) {
	var mu sync.Mutex
	var debug []string
	stream, srv := startServer(t, "", func(s *fakeServer, req map[string]any) {
		if req["method"] == "initialize" {
			_, _ = io.WriteString(s.out, "not json at all\n")
			s.send(map[string]any{"id": req["id"], "error": map[string]any{"code": -32603, "message": "boom"}})
		}
	})
	c := New(stream, nil, "", headless.Handlers{OnDebug: func(format string, args ...any) {
		mu.Lock()
		debug = append(debug, fmt.Sprintf(format, args...))
		mu.Unlock()
	}})
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(debug) != 1 || !strings.Contains(debug[0], "not json at all") {
		t.Fatalf("debug=%v", debug)
	}
	if got := srv.methods(); len(got) != 1 {
		t.Fatalf("methods after failed initialize: %v", got)
	}
}

func TestResumeMustReturnTheRequestedThread(t *testing.T) {
	stream, _ := startServer(t, "", func(s *fakeServer, req map[string]any) {
		switch req["method"] {
		case "initialize":
			s.result(req, map[string]any{})
		case "thread/resume":
			if req["params"].(map[string]any)["excludeTurns"] != true {
				t.Errorf("resume must skip turn history: %v", req["params"])
			}
			s.result(req, map[string]any{"thread": map[string]any{"id": "other-thread"}})
		}
	})
	c := New(stream, map[string]any{"cwd": "/ws"}, thread, headless.Handlers{})
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected native session") {
		t.Fatalf("err=%v", err)
	}
}
