package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArcheMind/sessionman/internal/sessions"
)

const (
	offloadHistoryPrefix = "Offloaded conversation history: "
	offloadHistorySuffix = " — read it when earlier context is needed."
)

func (a App) offloadSessionHistory(history sessions.Session) (sessions.Message, error) {
	var data bytes.Buffer
	history.Messages = sessions.RechainMessages(history.Messages)
	history.MessageCount = len(history.Messages)
	if err := writeStructured(&data, history, OutputJSON); err != nil {
		return sessions.Message{}, fmt.Errorf("encode offload history: %w", err)
	}
	dir := filepath.Join(a.Sessions.HomeDir, ".local", "state", "sessionman", "history")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return sessions.Message{}, fmt.Errorf("create offload directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "offload-*.json")
	if err != nil {
		return sessions.Message{}, fmt.Errorf("create offload file: %w", err)
	}
	_, err = file.Write(data.Bytes())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		return sessions.Message{}, fmt.Errorf("write offload file %s: %w", file.Name(), err)
	}
	return freshHistoryMessage(file.Name(), bytes.Count(data.Bytes(), []byte("\n")), len(sessionTurns(history.Messages))), nil
}

// Expand before selection so history pointers never become selectable turns.
func loadSessionHistory(detail sessions.Session) (sessions.Session, bool, error) {
	messages, restored, err := expandSessionHistory(detail.Messages, make(map[string]bool))
	if err != nil {
		return sessions.Session{}, false, err
	}
	if restored {
		detail.Messages = sessions.SanitizeMessageIDs(sessions.SanitizeToolIDs(sessions.RechainMessages(fillTimestamps(messages))))
		detail.MessageCount = len(detail.Messages)
	}
	return detail, restored, nil
}

func expandSessionHistory(messages []sessions.Message, active map[string]bool) ([]sessions.Message, bool, error) {
	var out []sessions.Message
	restored := false
	for _, message := range messages {
		path := offloadHistoryPath(message)
		if path == "" {
			out = append(out, message)
			continue
		}
		path = filepath.Clean(path)
		if active[path] {
			return nil, false, fmt.Errorf("cyclic offload history reference: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("read offload history %s: %w", path, err)
		}
		var history sessions.Session
		if err := json.Unmarshal(data, &history); err != nil {
			return nil, false, fmt.Errorf("parse offload history %s: %w", path, err)
		}
		if len(history.Messages) == 0 || len(sessionTurns(history.Messages)) == 0 {
			return nil, false, fmt.Errorf("offload history %s contains no turns", path)
		}
		active[path] = true
		expanded, _, err := expandSessionHistory(history.Messages, active)
		delete(active, path)
		if err != nil {
			return nil, false, err
		}
		out = append(out, expanded...)
		restored = true
	}
	return out, restored, nil
}

func offloadHistoryPath(message sessions.Message) string {
	if message.Role != "user" || len(message.Content) != 1 || message.Content[0].Type != "text" {
		return ""
	}
	text := message.Content[0].Text
	if !strings.HasPrefix(text, offloadHistoryPrefix) || !strings.HasSuffix(text, offloadHistorySuffix) {
		return ""
	}
	path := strings.TrimSuffix(strings.TrimPrefix(text, offloadHistoryPrefix), offloadHistorySuffix)
	if index := strings.LastIndex(path, " — "); index >= 0 {
		var lines, turns int
		if _, err := fmt.Sscanf(path[index:], " — %d lines, %d turns", &lines, &turns); err == nil {
			path = path[:index]
		}
	}
	return path
}
