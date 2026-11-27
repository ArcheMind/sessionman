package app

import (
	"bufio"
	"crypto/rand"
	"fmt"

	"github.com/ArcheMind/sessionman/internal/sessions"
)

const (
	freshTurnThreshold = 7
	freshHeadUsers     = 2
	freshTailUsers     = 3
)

func exceedsFreshThreshold(messages []sessions.Message) (int, bool) {
	turns := 0
	for _, message := range messages {
		if message.Role == "user" {
			turns++
		}
	}
	return turns, turns > freshTurnThreshold
}

func freshHistoryMessage(historyPath string, lines, turns int) sessions.Message {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return sessions.Message{
		ID:   fmt.Sprintf("fresh-%x", buf),
		Role: "user",
		Content: []sessions.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("%s%s — %d lines, %d turns%s", offloadHistoryPrefix, historyPath, lines, turns, offloadHistorySuffix),
		}},
	}
}

func (a App) freshSessionDetail(detail sessions.Session) (sessions.Session, error) {
	turns := sessionTurns(detail.Messages)
	states := make([]turnDisposition, len(turns))
	for i := range states {
		if i >= freshHeadUsers && i < len(turns)-freshTailUsers {
			states[i] = turnOffload
		}
	}
	return a.applySessionTurns(detail, states)
}

func (a App) confirmLargeContext(reader *bufio.Reader, turns int) (selectionKey, error) {
	fmt.Fprintf(a.Stdout, "Load session · %d turns\r\n\r\n[a] Auto offload  [k] Keep all  [m] Manual  [Esc] Cancel", turns)
	restore, err := makeRaw(a.Stdin)
	if err != nil {
		return selectionCancel, err
	}
	defer restore()
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return selectionCancel, err
		}
		var key selectionKey
		switch value {
		case 'a', 'A':
			key = selectionFresh
		case 'k', 'K':
			key = selectionConfirm
		case 'm', 'M':
			key = selectionManage
		case 3, 27:
			key = selectionCancel
		default:
			continue
		}
		fmt.Fprint(a.Stdout, "\r\n")
		return key, nil
	}
}
