package sessions

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClosedSessionsKeepLatestAcrossProvidersAndConcurrentExits(t *testing.T) {
	service := Service{HomeDir: t.TempDir()}
	if _, ok, err := service.LatestClosed(); err != nil || ok {
		t.Fatalf("empty history: found=%v error=%v", ok, err)
	}
	start := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			if err := service.SaveClosed("codex", fmt.Sprintf("session-%d", i), start.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	latest, ok, err := service.LatestClosed()
	if err != nil || !ok || latest.Agent != "codex" || latest.ID != "session-11" {
		t.Fatalf("latest concurrent exit: %+v found=%v error=%v", latest, ok, err)
	}
	if err := service.SaveClosed("claude", "session-11", start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveClosed("codex", "session-11", start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	db, err := service.closedDatabase(true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM closed_sessions`).Scan(&count); err != nil || count != 13 {
		t.Fatalf("records should overwrite per provider/session: count=%d error=%v", count, err)
	}
	latest, ok, err = service.LatestClosed()
	if err != nil || !ok || latest.Agent != "codex" || !latest.ClosedAt.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("latest reopened session: %+v found=%v error=%v", latest, ok, err)
	}
	for i := range 12 {
		workers.Go(func() {
			if err := service.SaveClosed("codex", "session-11", start.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	latest, ok, err = service.LatestClosed()
	if err != nil || !ok || latest.Agent != "codex" || !latest.ClosedAt.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("delayed earlier exits overwrote the latest close: %+v found=%v error=%v", latest, ok, err)
	}
}

func TestRecentClosedSessionPinsBeyondLimitWithoutChangingNativeTimes(t *testing.T) {
	home, workspace, other := t.TempDir(), t.TempDir(), t.TempDir()
	writeRollout := func(id, cwd, timestamp string) {
		writeFixture(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "20", id+".jsonl"), strings.Join([]string{
			fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":"2026-11-20T08:00:00Z","source":"cli"}}`, id, cwd),
			fmt.Sprintf(`{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"user","content":"retained searchable request"}}`, timestamp),
		}, "\n"))
	}
	writeRollout("newer", workspace, "2026-11-20T12:00:00Z")
	writeRollout("older-closed", other, "2026-11-20T09:00:00Z")
	writeRollout("other", other, "2026-11-20T10:00:00Z")
	service := Service{HomeDir: home}
	closedAt := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	if err := service.SaveClosed("codex", "older-closed", closedAt); err != nil {
		t.Fatal(err)
	}
	groups, err := service.RecentGroups(workspace, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Current) != 1 || groups.Current[0].ID != "older-closed" || groups.Current[0].ClosedAt != closedAt.Format(time.RFC3339Nano) {
		t.Fatalf("missing pinned session beyond limit: %+v", groups)
	}
	if groups.Current[0].UpdatedAt != "2026-11-20T09:00:00Z" || len(groups.Current[0].UserPrompts) != 1 {
		t.Fatalf("native time or prompt search changed: %+v", groups.Current[0])
	}
	if len(groups.Global) != 1 || groups.Global[0].ID != "other" {
		t.Fatalf("duplicate closed session or lost global entry: %+v", groups.Global)
	}
	list, err := service.List(ListOptions{All: true})
	if err != nil || len(list) != 3 || list[0].ID != "newer" {
		t.Fatalf("session list sorting changed: %+v error=%v", list, err)
	}
	if err := service.SaveClosed("codex", "deleted", closedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	groups, err = service.RecentGroups(workspace, 1, false)
	if err != nil || len(groups.Current) != 1 || groups.Current[0].ID != "newer" || groups.Current[0].ClosedAt != "" {
		t.Fatalf("missing native session should not mark an older close: %+v error=%v", groups, err)
	}
}
