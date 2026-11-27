package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
	"github.com/ArcheMind/sessionman/internal/runtime"
	"github.com/ArcheMind/sessionman/internal/sessions"
)

type duplexEvent struct {
	Type        string                   `json:"type"`
	ID          string                   `json:"id"`
	Disposition string                   `json:"disposition"`
	Error       string                   `json:"error"`
	ProtocolMessage       sessions.ProtocolMessage           `json:"message"`
	Session     sessions.ProtocolSession `json:"session"`
}

func decodeDuplexEvents(t *testing.T, output string) ([]sessions.ProtocolMessage, []duplexEvent, *sessions.ProtocolSession) {
	t.Helper()
	var entries []sessions.ProtocolMessage
	var acks []duplexEvent
	var result *sessions.ProtocolSession
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev duplexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		if result != nil {
			t.Fatalf("event after terminal result: %s", line)
		}
		switch ev.Type {
		case "message":
			if err := transcript.ValidateMessage(ev.ProtocolMessage); err != nil {
				t.Fatalf("invalid streamed 2.0 entry: %v", err)
			}
			entries = append(entries, ev.ProtocolMessage)
		case "steer_ack":
			acks = append(acks, ev)
		case "result":
			if err := transcript.ValidateSession(ev.Session); err != nil {
				t.Fatalf("invalid terminal 2.0 session: %v", err)
			}
			s := ev.Session
			result = &s
		default:
			t.Fatalf("unknown event type %q in %s", ev.Type, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return entries, acks, result
}

func TestSessionContinueDuplexFlagValidation(t *testing.T) {
	workspace := testWorkspace(t)
	transcript := continueTranscript(t, workspace)

	t.Run("requires stream", func(t *testing.T) {
		application, _, _ := continueApp(t, "claude", "", transcript)
		err := application.Run(context.Background(), []string{"session", "continue", "claude", "--duplex"})
		if err == nil || !strings.Contains(err.Error(), "--duplex requires --stream") {
			t.Fatalf("expected --duplex requires --stream error, got %v", err)
		}
	})

	t.Run("rejects file argument", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "transcript.json")
		if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		application, _, _ := continueApp(t, "claude", "", "")
		err := application.Run(context.Background(), []string{"session", "continue", "claude", path, "--stream", "--duplex"})
		if err == nil || !strings.Contains(err.Error(), "--duplex cannot be combined with a transcript file") {
			t.Fatalf("expected file conflict error, got %v", err)
		}
	})
}

func TestSessionContinueDuplexStartsOnLine1AndIgnoresStdinEOF(t *testing.T) {
	workspace := testWorkspace(t)
	inR, inW := io.Pipe()
	application, _, baseRunner := continueApp(t, "pi", "anthropic/claude-x", "")
	application.Stdin = inR
	capture := &continueStreamCapture{}
	application.Stdout = capture
	baseRunner.reply = []sessions.Message{
		{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "done"}}},
	}
	runner := &duplexPiRunner{
		continueRunner: baseRunner,
		promptStarted:  make(chan struct{}, 1),
		releaseTurn:    make(chan struct{}),
	}
	application.Runner = runner

	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x", "--stream", "--duplex"})
	}()

	// Write line 1 WITHOUT closing stdin: the turn must start immediately.
	if _, err := io.WriteString(inW, continueTranscript(t, workspace)+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.promptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("duplex turn did not start on line 1 without stdin EOF")
	}

	// Closing stdin (EOF) mid-turn must NOT abort or kill the running turn.
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	close(runner.releaseTurn)
	if err := <-errCh; err != nil {
		t.Fatalf("duplex run failed after stdin EOF: %v", err)
	}
	msgs, _, final := decodeDuplexEvents(t, capture.snapshot())
	if final == nil || len(msgs) != 1 || transcript.TextContent(msgs[0]) != "done" {
		t.Fatalf("unexpected output: %s", capture.snapshot())
	}
}

type duplexPiRunner struct {
	*continueRunner
	promptStarted chan struct{}
	releaseTurn   chan struct{}
	abortMode     bool
}

func (r *duplexPiRunner) StartStream(plan runtime.CommandPlan) (*runtime.Stream, error) {
	r.plans = append(r.plans, plan)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var writeMu sync.Mutex
	emit := func(line string) {
		writeMu.Lock()
		_, _ = io.WriteString(outW, line+"\n")
		writeMu.Unlock()
	}
	provider, model, _ := strings.Cut(r.model, "/")
	sessionPath := flagValue(plan.Args, "--session")

	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var cmd struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				continue
			}
			switch cmd.Type {
			case "prompt":
				_ = r.appendTurn(sessionPath, "pi", cmd.Message, sessions.SerializePi)
				emit(`{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`)
				emit(`{"type":"session","id":"pi-duplex"}`)
				select {
				case r.promptStarted <- struct{}{}:
				default:
				}
				if !r.abortMode {
					go func() {
						<-r.releaseTurn
						emit(fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","provider":%q,"model":%q,"stopReason":"stop","content":[{"type":"text","text":"done"}]}}`, provider, model))
						emit(`{"type":"agent_end"}`)
						emit(`{"type":"agent_settled","aborted":false}`)
					}()
				}
			case "steer":
				// Append steered user + assistant messages to Pi session file.
				if parsed, err := sessions.ParseNativeSessionFile("pi", sessionPath); err == nil {
					parsed.Messages = append(parsed.Messages,
						sessions.Message{ID: "pi-u", Role: "user", Content: []sessions.ContentBlock{{Type: "text", Text: cmd.Message}}},
						sessions.Message{ID: "pi-a", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "pi saw: " + cmd.Message}}},
					)
					records := sessions.SerializePi(parsed)
					var buf bytes.Buffer
					enc := json.NewEncoder(&buf)
					for _, rec := range records {
						_ = enc.Encode(rec)
					}
					_ = os.WriteFile(sessionPath, buf.Bytes(), 0o600)
				}
				emit(`{"type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}`)
			case "abort":
				emit(`{"type":"response","command":"abort","success":true}`)
				emit(`{"type":"agent_settled","aborted":true}`)
			}
		}
	}()
	return runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil }), nil
}

func TestSessionContinueDuplexPiRPCSteerAndCancel(t *testing.T) {
	t.Run("steer", func(t *testing.T) {
		workspace := testWorkspace(t)
		inR, inW := io.Pipe()
		defer inW.Close()
		application, _, baseRunner := continueApp(t, "pi", "anthropic/claude-x", "")
		application.Stdin = inR
		capture := &continueStreamCapture{}
		application.Stdout = capture
		baseRunner.reply = []sessions.Message{
			{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "step1"}}},
		}
		runner := &duplexPiRunner{
			continueRunner: baseRunner,
			promptStarted:  make(chan struct{}, 1),
			releaseTurn:    make(chan struct{}),
		}
		application.Runner = runner

		errCh := make(chan error, 1)
		go func() {
			errCh <- application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x", "--stream", "--duplex"})
		}()

		_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
		<-runner.promptStarted
		_, _ = io.WriteString(inW, `{"type":"steer","id":"p1","message":"check pi"}`+"\n")

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, acks, _ := decodeDuplexEvents(t, capture.snapshot())
			if len(acks) == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(runner.releaseTurn)
		if err := <-errCh; err != nil {
			t.Fatalf("pi duplex steer failed: %v", err)
		}
		msgs, acks, final := decodeDuplexEvents(t, capture.snapshot())
		if len(acks) != 1 || acks[0].ID != "p1" || acks[0].Disposition != "steered" {
			t.Fatalf("unexpected pi acks: %+v", acks)
		}
		if final == nil || len(msgs) != 3 || !reflect.DeepEqual(msgs, final.Messages) {
			t.Fatalf("expected 3 chronological pi entries, got %d: %+v", len(msgs), msgs)
		}
		if msgs[1].Source != transcript.SourceUser || transcript.TextContent(msgs[1]) != "check pi" {
			t.Fatalf("missing steered user entry in pi delta: %+v", msgs)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		workspace := testWorkspace(t)
		inR, inW := io.Pipe()
		defer inW.Close()
		application, _, baseRunner := continueApp(t, "pi", "anthropic/claude-x", "")
		application.Stdin = inR
		capture := &continueStreamCapture{}
		application.Stdout = capture
		baseRunner.reply = []sessions.Message{
			{ID: "a1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "step1"}}},
		}
		runner := &duplexPiRunner{
			continueRunner: baseRunner,
			promptStarted:  make(chan struct{}, 1),
			releaseTurn:    make(chan struct{}),
			abortMode:      true,
		}
		application.Runner = runner

		errCh := make(chan error, 1)
		go func() {
			errCh <- application.Run(context.Background(), []string{"session", "continue", "pi", "--model", "anthropic/claude-x", "--stream", "--duplex"})
		}()

		_, _ = io.WriteString(inW, continueTranscript(t, workspace)+"\n")
		<-runner.promptStarted
		// Wait until the turn is active and streaming before sending native cancel.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			msgs, _, _ := decodeDuplexEvents(t, capture.snapshot())
			if len(msgs) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = io.WriteString(inW, `{"type":"cancel"}`+"\n")

		err := <-errCh
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("expected pi abort error, got %v", err)
		}
		_, _, final := decodeDuplexEvents(t, capture.snapshot())
		if final != nil {
			t.Fatalf("aborted pi turn must not emit result")
		}
	})
}

func TestContinueStreamWriterConcurrentLockingAndTerminalBarrier(t *testing.T) {
	var buf bytes.Buffer
	w := newContinueStreamWriter(&buf)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = w.WriteEvent(duplexSteerAck{
					Type:        "steer_ack",
					ID:          fmt.Sprintf("g%d-%d", id, i),
					Disposition: "steered",
				})
			}
		}(g)
	}
	wg.Wait()

	if err := w.WriteTerminal(struct {
		Type    string                   `json:"type"`
		Session sessions.ProtocolSession `json:"session"`
	}{"result", sessions.ProtocolSession{ProtocolVersion: transcript.ProtocolVersion, ID: "done", Provider: "claude"}}); err != nil {
		t.Fatal(err)
	}
	// Post-terminal writes must be silently dropped so nothing follows result.
	_ = w.WriteEvent(duplexSteerAck{Type: "steer_ack", ID: "late", Disposition: "idle"})

	_, acks, final := decodeDuplexEvents(t, buf.String())
	if len(acks) != 200 || final == nil || final.ID != "done" {
		t.Fatalf("expected 200 acks + terminal result, got acks=%d final=%+v", len(acks), final)
	}
}

func TestSessionContinueNonDuplexMultilineStdinBackwardsCompat(t *testing.T) {
	workspace := testWorkspace(t)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(continueTranscript(t, workspace)), "", "  "); err != nil {
		t.Fatal(err)
	}
	application, _, runner := continueApp(t, "claude", "", pretty.String())
	capture := &continueStreamCapture{}
	application.Stdout = capture
	runner.reply = []sessions.Message{{ID: "r1", Role: "assistant", Content: []sessions.ContentBlock{{Type: "text", Text: "compat ok"}}}}

	if err := application.Run(context.Background(), []string{"session", "continue", "claude", "--stream"}); err != nil {
		t.Fatalf("non-duplex multiline stdin failed: %v", err)
	}
	msgs, _, final := decodeDuplexEvents(t, capture.snapshot())
	if final == nil || len(msgs) != 1 || transcript.TextContent(msgs[0]) != "compat ok" {
		t.Fatalf("unexpected non-duplex output: %s", capture.snapshot())
	}
}
