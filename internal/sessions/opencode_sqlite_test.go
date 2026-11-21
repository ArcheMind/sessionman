package sessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCodeSQLiteClosedSessionCanBePreviewedAndResumed(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	root := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	for _, row := range []struct{ id, parent string }{{"ses_root", ""}, {"ses_child", "ses_root"}} {
		if _, err := db.Exec(`INSERT INTO session VALUES (?, ?, ?, 'Native title', ?, ?)`, row.id, row.parent, workspace, stamp.UnixMilli(), stamp.Add(time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ id, role string }{{"msg_user", "user"}, {"msg_assistant", "assistant"}} {
		if _, err := db.Exec(`INSERT INTO message VALUES (?, 'ses_root', ?, ?)`, row.id, stamp.UnixMilli(), `{"role":"`+row.role+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	for index, row := range []struct{ id, message, data string }{
		{"part_user", "msg_user", `{"type":"text","text":"Searchable native request"}`},
		{"part_reason", "msg_assistant", `{"type":"reasoning","text":"Native reasoning"}`},
		{"part_tool", "msg_assistant", `{"type":"tool","callID":"call_native","tool":"read","state":{"status":"completed","input":{"path":"file"},"output":"tool output"}}`},
		{"part_text", "msg_assistant", `{"type":"text","text":"Native answer"}`},
	} {
		if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, 'ses_root', ?, ?)`, row.id, row.message, stamp.Add(time.Duration(index)*time.Second).UnixMilli(), row.data); err != nil {
			t.Fatal(err)
		}
	}
	service := Service{HomeDir: home}
	if err := service.SaveClosed("opencode", "ses_root", stamp.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	groups, err := service.RecentGroups(workspace, 10, false)
	if err != nil || len(groups.Current) != 1 || len(groups.Global) != 0 {
		t.Fatalf("SQLite catalog or subagent filter failed: %+v, %v", groups, err)
	}
	item := groups.Current[0]
	if item.ID != "ses_root" || item.ClosedAt == "" || item.TurnCount != 1 || item.LatestUserMessage != "Searchable native request" || len(item.UserPrompts) != 1 {
		t.Fatalf("closed SQLite session lacks preview/search metadata: %+v", item)
	}
	if item.StartedAt != stamp.Format(time.RFC3339Nano) || item.UpdatedAt != stamp.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("SQLite millisecond timestamps changed: %+v", item)
	}
	detail, err := service.Read(item)
	if err != nil || detail.ID != "ses_root" || detail.Workspace != workspace || len(detail.Messages) != 3 {
		t.Fatalf("closed session cannot be opened: %+v, %v", detail, err)
	}
	var thinking, tool bool
	for _, message := range detail.Messages {
		for _, block := range message.Content {
			thinking = thinking || block.Type == "thinking" && block.Text == "Native reasoning"
		}
		tool = tool || message.Role == "tool" && message.ToolUseID == "call_native" && message.Result == "tool output"
	}
	if !thinking || !tool {
		t.Fatalf("reading SQLite session lost reasoning or tool output: %+v", detail.Messages)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("read-only session access changed source: %v", err)
	}
}
