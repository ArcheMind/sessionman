package app

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/ArcheMind/agentx/internal/sessions"
)

type turnDisposition int

const (
	turnKeep turnDisposition = iota
	turnOffload
	turnDelete
)

type sessionTurn struct {
	start int
	end   int
}

func sessionTurns(messages []sessions.Message) []sessionTurn {
	var turns []sessionTurn
	for i, message := range messages {
		if message.Role == "user" {
			if len(turns) > 0 {
				turns[len(turns)-1].end = i
			}
			turns = append(turns, sessionTurn{start: i, end: len(messages)})
		}
	}
	return turns
}

func (a App) applySessionTurns(detail sessions.Session, states []turnDisposition) (sessions.Session, error) {
	turns := sessionTurns(detail.Messages)
	if len(states) != len(turns) {
		return sessions.Session{}, fmt.Errorf("turn selection count does not match session")
	}
	for _, state := range states {
		if state < turnKeep || state > turnDelete {
			return sessions.Session{}, fmt.Errorf("invalid turn selection")
		}
	}
	prefixEnd := len(detail.Messages)
	if len(turns) > 0 {
		prefixEnd = turns[0].start
	}
	messages := append([]sessions.Message(nil), detail.Messages[:prefixEnd]...)
	for i := 0; i < len(turns); {
		turn := turns[i]
		switch states[i] {
		case turnKeep:
			messages = append(messages, detail.Messages[turn.start:turn.end]...)
		case turnOffload:
			end := i + 1
			for end < len(turns) && states[end] == turnOffload {
				end++
			}
			history := detail
			history.Messages = detail.Messages[turn.start:turns[end-1].end]
			pointer, err := a.offloadSessionHistory(history)
			if err != nil {
				return sessions.Session{}, err
			}
			messages = append(messages, pointer)
			i = end
			continue
		case turnDelete:
		}
		i++
	}

	detail.Messages = sessions.SanitizeMessageIDs(sessions.SanitizeToolIDs(sessions.RechainMessages(fillTimestamps(messages))))
	detail.MessageCount = len(detail.Messages)
	return detail, nil
}

func (a App) chooseSessionTurns(reader *bufio.Reader, detail sessions.Session) ([]turnDisposition, error) {
	turns := sessionTurns(detail.Messages)
	if len(turns) == 0 {
		return nil, fmt.Errorf("session has no user prompts")
	}
	states := make([]turnDisposition, len(turns))
	selected, anchor, count, viewStart, lines := 0, -1, 0, 0, 0
	restore, err := makeRaw(a.Stdin)
	if err != nil {
		return nil, err
	}
	defer restore()
	labels := []string{"Keep", "Offload", "Delete"}
	for {
		width, height := a.selectorSize()
		if lines > 0 {
			fmt.Fprintf(a.Stdout, "\x1b[%dA\r\x1b[J", lines)
		}
		lines = 0
		writeLine := func(text string) {
			fmt.Fprintf(a.Stdout, "%s\r\n", fitSelectorLine(text, width))
			lines++
		}
		writeLine(fmt.Sprintf("Manual session load · %d turns", len(turns)))
		budget := len(turns)
		if height > 0 {
			budget = max(1, height-5)
		}
		if selected < viewStart {
			viewStart = selected
		}
		if selected >= viewStart+budget {
			viewStart = selected - budget + 1
		}
		lo, hi := selected, selected
		if anchor >= 0 {
			lo, hi = min(anchor, selected), max(anchor, selected)
		}
		for i := viewStart; i < min(len(turns), viewStart+budget); i++ {
			marker, selection := " ", " "
			if i == selected {
				marker = ">"
			}
			if anchor >= 0 && i >= lo && i <= hi {
				selection = "*"
			}
			prompt := strings.Join(strings.Fields(sessions.MessageText(detail.Messages[turns[i].start])), " ")
			if prompt == "" {
				prompt = "[No text]"
			}
			writeLine(fmt.Sprintf("%s%s [%-7s] %04d %s", marker, selection, labels[states[i]], i+1, prompt))
		}
		writeLine("j/k ↑↓ Move · V Select lines · g/G First/Last · number+j/k Move many")
		writeLine("r Keep · o Offload · d Delete · Enter Load · Esc Clear selection / Cancel")
		key, value, err := readSelectionInput(reader)
		if err != nil {
			return nil, err
		}
		if value >= '0' && value <= '9' {
			count = min(1000000, count*10+int(value-'0'))
			continue
		}
		distance := max(1, count)
		count = 0
		switch {
		case key == selectionUp:
			selected = max(0, selected-distance)
		case key == selectionDown:
			selected = min(len(turns)-1, selected+distance)
		case value == 'V':
			if anchor < 0 {
				anchor = selected
			} else {
				anchor = -1
			}
		case value == 'g':
			selected = 0
		case value == 'G':
			selected = len(turns) - 1
		case value == 'r' || value == 'o' || value == 'd':
			state := turnKeep
			if value == 'o' {
				state = turnOffload
			} else if value == 'd' {
				state = turnDelete
			}
			for i := lo; i <= hi; i++ {
				states[i] = state
			}
			anchor = -1
		case key == selectionConfirm:
			return states, nil
		case key == selectionCancel:
			if value == 27 && anchor >= 0 {
				anchor = -1
			} else {
				return nil, errSelectionCancelled
			}
		}
	}
}
