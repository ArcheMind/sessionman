package sessions

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Defect C: listing and the agent root follow the user's CODEX_HOME, the
// same root import and show use.
func TestListFollowsUserCodexHome(t *testing.T) {
	home, codexHome, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeFixture(t, filepath.Join(codexHome, "sessions", "2026", "09", "01", "rollout.jsonl"),
		fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-09-01T11:00:00Z","payload":{"id":"codex-id","cwd":%q}}`+"\n"+
			`{"type":"response_item","timestamp":"2026-09-01T11:01:00Z","payload":{"type":"message","role":"user","content":"hello"}}`, workspace))
	service := Service{HomeDir: home, LookPath: func(string) (string, error) { return "", fmt.Errorf("absent") }}
	items, err := service.List(ListOptions{Agent: "codex", All: true})
	if err != nil || len(items) != 1 || items[0].ID != "codex-id" {
		t.Fatalf("items = %+v, err = %v", items, err)
	}
	for _, agent := range service.Agents() {
		if agent.ID == "codex" && agent.Root != filepath.Join(codexHome, "sessions") {
			t.Fatalf("codex root = %q", agent.Root)
		}
	}
}
