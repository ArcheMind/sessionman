package app

import (
	"strings"
	"testing"
	"time"

	"github.com/ArcheMind/agentx/internal/sessions"
	"github.com/mattn/go-runewidth"
)

func TestLastClosedMarkerFollowsTitleAndReservesWidth(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC)
	groups := []sessionGroup{{Heading: "Current workspace", Items: []sessions.Summary{
		{ID: "closed", Agent: "codex", UpdatedAt: "2026-11-20T09:00:00Z", ClosedAt: "2026-11-20T14:00:00Z", FirstUserMessage: strings.Repeat("首", 40), LatestUserMessage: strings.Repeat("尾", 40)},
		{ID: "ordinary", Agent: "claude", Title: "ordinary session"},
	}}}
	rows := buildContentRows(groups, now)
	closed := rows[2]
	if closed.updated != displaySessionTime(groups[0].Items[0].ClosedAt, now) {
		t.Fatalf("row uses activity time instead of close time: %+v", closed)
	}
	for _, width := range []int{60, 100, 160} {
		row := fitSessionRow(closed, width)
		plain := styledContentRow(row, true, false)
		if !strings.HasSuffix(plain, " [last closed]") || runewidth.StringWidth(plain) > width {
			t.Fatalf("width=%d text=%q", width, plain)
		}
		colored := styledContentRow(row, true, true)
		if !strings.Contains(colored, ansiGreen+"[last closed]") {
			t.Fatalf("marker lacks its own green color: %q", colored)
		}
	}
	if strings.Contains(styledContentRow(rows[3], false, false), "[last closed]") {
		t.Fatal("ordinary session has a close marker")
	}
	_, items := buildHomeRows(nil, nil, nil, groups, now)
	if len(items) != 3 || items[1].session.ID != "closed" {
		t.Fatalf("marker row no longer selects the original session: %+v", items)
	}
}
