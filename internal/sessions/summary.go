package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
		results := make([]struct {
			item Summary
			ok   bool
			err  error
		}, len(paths))
		jobs := make(chan int)
		var workers sync.WaitGroup
		for range min(4, len(paths)) {
			workers.Go(func() {
				for i := range jobs {
					results[i].item, results[i].ok, results[i].err = readJSONLMetadata(agent, paths[i])
				}
			})
		}
		for i := range paths {
			jobs <- i
		}
		close(jobs)
		workers.Wait()
		for _, result := range results {
			if result.err != nil {
				failures = append(failures, result.err)
				continue
			}
			if result.ok {
				items = append(items, result.item)
			}
		}
	}
	return items, errors.Join(failures...)
}

func readJSONLMetadata(agent, path string) (Summary, bool, error) {
	item := Summary{Agent: agent, Source: path}
	file, err := os.Open(path)
	if err != nil {
		return item, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return item, false, err
	}
	item.UpdatedAt = sessionFileTimestamp(file, info)
	decode := jsonlRecordDecoder(agent)
	err = scanJSONLReader(file, func(line []byte) bool {
		var record map[string]any
		user := false
		if agent == "codex" {
			var envelope struct {
				Type    string
				Payload struct {
					Type, Role, ID, Cwd string
					SessionID           string `json:"session_id"`
					Timestamp, Source   any
				}
			}
			if json.Unmarshal(line, &envelope) != nil {
				return false
			}
			payload := envelope.Payload
			if envelope.Type == "session_meta" && item.ID == "" {
				item.ID = firstNonEmpty(payload.ID, payload.SessionID)
				item.Workspace = payload.Cwd
				item.StartedAt = timeValue(payload.Timestamp)
				item.IsSubagent = codexSubagentSource(payload.Source)
			}
			user = envelope.Type == "response_item" && payload.Type == "message" && payload.Role == "user"
		} else {
			if json.Unmarshal(line, &record) != nil {
				return false
			}
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
			case "pi":
				if stringValue(record["type"]) == "session" {
					item.ID = stringValue(record["id"])
					item.Workspace = stringValue(record["cwd"])
					item.StartedAt = timeValue(record["timestamp"])
				}
			}
			user = jsonlUserRecord(agent, record)
		}
		if user {
			if record == nil && json.Unmarshal(line, &record) != nil {
				return false
			}
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
		item.TurnCount = 0
		item.FirstUserMessage, item.LatestUserMessage = "", ""
		item.UserPrompts = nil
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
	item.FirstUserMessage, item.LatestUserMessage = "", ""
	item.TurnCount = 0
	item.UserPrompts = nil
	err := scanJSONLLines(item.Source, func(line []byte) bool {
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
			if message.Role == "user" {
				item.addUserMessage(text)
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

func sessionFileTimestamp(file *os.File, info os.FileInfo) string {
	timestamp := info.ModTime().UTC().Format(time.RFC3339Nano)
	_ = scanJSONLReverseReader(file, info.Size(), func(line []byte) bool {
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
