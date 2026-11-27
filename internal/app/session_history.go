package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ArcheMind/sessionman/internal/sessions"
	"gopkg.in/yaml.v3"
)

const (
	offloadHistoryPrefix = "Offloaded conversation history: "
	offloadHistorySuffix = " — read it when earlier context is needed."
)

func (a App) offloadSessionHistory(history sessions.Session) (sessions.Message, error) {
	var data bytes.Buffer
	history.Messages = sessions.RechainMessages(history.Messages)
	history.MessageCount = len(history.Messages)
	if err := writeStructured(&data, history, OutputYAML); err != nil {
		return sessions.Message{}, fmt.Errorf("encode offload history: %w", err)
	}
	dir := filepath.Join(a.Sessions.HomeDir, ".local", "state", "sessionman", "history")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return sessions.Message{}, fmt.Errorf("create offload directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "offload-*.yaml")
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
		if err := unmarshalOffloadHistory(data, &history); err != nil {
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

func unmarshalOffloadHistory(data []byte, out *sessions.Session) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return json.Unmarshal(trimmed, out)
	}
	if err := yaml.Unmarshal(trimmed, out); err != nil {
		if repaired := repairYAMLBlockScalarTabs(trimmed); !bytes.Equal(repaired, trimmed) {
			return yaml.Unmarshal(repaired, out)
		}
		return err
	}
	return nil
}

// repairYAMLBlockScalarTabs works around a gopkg.in/yaml.v3 encoder bug where
// multiline strings starting with '\t' are emitted as "|" / "|-" / "|+" without
// an explicit indentation indicator (e.g. "|2-"), causing yaml.v3's parser to
// reject the leading tab on re-read.
func repairYAMLBlockScalarTabs(data []byte) []byte {
	if !bytes.Contains(data, []byte{'\t'}) {
		return data
	}
	var lines [][]byte
	current := data
	for {
		var probe yaml.Node
		err := yaml.Unmarshal(current, &probe)
		if err == nil {
			return current
		}
		lineNum, ok := blockScalarTabErrorLine(err)
		if !ok {
			return data
		}
		if lines == nil {
			lines = bytes.Split(data, []byte{'\n'})
		}
		idx := lineNum - 1
		if idx < 0 || idx >= len(lines) {
			return data
		}
		updated, changed := addBlockScalarIndentIndicator(lines[idx], '2')
		if !changed {
			return data
		}
		lines[idx] = updated
		current = bytes.Join(lines, []byte{'\n'})
	}
}

func blockScalarTabErrorLine(err error) (int, bool) {
	const prefix = "yaml: line "
	const suffix = ": found a tab character where an indentation space is expected"
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, suffix) {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(msg, prefix), suffix)
	line, convErr := strconv.Atoi(rest)
	if convErr != nil || line <= 0 {
		return 0, false
	}
	return line, true
}

func addBlockScalarIndentIndicator(line []byte, indent byte) ([]byte, bool) {
	trimmed := bytes.TrimSuffix(line, []byte{'\r'})
	cr := len(trimmed) < len(line)
	n := len(trimmed)
	if n == 0 {
		return line, false
	}
	chomp := byte(0)
	if trimmed[n-1] == '-' || trimmed[n-1] == '+' {
		chomp = trimmed[n-1]
		n--
	}
	if n == 0 || (trimmed[n-1] != '|' && trimmed[n-1] != '>') {
		return line, false
	}
	out := make([]byte, 0, len(line)+1)
	out = append(out, trimmed[:n]...)
	out = append(out, indent)
	if chomp != 0 {
		out = append(out, chomp)
	}
	if cr {
		out = append(out, '\r')
	}
	return out, true
}
