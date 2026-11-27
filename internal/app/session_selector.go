package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/ArcheMind/sessionman/internal/presets"
	"github.com/ArcheMind/sessionman/internal/sessions"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const recentSessionsPerAgent = 10

type sessionGroup struct {
	Heading string
	Items   []sessions.Summary
}

func (a App) recentSessionGroups(includeSubagents bool) ([]sessionGroup, error) {
	items, err := a.Sessions.RecentGroups("", recentSessionsPerAgent, includeSubagents)
	if err != nil {
		if !sessions.IsCatalogError(err) {
			return nil, err
		}
		fmt.Fprintf(a.Stderr, "warning: partial session catalog: %s\n", err)
	}
	return []sessionGroup{
		{Heading: "Current workspace", Items: items.Current},
		{Heading: "Global", Items: items.Global},
	}, nil
}

type contentRow struct {
	text        string
	itemIndex   int // selectable item index; -1 for structural rows
	kind        contentRowKind
	agent       string
	updated     string
	workspace   string
	title       string
	firstUser   string
	latestUser  string
	turnCount   int
	userPrompts []string
	lastClosed  bool
}

type contentRowKind int

const (
	contentRowPlain contentRowKind = iota
	contentRowHeading
	contentRowEmpty
	contentRowSession
	contentRowPreset
	contentRowCreate
	contentRowProblem
)

func buildContentRows(groups []sessionGroup, now time.Time) []contentRow {
	var rows []contentRow
	itemIndex := 0
	for _, group := range groups {
		rows = append(rows, contentRow{text: "", itemIndex: -1})
		rows = append(rows, contentRow{text: group.Heading, itemIndex: -1, kind: contentRowHeading})
		if len(group.Items) == 0 {
			rows = append(rows, contentRow{text: "  No recent sessions", itemIndex: -1, kind: contentRowEmpty})
			continue
		}
		for _, item := range group.Items {
			title := item.Title
			if title == "" {
				title = item.ID
			}
			first := item.FirstUserMessage
			if first == "" {
				first = title
			}
			first = singleLine(first, 60)
			latest := singleLine(item.LatestUserMessage, 60)
			title = sessionMessagePreview(first, latest, 0)
			updated := displaySessionTime(item.UpdatedAt, now)
			if item.ClosedAt != "" {
				updated = displaySessionTime(item.ClosedAt, now)
			}
			if group.Heading == "Global" {
				text := fmt.Sprintf("%-10s  %-8s  %-20s  [%dt] %s", updated, item.Agent, displayWorkspace(item.Workspace, 20), item.TurnCount, title)
				rows = append(rows, contentRow{
					text: text, itemIndex: itemIndex, kind: contentRowSession,
					agent: item.Agent, updated: updated, workspace: displayWorkspace(item.Workspace, 20), title: title, firstUser: first, latestUser: latest, turnCount: item.TurnCount, userPrompts: item.UserPrompts, lastClosed: item.ClosedAt != "",
				})
			} else {
				text := fmt.Sprintf("%-10s  %-8s  [%dt] %s", updated, item.Agent, item.TurnCount, title)
				rows = append(rows, contentRow{
					text: text, itemIndex: itemIndex, kind: contentRowSession,
					agent: item.Agent, updated: updated, title: title, firstUser: first, latestUser: latest, turnCount: item.TurnCount, userPrompts: item.UserPrompts, lastClosed: item.ClosedAt != "",
				})
			}
			itemIndex++
		}
	}
	return rows
}

func sessionMessageParts(first, latest string, width int) (string, string) {
	first = singleLine(first, 60)
	latest = singleLine(latest, 60)
	if latest == "" || latest == first {
		if width > 0 {
			first = runewidth.Truncate(first, width, "…")
		}
		return "", first
	}
	if width <= 0 {
		return latest, first
	}
	if width < 5 {
		return runewidth.Truncate(latest+" ← "+first, width, "…"), ""
	}
	available := width - 3
	latestWidth := min(runewidth.StringWidth(latest), available/2)
	firstWidth := min(runewidth.StringWidth(first), available-latestWidth)
	latestWidth = min(runewidth.StringWidth(latest), available-firstWidth)
	return runewidth.Truncate(latest, latestWidth, "…"), runewidth.Truncate(first, firstWidth, "…")
}

func sessionMessagePreview(first, latest string, width int) string {
	latest, first = sessionMessageParts(first, latest, width)
	if latest == "" {
		return first
	}
	if first == "" {
		return latest
	}
	return latest + " ← " + first
}

func fitSessionRow(row contentRow, width int) contentRow {
	base := runewidth.StringWidth(styledContentRow(row, false, false)) - runewidth.StringWidth(row.title)
	if width > base {
		row.latestUser, row.firstUser = sessionMessageParts(row.firstUser, row.latestUser, width-base)
		row.title = sessionMessagePreview(row.firstUser, row.latestUser, 0)
	} else {
		row.text = runewidth.Truncate(styledContentRow(row, false, false), max(0, width), "…")
		row.kind = contentRowPlain
	}
	return row
}

func selectedRowRange(rows []contentRow, selected int) (int, int) {
	first, last := -1, 0
	for i, r := range rows {
		if r.itemIndex == selected {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, 0
	}
	return first, last
}

func countItems(rows []contentRow) int {
	seen := map[int]bool{}
	for _, r := range rows {
		if r.itemIndex >= 0 {
			seen[r.itemIndex] = true
		}
	}
	return len(seen)
}

func (a App) selectorSize() (int, int) {
	var width, height int
	for _, stream := range []any{a.Stdout, a.Stdin} {
		file, ok := stream.(*os.File)
		if !ok || !term.IsTerminal(int(file.Fd())) {
			continue
		}
		width, height = 80, 24
		if w, h, err := term.GetSize(int(file.Fd())); err == nil && w > 0 && h > 0 {
			width, height = w, h
			break
		}
	}
	if a.termWidth > 0 {
		width = a.termWidth
	}
	if a.termHeight > 0 {
		height = a.termHeight
	}
	return width, height
}

func fitSelectorLine(value string, width int) string {
	if width <= 0 {
		return value
	}
	var plain strings.Builder
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "\x1b[") {
			if end := strings.IndexByte(value[i:], 'm'); end >= 0 {
				i += end + 1
				continue
			}
		}
		plain.WriteByte(value[i])
		i++
	}
	text := plain.String()
	if runewidth.StringWidth(text) <= width {
		return value
	}
	prefix := strings.TrimSuffix(runewidth.Truncate(text, width, "…"), "…")
	var fitted strings.Builder
	for i, copied := 0, 0; i < len(value) && copied < len(prefix); {
		if strings.HasPrefix(value[i:], "\x1b[") {
			if end := strings.IndexByte(value[i:], 'm'); end >= 0 {
				fitted.WriteString(value[i : i+end+1])
				i += end + 1
				continue
			}
		}
		fitted.WriteByte(value[i])
		i++
		copied++
	}
	fitted.WriteString("…")
	if strings.Contains(value, "\x1b[") {
		fitted.WriteString(ansiReset)
	}
	return fitted.String()
}

func (a App) chooseOptions(reader *bufio.Reader, title string, options []string) (int, error) {
	rows := make([]contentRow, len(options))
	for index, option := range options {
		rows[index] = contentRow{text: option, itemIndex: index, agent: option}
	}
	return a.chooseInteractive(reader, title, rows, len(options), errors.New("no options available"))
}

func (a App) pick(reader *bufio.Reader, title string, options []string) (int, error) {
	rows := make([]contentRow, len(options))
	for index, option := range options {
		rows[index] = contentRow{text: option, itemIndex: index, agent: option}
	}
	index, _, err := a.runSelector(reader, selector{title: title, help: selectorHelp, marker: "> "}, rows, len(options), errors.New("no options available"))
	return index, err
}

var errSelectionCancelled = errors.New("interactive selection cancelled")

const selectorHelp = "Use ↑/↓ or j/k to move, Enter to select, q to cancel."

type selector struct {
	title    string
	help     string
	footer   bool
	marker   string
	manage   bool
	selected int
	search   bool
}

func (a App) chooseInteractive(reader *bufio.Reader, title string, rows []contentRow, itemCount int, emptyErr error) (int, error) {
	index, _, err := a.runSelector(reader, selector{title: title, help: selectorHelp, marker: "> "}, rows, itemCount, emptyErr)
	if errors.Is(err, errSelectionCancelled) {
		return 0, errors.New("interactive session resume cancelled")
	}
	return index, err
}

// runSelector reports the key that ended the loop: selectionConfirm, or selectionManage/selectionExport/selectionFresh when enabled.
func (a App) runSelector(reader *bufio.Reader, options selector, allRows []contentRow, itemCount int, emptyErr error) (int, selectionKey, error) {
	selected := options.selected
	sourceRows := allRows
	visibleItems := selectorItemIndices(allRows)
	query := ""
	searching, pending := false, false
	searchVersion := 0
	var cancelSearch context.CancelFunc
	defer func() {
		if cancelSearch != nil {
			cancelSearch()
		}
	}()
	type searchResult struct {
		version int
		rows    []contentRow
	}
	matches := make(chan searchResult, 1)
	viewStart := 0
	lineCount := 0
	termW, termH := a.selectorSize()

	render := func(clear bool) {
		width, height := a.selectorSize()
		resized := width != termW || height != termH
		termW, termH = width, height
		if clear {
			if resized {
				// Reflow can move the old origin outside the visible screen.
				fmt.Fprint(a.Stdout, "\x1b[H\x1b[2J")
			} else if lineCount > 0 {
				fmt.Fprintf(a.Stdout, "\x1b[%dA\r\x1b[J", lineCount)
			} else {
				fmt.Fprint(a.Stdout, "\r\x1b[J")
			}
		}
		lineCount = 0
		writeLine := func(value string) {
			if termH == 1 {
				fmt.Fprint(a.Stdout, fitSelectorLine(value, termW))
				return
			}
			fmt.Fprintf(a.Stdout, "%s\r\n", fitSelectorLine(value, termW))
			lineCount++
		}
		var header, footer []string
		if options.title != "" && (termH == 0 || termH > 2) {
			header = append(header, colorize(a.Color, ansiBold, options.title))
		}
		help := options.help
		if searching {
			if termH == 0 || termH > 2+len(header) {
				header = append(header, "Search prompts: "+query+"▏")
			}
			status := fmt.Sprintf("%d matches", len(visibleItems))
			if len(strings.Fields(query)) == 0 {
				status = "Type to search user prompts"
			}
			if pending {
				status = "Searching…"
			}
			help = status + " · ↑/↓ select · Enter open · Esc clear"
		}
		if termW > 0 {
			help = runewidth.Wrap(help, termW)
		}
		helpLines := strings.Split(help, "\n")
		if termH > 0 {
			// Leave one row for the cursor and one for the selected item.
			helpLines = helpLines[:min(len(helpLines), max(0, termH-2-len(header)))]
		}
		for _, line := range helpLines {
			if options.footer {
				footer = append(footer, colorize(a.Color, ansiDim, line))
			} else {
				header = append(header, colorize(a.Color, ansiDim, line))
			}
		}
		budget := len(allRows) + 2
		if termH > 0 {
			budget = max(1, termH-1-len(header)-len(footer))
		}
		if options.footer && (termH == 0 || budget > 1) {
			footer = append([]string{""}, footer...)
			budget--
		}
		for _, line := range header {
			writeLine(line)
		}

		visStart, visEnd := 0, len(allRows)
		showIndicators := budget >= 3
		if len(allRows) > budget {
			visible := budget
			if showIndicators {
				visible -= 2
			}
			selFirst, selLast := selectedRowRange(allRows, selected)
			if selFirst < viewStart {
				viewStart = selFirst
			}
			if selLast >= viewStart+visible {
				viewStart = selLast - visible + 1
			}
			viewStart = max(0, min(viewStart, len(allRows)-visible))
			visStart = viewStart
			visEnd = min(len(allRows), viewStart+visible)
		}
		if hidden := countItems(allRows[:visStart]); showIndicators && hidden > 0 {
			writeLine(colorize(a.Color, ansiDim, fmt.Sprintf("  ↑ %d more", hidden)))
		}
		for index, row := range allRows[visStart:visEnd] {
			marker := ""
			if row.itemIndex >= 0 {
				marker = "  "
				continuation := visStart+index > 0 && allRows[visStart+index-1].itemIndex == row.itemIndex
				if row.itemIndex == selected && !continuation {
					marker = options.marker
				}
				if row.kind == contentRowSession && termW > 0 {
					row = fitSessionRow(row, termW-runewidth.StringWidth(marker))
				}
			}
			writeLine(styledMarker(marker, row, a.Color) + styledContentRow(row, row.itemIndex == selected, a.Color))
		}
		if hidden := countItems(allRows[visEnd:]); showIndicators && hidden > 0 {
			writeLine(colorize(a.Color, ansiDim, fmt.Sprintf("  ↓ %d more", hidden)))
		}
		for _, line := range footer {
			writeLine(line)
		}
	}
	applyRows := func(rows []contentRow) {
		allRows = rows
		visibleItems = selectorItemIndices(rows)
		selected = -1
		if len(visibleItems) > 0 {
			selected = visibleItems[0]
		}
		viewStart = 0
	}
	updateSearch := func() {
		searchVersion++
		if cancelSearch != nil {
			cancelSearch()
		}
		if len(strings.Fields(query)) == 0 {
			pending = false
			applyRows(sourceRows)
			render(true)
			return
		}
		pending = true
		ctx, cancel := context.WithCancel(context.Background())
		cancelSearch = cancel
		value := query
		version := searchVersion
		go func() {
			rows := filterSessionPrompts(ctx, sourceRows, value)
			select {
			case matches <- searchResult{version: version, rows: rows}:
			case <-ctx.Done():
			}
		}()
		render(true)
	}

	render(false)
	if itemCount == 0 {
		return 0, selectionConfirm, emptyErr
	}

	restore, err := makeRaw(a.Stdin)
	if err != nil {
		return 0, selectionConfirm, err
	}
	defer restore()

	type keyResult struct {
		key   selectionKey
		value rune
		err   error
	}
	requests := make(chan struct{})
	results := make(chan keyResult)
	defer close(requests)
	go func() {
		for range requests {
			key, value, err := readSelectionInput(reader)
			results <- keyResult{key: key, value: value, err: err}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		requests <- struct{}{}
		var result keyResult
	waitForKey:
		for {
			select {
			case result = <-results:
				break waitForKey
			case match := <-matches:
				if searching && pending && match.version == searchVersion {
					pending = false
					applyRows(match.rows)
					render(true)
				}
			case <-ticker.C:
				width, height := a.selectorSize()
				if width != termW || height != termH {
					render(true)
				}
			}
		}
		if result.err != nil {
			return 0, selectionConfirm, result.err
		}
		key := result.key
		if searching {
			switch {
			case result.value == 27 && key == selectionCancel:
				query, searching = "", false
				updateSearch()
				continue
			case result.value == 8 || result.value == 127:
				runes := []rune(query)
				if len(runes) > 0 {
					query = string(runes[:len(runes)-1])
					updateSearch()
				}
				continue
			case unicode.IsPrint(result.value):
				query += string(result.value)
				updateSearch()
				continue
			}
		} else if options.search && result.value == '/' {
			searching = true
			render(true)
			continue
		}
		if key == selectionUp || key == selectionDown || key == selectionConfirm {
			for pending {
				match := <-matches
				if match.version == searchVersion {
					pending = false
					applyRows(match.rows)
					render(true)
				}
			}
		}
		position := -1
		for i, index := range visibleItems {
			if index == selected {
				position = i
				break
			}
		}
		switch key {
		case selectionUp:
			if position > 0 {
				selected = visibleItems[position-1]
				render(true)
			}
		case selectionDown:
			if position >= 0 && position < len(visibleItems)-1 {
				selected = visibleItems[position+1]
				render(true)
			}
		case selectionConfirm:
			if selected < 0 {
				continue
			}
			fmt.Fprint(a.Stdout, "\r\n")
			return selected, selectionConfirm, nil
		case selectionManage:
			if options.manage {
				fmt.Fprint(a.Stdout, "\r\n")
				return selected, selectionManage, nil
			}
		case selectionExport:
			if options.manage {
				fmt.Fprint(a.Stdout, "\r\n")
				return selected, selectionExport, nil
			}
		case selectionFresh:
			if options.manage {
				fmt.Fprint(a.Stdout, "\r\n")
				return selected, selectionFresh, nil
			}
		case selectionCancel:
			fmt.Fprint(a.Stdout, "\r\n")
			return 0, selectionConfirm, errSelectionCancelled
		}
	}
}

func styledMarker(marker string, row contentRow, color bool) string {
	if marker == "" || marker == "  " {
		return marker
	}
	if row.kind == contentRowPreset || row.kind == contentRowCreate || row.kind == contentRowProblem {
		return colorize(color, presetAccent+ansiBold, marker)
	}
	style := agentColor(row.agent)
	if style == "" {
		style = ansiCyan
	}
	return colorize(color, style+ansiBold, marker)
}

func styledContentRow(row contentRow, selected, color bool) string {
	switch row.kind {
	case contentRowHeading:
		return colorize(color, ansiBold, row.text)
	case contentRowEmpty:
		return colorize(color, ansiDim, row.text)
	case contentRowPreset:
		name := row.title
		if selected {
			name = colorize(color, presetAccent+ansiBold, name)
		}
		combo := colorize(color, agentColor(row.agent), presets.AgentName(row.agent)) + colorize(color, ansiDim, row.updated)
		return name + "  " + combo + "  " + row.text
	case contentRowCreate:
		style := presetAccent
		if selected {
			style += ansiBold
		}
		return colorize(color, style, row.text)
	case contentRowProblem:
		name := row.title
		if selected {
			name = colorize(color, ansiBold, name)
		}
		if name == "" {
			return colorize(color, ansiRed, row.text)
		}
		return name + "  " + colorize(color, ansiRed, row.text)
	case contentRowSession:
		agent := colorize(color, agentColor(row.agent)+ansiDim, fmt.Sprintf("%-8s", row.agent))
		updated := colorize(color, ansiDim, fmt.Sprintf("%-10s", row.updated))
		titleStyle := ""
		if selected {
			titleStyle = ansiBold
		}
		title := colorize(color, titleStyle, row.title)
		if row.latestUser != "" && row.firstUser != row.latestUser {
			title = colorize(color, ansiDim, row.latestUser)
			if row.firstUser != "" {
				title += " ← " + colorize(color, titleStyle, row.firstUser)
			}
		}
		turns := fmt.Sprintf("[%dt] ", row.turnCount)
		if row.turnCount < 0 {
			turns = "[?t] "
		}
		title = colorize(color, ansiDim, turns) + title
		if row.lastClosed {
			title += " " + colorize(color, ansiGreen, "[last closed]")
		}
		if row.workspace == "" {
			return fmt.Sprintf("%s  %s  %s", updated, agent, title)
		}
		workspace := colorize(color, ansiDim, fmt.Sprintf("%-20s", row.workspace))
		return fmt.Sprintf("%s  %s  %s  %s", updated, agent, workspace, title)
	default:
		style := agentColor(row.agent)
		if selected {
			style += ansiBold
		}
		return colorize(color && style != "", style, row.text)
	}
}

type selectionKey int

const (
	selectionIgnore selectionKey = iota
	selectionUp
	selectionDown
	selectionConfirm
	selectionCancel
	selectionManage
	selectionExport
	selectionFresh
)

func readSelectionKey(reader *bufio.Reader) (selectionKey, error) {
	key, _, err := readSelectionInput(reader)
	return key, err
}

func readSelectionInput(reader *bufio.Reader) (selectionKey, rune, error) {
	value, _, err := reader.ReadRune()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return selectionIgnore, 0, errors.New("interactive input closed")
		}
		return selectionIgnore, 0, fmt.Errorf("read interactive input: %w", err)
	}
	switch value {
	case '\r', '\n':
		return selectionConfirm, value, nil
	case 'k', 'K':
		return selectionUp, value, nil
	case 'j', 'J':
		return selectionDown, value, nil
	case 'q', 'Q', 3:
		return selectionCancel, value, nil
	case 'm', 'M':
		return selectionManage, value, nil
	case 'e', 'E':
		return selectionExport, value, nil
	case 't', 'T':
		return selectionFresh, value, nil
	case 27:
		if reader.Buffered() < 2 {
			return selectionCancel, value, nil
		}
		prefix, _ := reader.Peek(1)
		if prefix[0] != '[' {
			return selectionCancel, value, nil
		}
		first, err := reader.ReadByte()
		if err != nil || first != '[' {
			return selectionCancel, value, nil
		}
		second, err := reader.ReadByte()
		if err != nil {
			return selectionCancel, value, nil
		}
		if second == 'A' {
			return selectionUp, value, nil
		}
		if second == 'B' {
			return selectionDown, value, nil
		}
	}
	return selectionIgnore, value, nil
}

func makeRaw(input io.Reader) (func(), error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return func() {}, nil
	}
	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enable interactive terminal input: %w", err)
	}
	return func() { _ = term.Restore(int(file.Fd()), state) }, nil
}

func displaySessionTime(value string, now time.Time) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return singleLine(value, 10)
	}
	parsed = parsed.In(now.Location())
	elapsed := now.Sub(parsed)
	if elapsed < time.Minute {
		return "Just now"
	}
	if elapsed < time.Hour {
		return fmt.Sprintf("%dm ago", int(elapsed/time.Minute))
	}
	if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(elapsed/time.Hour))
	}
	if sameCalendarDay(parsed, now.AddDate(0, 0, -1)) {
		return "Yesterday"
	}
	if elapsed < 30*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(elapsed/(24*time.Hour)))
	}
	return parsed.Format("2006-01-02")
}

func displaySessionDuration(startedAt, updatedAt string) string {
	started, startErr := time.Parse(time.RFC3339Nano, startedAt)
	updated, updateErr := time.Parse(time.RFC3339Nano, updatedAt)
	if startErr != nil || updateErr != nil || updated.Before(started) {
		return ""
	}
	duration := updated.Sub(started)
	if duration < time.Minute {
		return fmt.Sprintf("(%ds)", int(duration/time.Second))
	}
	if duration < time.Hour {
		return fmt.Sprintf("(%dm)", int(duration/time.Minute))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("(%dh %02dm)", int(duration/time.Hour), int(duration/time.Minute)%60)
	}
	days := int(duration / (24 * time.Hour))
	if days > 999 {
		return "(999d+)"
	}
	return fmt.Sprintf("(%dd %02dh)", days, int(duration/time.Hour)%24)
}

func sameCalendarDay(left, right time.Time) bool {
	leftYear, leftMonth, leftDay := left.Date()
	rightYear, rightMonth, rightDay := right.Date()
	return leftYear == rightYear && leftMonth == rightMonth && leftDay == rightDay
}

func displayWorkspace(value string, max int) string {
	if value == "" {
		return "(workspace unknown)"
	}
	value = filepath.Clean(value)
	if home, err := os.UserHomeDir(); err == nil {
		if value == home {
			value = "~"
		} else if relative, err := filepath.Rel(home, value); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			value = filepath.Join("~", relative)
		}
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return "…" + string(runes[len(runes)-max+1:])
}
