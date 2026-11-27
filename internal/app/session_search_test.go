package app

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ArcheMind/sessionman/internal/sessions"
)

func searchFixtureRows() []contentRow {
	return []contentRow{
		{text: "Presets", itemIndex: -1, kind: contentRowHeading},
		{text: "cached preset", itemIndex: 0, kind: contentRowPreset},
		{text: "Current workspace", itemIndex: -1, kind: contentRowHeading},
		{title: "缓存 performance", itemIndex: 1, kind: contentRowSession, userPrompts: []string{"unrelated"}},
		{title: "First request", itemIndex: 2, kind: contentRowSession, userPrompts: []string{"修复缓存层", "improve performance", "jqmet"}},
		{text: "Global", itemIndex: -1, kind: contentRowHeading},
		{title: "Other request", itemIndex: 3, kind: contentRowSession, userPrompts: []string{"缓存", "performance", "split", "boundary"}},
	}
}

func TestFilterSessionPrompts(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []int
	}{
		{"缓存 PERFORMANCE", []int{2, 3}},
		{"缓存层", []int{2}},
		{"splitboundary", nil},
		{"missing", nil},
		{" \t", []int{0, 1, 2, 3}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rows := filterSessionPrompts(context.Background(), searchFixtureRows(), tc.query)
			got := selectorItemIndices(rows)
			if len(got) != len(tc.want) {
				t.Fatalf("indices = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("indices = %v, want %v", got, tc.want)
				}
			}
			if tc.query == "缓存层" && (len(rows) != 2 || rows[0].text != "Current workspace") {
				t.Fatalf("unmatched groups remain: %+v", rows)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := filterSessionPrompts(ctx, searchFixtureRows(), "缓存"); got != nil {
		t.Fatalf("cancelled search returned %d rows", len(got))
	}
}

func TestSelectorPromptSearch(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        int
	}{
		{"unicode and multiple terms", "/缓存 PERFORMANCE\r", 2},
		{"query letters are not commands", "/jqmet\r", 2},
		{"unicode backspace", "/缓存错\x7f\r", 2},
		{"arrow after query", "/缓存\x1b[B\r", 3},
		{"clear no matches", "/missing\r\x1b\r", 0},
		{"restore after delete", "/missing" + strings.Repeat("\x7f", 7) + "\r", 0},
		{"latest query wins", "/missing" + strings.Repeat("\x7f", 7) + "jqmet\r", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application, _, _ := presetApp(t, tc.input+"next prompt\n")
			reader := bufio.NewReader(application.Stdin)
			index, action, err := application.runSelector(reader, selector{search: true, marker: "> "}, searchFixtureRows(), 4, nil)
			if err != nil || index != tc.want || action != selectionConfirm {
				t.Fatalf("index=%d action=%d error=%v; want index=%d", index, action, err, tc.want)
			}
			if got, _ := reader.ReadString('\n'); got != "next prompt\n" {
				t.Fatalf("search consumed next prompt: %q", got)
			}
		})
	}
}

func TestHomePromptSearchReturnsOriginalSession(t *testing.T) {
	application, _, _ := presetApp(t, "/middle\r")
	groups := []sessionGroup{{Heading: "Current workspace", Items: []sessions.Summary{
		{ID: "first", Title: "middle", UserPrompts: []string{"unrelated"}},
		{ID: "second", Title: "different title", UserPrompts: []string{"first", "middle request", "latest"}},
	}}}
	item, _, _, err := application.chooseHome(bufio.NewReader(application.Stdin), application.Hooks.Home, groups, 0)
	if err != nil || item.kind != homeSession || item.session.ID != "second" {
		t.Fatalf("selected=%+v, error=%v", item, err)
	}
}

func TestPromptSearchCancelAndOtherMenus(t *testing.T) {
	application, _, _ := presetApp(t, "/缓存\x03")
	_, _, err := application.runSelector(bufio.NewReader(application.Stdin), selector{search: true}, searchFixtureRows(), 4, nil)
	if !errors.Is(err, errSelectionCancelled) {
		t.Fatalf("Ctrl-C error=%v", err)
	}
	application, _, _ = presetApp(t, "/j\r")
	index, err := application.pick(bufio.NewReader(application.Stdin), "Other menu", []string{"first", "second"})
	if err != nil || index != 1 {
		t.Fatalf("other menu index=%d, error=%v", index, err)
	}
}

func BenchmarkSessionPromptSearch(b *testing.B) {
	var rows []contentRow
	if home := os.Getenv("SM_SEARCH_PERF_HOME"); home != "" {
		started := time.Now()
		groups, err := (sessions.Service{HomeDir: home}).RecentGroups(os.Getenv("SM_SEARCH_PERF_WORKSPACE"), recentSessionsPerAgent, false)
		if err != nil {
			b.Fatal(err)
		}
		rows = buildContentRows([]sessionGroup{{Heading: "Current workspace", Items: groups.Current}, {Heading: "Global", Items: groups.Global}}, time.Now())
		b.Logf("recent sessions preparation: %s", time.Since(started))
	} else {
		for i := range 80 {
			rows = append(rows, contentRow{itemIndex: i, kind: contentRowSession, userPrompts: []string{strings.Repeat("prompt body 缓存 performance ", 32768)}})
		}
	}
	bytes, prompts := 0, 0
	for _, row := range rows {
		for _, prompt := range row.userPrompts {
			bytes += len(prompt)
			prompts++
		}
	}
	b.Logf("sessions=%d prompts=%d searchable_bytes=%d", len(selectorItemIndices(rows)), prompts, bytes)
	for _, query := range []string{"缓存 performance", "no-match-4b7a592e", "缓存 no-match-4b7a592e"} {
		b.Run(query, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				filterSessionPrompts(context.Background(), rows, query)
			}
		})
	}
	b.Run("input-filter-redraw", func(b *testing.B) {
		if len(selectorItemIndices(rows)) == 0 {
			b.Skip("no sessions")
		}
		b.ReportAllocs()
		for b.Loop() {
			input := strings.NewReader("/no-match-4b7a592e\r\x1b\r")
			application := New(false, input, io.Discard, io.Discard)
			_, _, err := application.runSelector(bufio.NewReader(input), selector{search: true, marker: "> "}, rows, len(selectorItemIndices(rows)), nil)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
