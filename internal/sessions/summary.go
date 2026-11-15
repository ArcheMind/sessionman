package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func discoverJSONL(agent string, patterns ...string) ([]Summary, error) {
	var items []Summary
	var failures []error
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, path := range paths {
			item, ok, err := readJSONLMetadata(agent, path)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if ok {
				items = append(items, item)
			}
		}
	}
	return items, errors.Join(failures...)
}

func readJSONLMetadata(agent, path string) (Summary, bool, error) {
	item := Summary{Agent: agent, Source: path, UpdatedAt: sessionFileTimestamp(path)}
	decode := jsonlRecordDecoder(agent)
	err := scanJSONL(path, func(record map[string]any) bool {
		switch agent {
		case "claude":
			if sidechain, _ := record["isSidechain"].(bool); sidechain {
				item.IsSubagent = true
				if agentID := stringValue(record["agentId"]); agentID != "" {
					item.ID = agentID
				}
			}
			if item.StartedAt == "" {
				item.StartedAt = timeValue(record["timestamp"])
			}
			if item.ID == "" {
				item.ID = stringValue(record["sessionId"])
			}
			if item.Workspace == "" {
				item.Workspace = stringValue(record["cwd"])
			}
		case "codex":
			if stringValue(record["type"]) == "session_meta" && item.ID == "" {
				payload, _ := record["payload"].(map[string]any)
				item.ID = firstNonEmpty(stringValue(payload["id"]), stringValue(payload["session_id"]))
				item.Workspace = stringValue(payload["cwd"])
				item.StartedAt = timeValue(payload["timestamp"])
				item.IsSubagent = codexSubagentSource(payload["source"])
			}
		case "pi":
			if stringValue(record["type"]) == "session" {
				item.ID = stringValue(record["id"])
				item.Workspace = stringValue(record["cwd"])
				item.StartedAt = timeValue(record["timestamp"])
			}
		}
		if jsonlUserRecord(agent, record) {
			for _, message := range decode(record).Messages {
				if message.Role == "user" {
					text := TextContent(message.Content)
					item.addUserMessage(text)
					if item.Title == "" && isSessionTitle(text) {
						item.Title = oneLine(text, 96)
					}
				}
			}
		}
		return item.ID != "" && item.Workspace != "" && item.Title != ""
	})
	return item, item.ID != "", err
}

func summarizeJSONL(item Summary, includeCount bool) (Summary, error) {
	decode := jsonlRecordDecoder(item.Agent)
	if includeCount {
		item.MessageCount = 0
		item.FirstUserMessage, item.LatestUserMessage = "", ""
		lastRole := ""
		err := scanJSONL(item.Source, func(record map[string]any) bool {
			detail := decode(record)
			count := len(detail.Messages)
			if item.Agent == "codex" && count > 0 && lastRole == "assistant" {
				payload, _ := record["payload"].(map[string]any)
				switch stringValue(payload["type"]) {
				case "reasoning", "function_call", "custom_tool_call":
					count--
				}
			}
			item.MessageCount += count
			for _, message := range detail.Messages {
				if message.Role == "user" {
					item.addUserMessage(TextContent(message.Content))
				}
				lastRole = message.Role
			}
			return false
		})
		return item, err
	}
	item.LatestUserMessage = ""
	err := scanJSONLReverse(item.Source, func(line []byte) bool {
		user := false
		switch item.Agent {
		case "claude":
			var envelope struct{ Type string }
			user = json.Unmarshal(line, &envelope) == nil && envelope.Type == "user"
		case "codex":
			var envelope struct {
				Type    string
				Payload struct{ Type, Role string }
			}
			user = json.Unmarshal(line, &envelope) == nil && envelope.Type == "response_item" && envelope.Payload.Type == "message" && envelope.Payload.Role == "user"
		case "pi":
			var envelope struct {
				Type    string
				Message struct{ Role string }
			}
			user = json.Unmarshal(line, &envelope) == nil && envelope.Type == "message" && envelope.Message.Role == "user"
		}
		if !user {
			return false
		}
		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			return false
		}
		for _, message := range decode(record).Messages {
			text := TextContent(message.Content)
			if message.Role == "user" && isSessionTitle(text) {
				item.LatestUserMessage = oneLine(text, 60)
				return true
			}
		}
		return false
	})
	return item, err
}

func jsonlUserRecord(agent string, record map[string]any) bool {
	kind := stringValue(record["type"])
	switch agent {
	case "claude":
		return kind == "user"
	case "codex":
		payload, _ := record["payload"].(map[string]any)
		return kind == "response_item" && stringValue(payload["type"]) == "message" && stringValue(payload["role"]) == "user"
	case "pi":
		message, _ := record["message"].(map[string]any)
		return kind == "message" && stringValue(message["role"]) == "user"
	default:
		return false
	}
}

func jsonlRecordDecoder(agent string) func(map[string]any) Session {
	var meta map[string]any
	var parse func(string, []map[string]any) (Session, bool)
	switch agent {
	case "claude":
		meta = map[string]any{"sessionId": "summary"}
		parse = parseClaude
	case "codex":
		meta = map[string]any{"type": "session_meta", "payload": map[string]any{"id": "summary"}}
		parse = parseCodex
	case "pi":
		meta = map[string]any{"type": "session", "id": "summary"}
		parse = parsePi
	}
	return func(record map[string]any) Session {
		detail, _ := parse("summary", []map[string]any{meta, record})
		return detail
	}
}

func sessionFileTimestamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	timestamp := info.ModTime().UTC().Format(time.RFC3339Nano)
	_ = scanJSONLReverse(path, func(line []byte) bool {
		var record struct{ Timestamp any }
		if json.Unmarshal(line, &record) != nil {
			return false
		}
		value := record.Timestamp
		if value == nil {
			var nested struct{ Message struct{ Timestamp any } }
			if json.Unmarshal(line, &nested) == nil {
				value = nested.Message.Timestamp
			}
		}
		if parsed := timeValue(value); parsed != "" {
			timestamp = parsed
			return true
		}
		return false
	})
	return timestamp
}
