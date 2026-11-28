package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunTurnValidation(t *testing.T) {
	if _, err := RunTurn(context.Background(), TurnRequest{}); err == nil {
		t.Fatal("expected error for empty agent")
	}
	if _, err := RunTurn(context.Background(), TurnRequest{Agent: "unknown"}); err == nil {
		t.Fatal("expected error for unsupported agent")
	}
}

func TestRunTurnAGYACP(t *testing.T) {
	dir := t.TempDir()
	mockBin := filepath.Join(dir, "agy")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-sdk"}}\n' "$id"
      ;;
    *'"method":"session/set_model"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-sdk","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"VERDICT: PASS"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(mockBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := RunTurn(context.Background(), TurnRequest{
		Agent:  "agy",
		Cwd:    dir,
		Model:  "gemini-flash",
		Mode:   ModeYolo,
		Prompt: "check",
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !res.Completed || res.Text != "VERDICT: PASS" {
		t.Fatalf("got %+v, want Completed=true Text='VERDICT: PASS'", res)
	}
}
