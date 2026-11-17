package sessions

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionQueriesLoadOnlyRequiredSummaries(t *testing.T) {
	original := agentSpecs
	t.Cleanup(func() { agentSpecs = original })
	var previews, counts []string
	var failID string
	agentSpecs = []agentSpec{{
		id: "fixture",
		discoverIn: func(string, bool) ([]Summary, error) {
			return []Summary{
				{ID: "old", Agent: "fixture", Workspace: "/work", UpdatedAt: "2026-11-01"},
				{ID: "new", Agent: "fixture", Workspace: "/work", UpdatedAt: "2026-11-02"},
				{ID: "elsewhere", Agent: "fixture", Workspace: "/other", UpdatedAt: "2026-11-03"},
				{ID: "child", Agent: "fixture", IsSubagent: true, Workspace: "/work", UpdatedAt: "2026-11-04"},
			}, nil
		},
		summarize: func(item Summary, includeCount bool) (Summary, error) {
			previews = append(previews, item.ID)
			if item.ID == failID {
				return item, os.ErrPermission
			}
			if includeCount {
				counts = append(counts, item.ID)
				item.MessageCount = map[string]int{"old": 10, "new": 1, "elsewhere": 3}[item.ID]
			}
			return item, nil
		},
		read: func(string) (Session, error) {
			t.Fatal("listing loaded a full transcript")
			return Session{}, nil
		},
	}}
	service := Service{}
	t.Run("recent", func(t *testing.T) {
		previews, counts = nil, nil
		groups, err := service.RecentGroups("/work", 1, false)
		if err != nil || len(groups.Current) != 1 || groups.Current[0].ID != "new" || len(groups.Global) != 1 || groups.Global[0].ID != "elsewhere" {
			t.Fatalf("groups = %+v, error = %v", groups, err)
		}
		if !reflect.DeepEqual(previews, []string{"new", "elsewhere"}) || len(counts) != 0 {
			t.Fatalf("preview reads = %v, count reads = %v", previews, counts)
		}
	})
	t.Run("date", func(t *testing.T) {
		previews, counts = nil, nil
		items, err := service.List(ListOptions{Workspace: "/work", Limit: 1})
		if err != nil || len(items) != 1 || items[0].ID != "new" || !reflect.DeepEqual(counts, []string{"new"}) {
			t.Fatalf("items = %+v, reads = %v, error = %v", items, counts, err)
		}
	})
	t.Run("messages", func(t *testing.T) {
		previews, counts = nil, nil
		items, err := service.List(ListOptions{Workspace: "/work", Limit: 1, Sort: "messages"})
		if err != nil || len(items) != 1 || items[0].ID != "old" || !reflect.DeepEqual(counts, []string{"new", "old"}) {
			t.Fatalf("items = %+v, reads = %v, error = %v", items, counts, err)
		}
	})
	t.Run("failed count does not consume limit", func(t *testing.T) {
		failID = "new"
		previews, counts = nil, nil
		items, err := service.List(ListOptions{Workspace: "/work", Limit: 1})
		if !IsCatalogError(err) || len(items) != 1 || items[0].ID != "old" || items[0].MessageCount != 10 {
			t.Fatalf("items = %+v, error = %v", items, err)
		}
		if !reflect.DeepEqual(previews, []string{"new", "old"}) {
			t.Fatalf("preview reads = %v", previews)
		}
	})
}

func TestReadSelectedSessionDoesNotRescanCatalog(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "14", "selected.jsonl")
	writeFixture(t, path, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"selected","cwd":"/work"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"selected request"}}`,
	}, "\n"))
	service := Service{HomeDir: home}
	groups, err := service.RecentGroups("/work", 1, false)
	if err != nil || len(groups.Current) != 1 {
		t.Fatalf("groups = %+v, error = %v", groups, err)
	}
	// A broken neighboring entry must not affect reading an already selected source.
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "broken.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Read(groups.Current[0])
	if err != nil || detail.ID != "selected" || MessageText(detail.Messages[0]) != "selected request" {
		t.Fatalf("detail = %+v, error = %v", detail, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n" + `{"type":"response_item","timestamp":"2026-11-14T12:00:00Z","payload":{"type":"message","role":"assistant","content":"fresh answer"}}`)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatalf("append: %v, close: %v", err, closeErr)
	}
	detail, err = service.Read(groups.Current[0])
	if err != nil || detail.UpdatedAt != "2026-11-14T12:00:00Z" || len(detail.Messages) != 2 {
		t.Fatalf("selected source was not refreshed: %+v, %v", detail, err)
	}
	writeFixture(t, path, `{"type":"session_meta","payload":{"id":"replacement","cwd":"/work"}}`)
	if _, err := service.Read(groups.Current[0]); err == nil || !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("changed source identity accepted: %v", err)
	}
}

func TestClaudeDiscoverySkipsHiddenSubagentFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "projects", "project")
	writeFixture(t, filepath.Join(root, "root.jsonl"), `{"type":"user","sessionId":"root","cwd":"/work","message":{"content":"request"}}`)
	broken := filepath.Join(root, "root", "subagents", "broken.jsonl")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	service := Service{HomeDir: home}
	groups, err := service.RecentGroups("/work", 1, false)
	if err != nil || len(groups.Current) != 1 {
		t.Fatalf("hidden entry affected discovery: %+v, %v", groups, err)
	}
	if _, err := service.RecentGroups("/work", 1, true); !IsCatalogError(err) {
		t.Fatalf("included unreadable entry did not report an error: %v", err)
	}
}
