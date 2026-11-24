package claudestream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArcheMind/agentx/internal/headless"
	"github.com/ArcheMind/agentx/internal/runtime"
)

type fakeProc struct {
	stream *runtime.Stream
	stdin  chan string
	out    *io.PipeWriter
}

func newFakeProc(t *testing.T, stderr string) *fakeProc {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &fakeProc{stdin: make(chan string, 16), out: outW}
	go func() {
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			p.stdin <- scanner.Text()
		}
	}()
	p.stream = runtime.NewPipeStream(inW, outR, func() string { return stderr }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })
	t.Cleanup(p.stream.Kill)
	return p
}

func (p *fakeProc) emit(lines ...string) {
	for _, line := range lines {
		_, _ = io.WriteString(p.out, line+"\n")
	}
}

func (p *fakeProc) emitFile(t *testing.T, name string) {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p.emit(strings.Split(strings.TrimSpace(string(data)), "\n")...)
}

func (p *fakeProc) nextStdin(t *testing.T) string {
	t.Helper()
	select {
	case line := <-p.stdin:
		return line
	case <-time.After(3 * time.Second):
		t.Fatal("no stdin line")
		return ""
	}
}

func newClient(p *fakeProc, resume string, h headless.Handlers) *Client {
	c := New(p.stream, resume, h)
	c.grace, c.notify = 30*time.Millisecond, 400*time.Millisecond
	return c
}

type turnOutcome struct {
	res headless.TurnResult
	err error
}

func startTurn(c *Client, prompt string) (<-chan turnOutcome, <-chan struct{}) {
	out := make(chan turnOutcome, 1)
	ready := make(chan struct{})
	go func() {
		res, err := c.Turn(context.Background(), prompt, func() { close(ready) })
		out <- turnOutcome{res, err}
	}()
	return out, ready
}

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"s-1"}`
	resultLine = `{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"output_tokens":1}}`
)

func TestTurnWaitsForBackgroundTaskFollowUpTurn(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "", headless.Handlers{})
	out, ready := startTurn(c, "run it in the background")
	<-ready
	p.nextStdin(t)
	p.emitFile(t, "background_first.jsonl")
	select {
	case got := <-out:
		t.Fatalf("turn returned before the background task finished: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	p.emitFile(t, "background_followup.jsonl")
	got := <-out
	if got.err != nil || !got.res.Completed || got.res.Text != "DONE1\n\nSTEERWORD" || got.res.SessionID != "8fc7b8ea-b828-449f-a383-7645d21c515a" {
		t.Fatalf("got %+v", got)
	}
}

func TestTurnSettlesWhenFinishedTaskOpensNoFollowUp(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "", headless.Handlers{})
	out, ready := startTurn(c, "go")
	<-ready
	p.emit(initLine,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b1"}]}`,
		resultLine,
		`{"type":"system","subtype":"task_notification","task_id":"b1","status":"killed"}`,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
	select {
	case got := <-out:
		if got.err != nil || !got.res.Completed {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn never settled")
	}
}

func TestSteerRacingSettlementIsIdle(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "", headless.Handlers{})
	out, ready := startTurn(c, "go")
	<-ready
	p.nextStdin(t)
	p.emit(initLine)
	if disp, err := c.Steer(context.Background(), "mid-turn"); err != nil || disp != headless.Steered {
		t.Fatalf("steer=%q err=%v", disp, err)
	}
	if line := p.nextStdin(t); !strings.Contains(line, `"mid-turn"`) || strings.Contains(line, "priority") {
		t.Fatalf("steer line %s", line)
	}
	p.emit(resultLine)
	// Steers that beat the result reaching the client are accepted; once it has been seen,
	// the grace window before settling must refuse them.
	deadline := time.Now().Add(2 * time.Second)
	for {
		disp, err := c.Steer(context.Background(), "racing")
		if err != nil {
			t.Fatal(err)
		}
		if disp == headless.Idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("steer after result was still accepted")
		}
	}
	for len(p.stdin) > 0 {
		<-p.stdin
	}
	if disp, _ := c.Steer(context.Background(), "too late"); disp != headless.Idle {
		t.Fatalf("steer in the settle window=%q", disp)
	}
	<-out
	if disp, _ := c.Steer(context.Background(), "after"); disp != headless.Idle {
		t.Fatalf("steer after settle=%q", disp)
	}
	select {
	case line := <-p.stdin:
		t.Fatalf("idle steer reached the process: %s", line)
	default:
	}
}

func TestSteerAfterResultWithRunningTaskOpensFollowUpTurn(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "", headless.Handlers{})
	out, ready := startTurn(c, "go")
	<-ready
	p.emit(initLine, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b1"}]}`, resultLine)
	time.Sleep(100 * time.Millisecond)
	if disp, err := c.Steer(context.Background(), "while waiting"); err != nil || disp != headless.Steered {
		t.Fatalf("steer=%q err=%v", disp, err)
	}
	p.emit(initLine, `{"type":"result","subtype":"success","is_error":false,"result":"second"}`,
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
	got := <-out
	if got.err != nil || got.res.Text != "second" {
		t.Fatalf("got %+v", got)
	}
}

func TestCancelInterruptsAndReportsCancelled(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "", headless.Handlers{})
	out, ready := startTurn(c, "go")
	<-ready
	p.nextStdin(t)
	p.emit(initLine)
	if err := c.Cancel(); err != nil {
		t.Fatal(err)
	}
	if line := p.nextStdin(t); !strings.Contains(line, `"subtype":"interrupt"`) || !strings.Contains(line, `"type":"control_request"`) {
		t.Fatalf("interrupt line %s", line)
	}
	p.emit(`{"type":"control_response","response":{"subtype":"success","request_id":"x"}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":false,"terminal_reason":"aborted_streaming"}`)
	got := <-out
	if got.err != nil || !got.res.Cancelled || got.res.Completed {
		t.Fatalf("got %+v", got)
	}
	// The process stays alive for the next turn.
	out2, ready2 := startTurn(c, "again")
	<-ready2
	p.emit(initLine, resultLine)
	if got := <-out2; got.err != nil || !got.res.Completed {
		t.Fatalf("turn after cancel: %+v", got)
	}
}

func TestProcessCrashMidTurnReportsExit(t *testing.T) {
	p := newFakeProc(t, "boom: sk-proj-abcdefghijklmnopqrstuvwxyz")
	exited := make(chan struct{})
	c := newClient(p, "", headless.Handlers{OnExit: func() { close(exited) }})
	out, ready := startTurn(c, "go")
	<-ready
	p.emit(initLine)
	_ = p.out.Close()
	got := <-out
	if !errors.Is(got.err, headless.ErrExited) || !strings.Contains(got.err.Error(), "boom") || strings.Contains(got.err.Error(), "sk-proj") {
		t.Fatalf("err=%v", got.err)
	}
	<-exited
}

func TestMalformedOutputIsLoggedAndStartupFailureSurfaces(t *testing.T) {
	p := newFakeProc(t, "unknown option '--bogus'")
	var mu sync.Mutex
	var debug []string
	c := newClient(p, "", headless.Handlers{OnDebug: func(format string, args ...any) {
		mu.Lock()
		debug = append(debug, fmt.Sprintf(format, args...))
		mu.Unlock()
	}})
	out, ready := startTurn(c, "go")
	<-ready
	p.emit("error: not json")
	_ = p.out.Close()
	got := <-out
	if !errors.Is(got.err, headless.ErrExited) || !strings.Contains(got.err.Error(), "unknown option") {
		t.Fatalf("err=%v", got.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(debug) != 1 || !strings.Contains(debug[0], "error: not json") {
		t.Fatalf("debug=%v", debug)
	}
}

func TestResumeRejectsADifferentNativeSession(t *testing.T) {
	p := newFakeProc(t, "")
	c := newClient(p, "11111111-2222-4333-8444-555555555555", headless.Handlers{})
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, ready := startTurn(c, "go")
	<-ready
	p.emit(initLine)
	got := <-out
	if got.err == nil && !strings.Contains(got.res.Failure, "unexpected Claude session") {
		t.Fatalf("got %+v", got)
	}
}
