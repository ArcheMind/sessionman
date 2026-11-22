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
