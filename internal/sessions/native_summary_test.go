package sessions

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgyRecentDoesNotReadExternalTranscriptResources(t *testing.T) {
	for _, resource := range []string{"user image", "tool output"} {
		t.Run(resource, func(t *testing.T) {
			home := t.TempDir()
			source := agyTestSession()
			id, err := WriteAgySessionFile(source, agyRoot(home))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(agyRoot(home), "conversations", id+".db")
			before, err := ParseAgySessionFile(path)
			if err != nil {
				t.Fatal(err)
			}
			db, err := agyOpen(path, false)
			if err != nil {
				t.Fatal(err)
			}
			missing := agyFileURL(filepath.Join(home, "missing-resource"))
			kind := 14
			if resource == "tool output" {
				kind = 132
			}
			var idx int
			var raw []byte
			if err := db.QueryRow(`SELECT idx,step_payload FROM steps WHERE step_type=? LIMIT 1`, kind).Scan(&idx, &raw); err != nil {
				t.Fatal(err)
			}
			p, err := agyDecode(raw)
			if err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if resource == "user image" {
				user, err := agyDecode(p.bytes(19))
				if err != nil {
					t.Fatal(err)
				}
				image := agyJoin(agyText(1, "image/png"), agyText(5, missing.String()))
				payload = agyJoin(agyInt(1, 14), agyBytes(5, p.bytes(5)), agyBytes(19, agyJoin(agyText(2, user.text(2)), agyBytes(9, image))))
			} else {
				payload = agyJoin(agyInt(1, 132), agyBytes(5, p.bytes(5)), agyBytes(140, agyBytes(2, agyText(7, missing.String()))))
			}
			if _, err := db.Exec(`UPDATE steps SET step_payload=? WHERE idx=?`, payload, idx); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			service := Service{HomeDir: home}
			groups, err := service.RecentGroups(source.Workspace, 10, false)
			if err != nil || len(groups.Current) != 1 {
				t.Fatalf("recent depends on external resource: %+v %v", groups, err)
			}
			item := groups.Current[0]
			if item.TurnCount != 1 || item.ID != id || item.FirstUserMessage != "retain marker" || item.LatestUserMessage != "retain marker" || item.StartedAt != before.StartedAt || item.UpdatedAt != before.UpdatedAt {
				t.Fatalf("summary changed: %+v", item)
			}
			if !reflect.DeepEqual(item.UserPrompts, []string{"retain marker"}) {
				t.Fatalf("recent prompts = %#v, want the user prompt only", item.UserPrompts)
			}
			list, err := service.List(ListOptions{Agent: "agy", All: true})
			if err != nil || len(list) != 1 || list[0].MessageCount != before.MessageCount {
				t.Fatalf("count reads external resource: %+v %v", list, err)
			}
			if _, err := service.Info(id, "agy"); err == nil {
				t.Fatal("detail silently lost the missing transcript resource")
			}
		})
	}
}

func TestNativeSummariesPreserveMessageCounts(t *testing.T) {
	for _, path := range []string{"testdata/agy/native-v1.5.8.db", "testdata/agy/native-fidelity-v1.5.8.db"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			path := agySummaryFixture(t, path)
			detail, err := ParseAgySessionFile(path)
			if err != nil {
				t.Fatal(err)
			}
			item, err := agySummaryFile(path)
			if err != nil {
				t.Fatal(err)
			}
			item, err = summarizeAgy(item, true)
			if err != nil {
				t.Fatal(err)
			}
			want := SummaryOf(detail)
			if item.MessageCount != want.MessageCount || item.FirstUserMessage != want.FirstUserMessage || item.LatestUserMessage != want.LatestUserMessage || item.StartedAt != want.StartedAt || item.UpdatedAt != want.UpdatedAt {
				t.Fatalf("summary = %+v, want %+v", item, want)
			}
		})
	}
}

func TestAgySummaryUsesPayloadIdentityAndMetadata(t *testing.T) {
	for _, columns := range []string{"missing metadata", "stale index fields"} {
		t.Run(columns, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.db")
			source := agyTestSession()
			source.Messages = append(source.Messages, Message{ID: "last", Role: "user", Timestamp: "2026-11-13T12:00:03Z", Content: []ContentBlock{{Type: "text", Text: "latest request"}}})
			if err := WriteAgyDatabaseFile(source, path); err != nil {
				t.Fatal(err)
			}
			db, err := agyOpen(path, false)
			if err != nil {
				t.Fatal(err)
			}
			var metadata []byte
			if columns == "stale index fields" {
				metadata = agyBytes(1, agyTimestamp("2099-01-01T00:00:00Z"))
			}
			if _, err := db.Exec(`UPDATE steps SET step_type=0,metadata=?`, metadata); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			detail, err := ParseAgySessionFile(path)
			if err != nil {
				t.Fatal(err)
			}
			item, err := agySummaryFile(path)
			if err != nil {
				t.Fatal(err)
			}
			item, err = summarizeAgy(item, true)
			if err != nil {
				t.Fatal(err)
			}
			want := SummaryOf(detail)
			if item.MessageCount != want.MessageCount || item.FirstUserMessage != want.FirstUserMessage || item.LatestUserMessage != want.LatestUserMessage || item.StartedAt != want.StartedAt || item.UpdatedAt != want.UpdatedAt {
				t.Fatalf("summary trusts redundant columns: %+v, want %+v", item, want)
			}
		})
	}
}

func agySummaryFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := []byte("file:///" + strings.Repeat("X", 232))
	offset := bytes.Index(data, placeholder)
	if offset < 0 {
		return path
	}
	dir := t.TempDir()
	prefix := agyFileURL(filepath.Join(dir, "result-"))
	uri := prefix.String()
	if len(uri) >= len(placeholder) {
		t.Fatal("fixture resource URI exceeds reserved field width")
	}
	uri += strings.Repeat("x", len(placeholder)-len(uri))
	if err := os.WriteFile(agyWorkspace(uri), []byte("complete native tool result"), 0600); err != nil {
		t.Fatal(err)
	}
	copy(data[offset:offset+len(placeholder)], uri)
	copyPath := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(copyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return copyPath
}

func TestReadAgySessionRefreshesTitleWithoutRescanning(t *testing.T) {
	home := t.TempDir()
	id, err := WriteAgySessionFile(agyTestSession(), agyRoot(home))
	if err != nil {
		t.Fatal(err)
	}
	service := Service{HomeDir: home}
	groups, err := service.RecentGroups("/tmp/agy-fixture", 10, false)
	if err != nil || len(groups.Current) != 1 {
		t.Fatalf("recent = %+v %v", groups, err)
	}
	db, err := agyOpen(filepath.Join(agyRoot(home), "conversation_summaries.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE conversation_summaries SET title=? WHERE conversation_id=?`, "updated native title", id); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agyRoot(home), "conversations", "unrelated.db"), []byte("corrupt sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Read(groups.Current[0])
	if err != nil {
		t.Fatal(err)
	}
	if detail.ID != id || detail.Title != "updated native title" {
		t.Fatalf("read stale title or wrong source: %+v", detail)
	}
}
