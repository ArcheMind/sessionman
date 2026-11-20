package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type openCodeMessage struct {
	id        string
	role      string
	timestamp string
}

func discoverOpenCodeIn(root string, includeSubagents bool) ([]Summary, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "storage", "session", "info", "*.json"))
	if err != nil {
		return nil, err
	}
	var items []Summary
	var failures []error
	for _, path := range paths {
		item, messages, ok, err := openCodeIndex(path)
		if err == nil && ok && (includeSubagents || !item.IsSubagent) {
			err = openCodeBounds(&item, messages)
			if err == nil {
				err = openCodeUserPreview(&item, messages, false)
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("read opencode session %s: %w", path, err))
			continue
		}
		if ok && (includeSubagents || !item.IsSubagent) {
			items = append(items, item)
		}
	}
	return items, errors.Join(failures...)
}

func summarizeOpenCode(item Summary, includeCount bool) (Summary, error) {
	_, messages, ok, err := openCodeIndex(item.Source)
	if err != nil {
		return item, err
	}
	if !ok {
		return item, fmt.Errorf("parse opencode session %s: not a recognizable session", item.Source)
	}
	item.TurnCount = 0
	item.FirstUserMessage, item.LatestUserMessage = "", ""
	item.UserPrompts = nil
	if includeCount {
		item.MessageCount = 0
	}
	for _, message := range messages {
		if !includeCount && message.role != "user" {
			continue
		}
		blocks, tools, text, err := openCodePartSummary(item, message)
		if err != nil {
			return item, err
		}
		if message.role == "user" {
			item.addUserMessage(text)
		}
		if includeCount {
			if blocks {
				item.MessageCount++
			}
			item.MessageCount += tools
		}
	}
	return item, nil
}

func openCodeIndex(path string) (Summary, []openCodeMessage, bool, error) {
	item := Summary{Agent: "opencode", Source: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return item, nil, false, err
	}
	var info struct {
		ID       string `json:"id"`
		ParentID string `json:"parentID"`
		Title    string `json:"title"`
		Time     struct {
			Created any `json:"created"`
			Updated any `json:"updated"`
		} `json:"time"`
	}
	if json.Unmarshal(data, &info) != nil || info.ID == "" {
		return item, nil, false, nil
	}
	item.ID = info.ID
	item.IsSubagent = info.ParentID != ""
	item.Title = info.Title
	item.StartedAt = timeValue(info.Time.Created)
	item.UpdatedAt = timeValue(info.Time.Updated)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(path)), "message", item.ID, "*.json"))
	if err != nil {
		return item, nil, false, err
	}
	var messages []openCodeMessage
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return item, nil, false, err
		}
		var message struct {
			ID   string `json:"id"`
			Role string `json:"role"`
			Path struct {
				Root string `json:"root"`
				CWD  string `json:"cwd"`
			} `json:"path"`
			Time struct {
				Created any `json:"created"`
			} `json:"time"`
		}
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if item.Workspace == "" {
			item.Workspace = firstNonEmpty(message.Path.Root, message.Path.CWD)
		}
		messages = append(messages, openCodeMessage{id: message.ID, role: message.Role, timestamp: timeValue(message.Time.Created)})
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].timestamp < messages[j].timestamp })
	return item, messages, true, nil
}

func openCodePartPaths(item Summary, message openCodeMessage) ([]string, error) {
	return filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(item.Source)), "part", item.ID, message.id, "*.json"))
}

func openCodePartSummary(item Summary, message openCodeMessage) (bool, int, string, error) {
	paths, err := openCodePartPaths(item, message)
	if err != nil {
		return false, 0, "", err
	}
	blocks, tools := false, 0
	var texts []string
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, 0, "", err
		}
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &part) != nil {
			continue
		}
		switch part.Type {
		case "text":
			if strings.TrimSpace(part.Text) != "" {
				blocks = true
				texts = append(texts, part.Text)
			}
		case "step-start", "patch":
			blocks = true
		case "step-finish":
			tools++
		}
	}
	return blocks, tools, strings.Join(texts, "\n"), nil
}

func openCodeBounds(item *Summary, messages []openCodeMessage) error {
	for _, reverse := range []bool{false, true} {
		for n := range messages {
			i := n
			if reverse {
				i = len(messages) - 1 - n
			}
			message := messages[i]
			if message.timestamp == "" {
				continue
			}
			if reverse && message.timestamp <= item.UpdatedAt || !reverse && item.StartedAt != "" && message.timestamp >= item.StartedAt {
				break
			}
			blocks, tools, _, err := openCodePartSummary(*item, message)
			if err != nil {
				return err
			}
			if !blocks && tools == 0 {
				continue
			}
			if reverse {
				item.UpdatedAt = message.timestamp
			} else {
				item.StartedAt = message.timestamp
			}
			break
		}
	}
	return nil
}

func openCodeUserPreview(item *Summary, messages []openCodeMessage, latest bool) error {
	for n := range messages {
		i := n
		if latest {
			i = len(messages) - 1 - n
		}
		if messages[i].role != "user" {
			continue
		}
		_, _, text, err := openCodePartSummary(*item, messages[i])
		if err != nil {
			return err
		}
		if !isSessionPreview(text) {
			continue
		}
		if latest {
			item.LatestUserMessage = oneLine(text, 60)
		} else {
			item.addUserMessage(text)
			if item.Title == "" {
				item.Title = oneLine(text, 96)
			}
		}
		break
	}
	return nil
}

func readOpenCodeSession(path string) (Session, error) {
	item, messages, ok, err := openCodeIndex(path)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, fmt.Errorf("parse opencode session %s: not a recognizable session", path)
	}
	detail := Session{ID: item.ID, Provider: item.Agent, Source: item.Source, Workspace: item.Workspace, Title: item.Title, IsSubagent: item.IsSubagent, StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt}
	var lastID string
	for _, message := range messages {
		paths, err := openCodePartPaths(item, message)
		if err != nil {
			return Session{}, err
		}
		var blocks []ContentBlock
		var toolMessages []Message
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return Session{}, err
			}
			var part map[string]any
			if json.Unmarshal(data, &part) != nil {
				continue
			}
			switch stringValue(part["type"]) {
			case "text":
				blocks = append(blocks, ContentBlock{Type: "text", Text: stringValue(part["text"])})
			case "step-start":
				blocks = append(blocks, ContentBlock{Type: "tool_use", ID: stringValue(part["id"]), Name: "step"})
			case "step-finish":
				tokenInfo := ""
				if tokens, ok := part["tokens"].(map[string]any); ok {
					data, _ := json.Marshal(tokens)
					tokenInfo = string(data)
				}
				toolMessages = append(toolMessages, Message{ID: fmt.Sprintf("oc-%d-tool", len(detail.Messages)+len(toolMessages)), Role: "tool", Result: tokenInfo, Timestamp: message.timestamp, ToolUseID: stringValue(part["id"]), ToolName: "step"})
			case "patch":
				var input map[string]any
				if h := stringValue(part["hash"]); h != "" {
					input = map[string]any{"hash": h}
				}
				if files, ok := part["files"].([]any); ok {
					if input == nil {
						input = map[string]any{}
					}
					input["files"] = files
				}
				blocks = append(blocks, ContentBlock{Type: "tool_use", ID: stringValue(part["id"]), Name: "patch", Input: input})
			}
		}
		blocks = nonEmptyBlocks(blocks)
		if len(blocks) > 0 {
			msgID := fmt.Sprintf("oc-%d", len(detail.Messages))
			detail.Messages = append(detail.Messages, Message{ID: msgID, ParentID: lastID, Role: message.role, Content: blocks, Timestamp: message.timestamp})
			updateTimeBounds(&detail, message.timestamp)
			lastID = msgID
		}
		for i := range toolMessages {
			toolMessages[i].ParentID = lastID
			detail.Messages = append(detail.Messages, toolMessages[i])
			updateTimeBounds(&detail, message.timestamp)
			lastID = toolMessages[i].ID
		}
	}
	detail, _ = finishDetail(detail)
	return detail, nil
}
