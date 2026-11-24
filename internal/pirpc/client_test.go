package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/runtime"
)

func TestRunTurnSucceedsAndChecksModel(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var cmd map[string]any
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				continue
			}
			if cmd["type"] == "prompt" {
				_, _ = io.WriteString(outW, `{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"session","id":"pi-native"}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"message_end","message":{"role":"assistant","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"agent_end"}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"agent_settled","aborted":false}`+"\n")
			}
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })
	var seen []string
	res, err := RunTurn(context.Background(), stream, "hi", "anthropic/claude-x", func(line string) error {
		seen = append(seen, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.SessionID != "pi-native" || res.Failure != "" {
		t.Fatalf("res=%+v", res)
	}
	if len(seen) != 4 {
		t.Fatalf("seen=%d", len(seen))
	}
}

func TestRunTurnDetectsModelMismatchAndAbort(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			_, _ = io.WriteString(outW, `{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`+"\n")
			_, _ = io.WriteString(outW, `{"type":"message_end","message":{"role":"assistant","provider":"wrong","model":"m","stopReason":"stop","content":[{"type":"text","text":"x"}]}}`+"\n")
			_, _ = io.WriteString(outW, `{"type":"agent_settled","aborted":false}`+"\n")
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })
	res, err := RunTurn(context.Background(), stream, "hi", "anthropic/claude-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Failure, "Pi returned model wrong/m instead of anthropic/claude-x") {
		t.Fatalf("res=%+v", res)
	}
}

func TestRunTurnWithControlSteerAndIdleAfterSettle(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	finishTurn := make(chan struct{})
	var steered []string
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var cmd map[string]any
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				continue
			}
			switch cmd["type"] {
			case "prompt":
				_, _ = io.WriteString(outW, `{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"session","id":"pi-steer"}`+"\n")
				go func() {
					<-finishTurn
					_, _ = io.WriteString(outW, `{"type":"message_end","message":{"role":"assistant","provider":"anthropic","model":"claude-x","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`+"\n")
					_, _ = io.WriteString(outW, `{"type":"agent_end"}`+"\n")
					_, _ = io.WriteString(outW, `{"type":"agent_settled","aborted":false}`+"\n")
				}()
			case "steer":
				msg, _ := cmd["message"].(string)
				steered = append(steered, msg)
				if msg == "already-idle" {
					_, _ = io.WriteString(outW, `{"type":"response","command":"steer","success":true,"data":{"disposition":"idle"}}`+"\n")
				} else {
					_, _ = io.WriteString(outW, `{"type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}`+"\n")
				}
			}
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })

	ctrlCh := make(chan TurnControl, 1)
	doneCh := make(chan TurnResult, 1)
	go func() {
		res, err := RunTurnWithControl(context.Background(), stream, "hi", "anthropic/claude-x", nil, func(tc TurnControl) {
			ctrlCh <- tc
		})
		if err != nil {
			t.Errorf("RunTurnWithControl err: %v", err)
		}
		doneCh <- res
	}()

	tc := <-ctrlCh
	if disp, err := tc.Steer(context.Background(), "nudge"); err != nil || disp != "steered" {
		t.Fatalf("expected steered, got disp=%q err=%v", disp, err)
	}
	if disp, err := tc.Steer(context.Background(), "already-idle"); err != nil || disp != "idle" {
		t.Fatalf("expected idle, got disp=%q err=%v", disp, err)
	}
	close(finishTurn)
	res := <-doneCh
	if !res.Completed || res.SessionID != "pi-steer" || res.Failure != "" {
		t.Fatalf("res=%+v", res)
	}
	// Steering after settlement must return idle cleanly.
	if disp, err := tc.Steer(context.Background(), "after-settle"); err != nil || disp != "idle" {
		t.Fatalf("expected idle after settle, got disp=%q err=%v", disp, err)
	}
	if len(steered) != 2 {
		t.Fatalf("expected 2 steer commands sent, got %v", steered)
	}
}

func TestRunTurnWithControlCancelAbortsTurn(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer outW.Close()
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var cmd map[string]any
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				continue
			}
			switch cmd["type"] {
			case "prompt":
				_, _ = io.WriteString(outW, `{"type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`+"\n")
			case "abort":
				_, _ = io.WriteString(outW, `{"type":"response","command":"abort","success":true}`+"\n")
				_, _ = io.WriteString(outW, `{"type":"agent_settled","aborted":true}`+"\n")
			}
		}
	}()
	stream := runtime.NewPipeStream(inW, outR, func() string { return "" }, func() { _ = inW.Close(); _ = outW.Close() }, func() error { return nil })

	ctrlCh := make(chan TurnControl, 1)
	resCh := make(chan TurnResult, 1)
	go func() {
		res, err := RunTurnWithControl(context.Background(), stream, "hi", "anthropic/claude-x", nil, func(tc TurnControl) {
			ctrlCh <- tc
		})
		if err != nil {
			t.Errorf("RunTurnWithControl err: %v", err)
		}
		resCh <- res
	}()

	tc := <-ctrlCh
	if err := tc.Cancel(); err != nil {
		t.Fatalf("cancel err: %v", err)
	}
	res := <-resCh
	if res.Failure != "pi run aborted" {
		t.Fatalf("expected pi run aborted, got %+v", res)
	}
}
