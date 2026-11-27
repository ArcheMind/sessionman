package app

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/presets"
	"github.com/ArcheMind/sessionman/internal/sessions"
	"github.com/mattn/go-runewidth"
)

var terminalControl = regexp.MustCompile(`^\x1b\[(\d*)([A-Za-z])`)

// renderScreen applies the cursor controls the selector emits, so redraw drift shows up as leftover lines.
func renderScreen(output string) []string {
	var lines [][]rune
	row, col := 0, 0
	ensure := func() {
		for len(lines) <= row {
			lines = append(lines, nil)
		}
	}
	for index := 0; index < len(output); {
		if match := terminalControl.FindStringSubmatch(output[index:]); match != nil {
			count, _ := strconv.Atoi(match[1])
			switch match[2] {
			case "A":
				row = max(0, row-max(1, count))
			case "J":
				ensure()
				lines[row] = lines[row][:min(col, len(lines[row]))]
				lines = lines[:row+1]
			case "K":
				ensure()
				lines[row] = nil
			}
			index += len(match[0])
			continue
		}
		r, size := []rune(output[index:])[0], len(string([]rune(output[index:])[0]))
		index += size
		switch r {
		case '\r':
			col = 0
		case '\n':
			row++
		default:
			ensure()
			for len(lines[row]) < col {
				lines[row] = append(lines[row], ' ')
			}
			if col < len(lines[row]) {
				lines[row][col] = r
			} else {
				lines[row] = append(lines[row], r)
			}
			col++
		}
	}
	result := make([]string, len(lines))
	for index, line := range lines {
		result[index] = string(line)
	}
	return result
}

func TestHomeRedrawsInPlace(t *testing.T) {
	application, stdout, _ := presetApp(t, "jjjkkjq")
	mustCreate(t, application.Hooks.Home, deepReview)
	mustCreate(t, application.Hooks.Home, presets.Preset{Name: "daily", Description: "Everyday development", Agent: "claude", Model: "opus"})
	groups := []sessionGroup{{Heading: "Current workspace", Items: []sessions.Summary{{ID: "s1", Agent: "codex", Title: "Fix login state"}}}, {Heading: "Global"}}
	application.chooseHome(bufio.NewReader(application.Stdin), application.Hooks.Home, groups, 0)
	screen := strings.Join(renderScreen(stdout.String()), "\n")
	for _, heading := range []string{"New session · Presets", "Recent sessions · Current workspace", homeHelp} {
		if count := strings.Count(screen, heading); count != 1 {
			t.Fatalf("%q appears %d times on screen:\n%s", heading, count, screen)
		}
	}
}

func TestSelectorFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {40, 12}, {16, 6}, {4, 3}, {1, 1}} {
		for _, footer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d/footer=%t", size[0], size[1], footer), func(t *testing.T) {
				application, stdout, _ := presetApp(t, "q")
				application.termWidth, application.termHeight = size[0], size[1]
				var rows []contentRow
				for i := 0; i < 30; i++ {
					rows = append(rows, contentRow{text: strings.Repeat("中文 preset ", 20), itemIndex: i})
				}
				_, _, err := application.runSelector(bufio.NewReader(application.Stdin), selector{
					title: "A long selector title", help: homeHelp, marker: "> ", footer: footer, selected: 15,
				}, rows, len(rows), nil)
				if err != errSelectionCancelled {
					t.Fatalf("error = %v", err)
				}
				frame := strings.TrimSuffix(stdout.String(), "\r\n")
				if frame == "" {
					t.Fatal("selection is blank")
				}
				lines := strings.Split(frame, "\r\n")
				if len(lines) > size[1] {
					t.Fatalf("rendered %d rows in height %d: %q", len(lines), size[1], frame)
				}
				for _, line := range lines {
					if width := runewidth.StringWidth(line); width > size[0] {
						t.Fatalf("line width %d exceeds %d: %q", width, size[0], line)
					}
				}
				if size[0] >= 4 && size[1] >= 3 && !strings.Contains(frame, "> ") {
					t.Fatalf("selection not visible: %q", frame)
				}
			})
		}
	}
}

func TestFitSelectorLinePreservesStyleAndCellWidth(t *testing.T) {
	value := ansiBold + "中文" + ansiReset + "abcdef"
	got := fitSelectorLine(value, 5)
	want := ansiBold + "中文…" + ansiReset
	if got != want {
		t.Fatalf("fitted = %q, want %q", got, want)
	}
}

func TestSelectorDoesNotConsumeNextPrompt(t *testing.T) {
	application, _, _ := presetApp(t, "\rnext prompt\n")
	reader := bufio.NewReader(application.Stdin)
	if _, err := application.pick(reader, "Pick", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := reader.ReadString('\n'); got != "next prompt\n" {
		t.Fatalf("remaining input = %q", got)
	}
}
