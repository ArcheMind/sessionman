package sessions

import (
	"fmt"
	"path/filepath"
	"strings"
)

func discoverClaude(home string, includeSubagents bool) ([]Summary, error) {
	return discoverClaudeIn(filepath.Join(home, ".claude", "projects"), includeSubagents)
}

func discoverClaudeIn(root string, includeSubagents bool) ([]Summary, error) {
	patterns := []string{filepath.Join(root, "*", "*.jsonl")}
	if includeSubagents {
		patterns = append(patterns, filepath.Join(root, "*", "*", "subagents", "*.jsonl"))
	}
	return discoverJSONL("claude", patterns...)
}

func parseClaude(path string, records []map[string]any) (Session, bool) {
	detail := newDetail("claude", path)
	var lastID string
	for _, record := range records {
		kind := stringValue(record["type"])
		if sidechain, _ := record["isSidechain"].(bool); sidechain {
			detail.IsSubagent = true
			if agentID := stringValue(record["agentId"]); agentID != "" {
				detail.ID = agentID
			}
		} else if detail.ID == "" {
			detail.ID = stringValue(record["sessionId"])
		}
		if detail.Workspace == "" {
			detail.Workspace = stringValue(record["cwd"])
		}
		if kind != "user" && kind != "assistant" && kind != "system" {
			continue
		}
		message, _ := record["message"].(map[string]any)
		ts := timeValue(record["timestamp"])
		parentID := stringValue(record["parentUuid"])
		if parentID == "" {
			parentID = lastID
		}

		if kind == "system" {
			// Most system records are bookkeeping without text; only text-bearing ones are conversation-visible.
			text := stringValue(record["content"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgID := stringValue(record["uuid"])
			if msgID == "" {
				msgID = fmt.Sprintf("claude-%d", len(detail.Messages))
			}
			detail.Messages = append(detail.Messages, Message{
				ID: msgID, ParentID: parentID, Role: "system",
				Content: []ContentBlock{{Type: "text", Text: text}}, Timestamp: ts,
			})
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		if kind == "assistant" {
			blocks := mapContentBlocks(message["content"])
			if len(blocks) == 0 {
				continue
			}
			msgID := stringValue(record["uuid"])
			if msgID == "" {
				msgID = fmt.Sprintf("claude-%d", len(detail.Messages))
			}
			m := Message{
				ID: msgID, ParentID: parentID, Role: "assistant",
				Content: blocks, Timestamp: ts,
				Model:      stringValue(message["model"]),
				StopReason: stringValue(message["stop_reason"]),
			}
			detail.Messages = append(detail.Messages, m)
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		// kind == "user": may contain tool_result blocks mixed with text
		nativeContent, _ := message["content"].([]any)
		var userBlocks []ContentBlock
		for _, item := range nativeContent {
			obj, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					userBlocks = append(userBlocks, ContentBlock{Type: "text", Text: s})
				}
				continue
			}
			blockType := stringValue(obj["type"])
			if blockType == "tool_result" {
				toolUseID := stringValue(obj["tool_use_id"])
				toolName := claudeToolNameByID(&detail, toolUseID)
				toolMsgID := fmt.Sprintf("claude-%d", len(detail.Messages))
				isError, _ := obj["is_error"].(bool)
				toolMsg := Message{
					ID: toolMsgID, ParentID: lastID, Role: "tool",
					Result:    obj["content"],
					Timestamp: ts, ToolUseID: toolUseID, ToolName: toolName, IsError: isError,
				}
				detail.Messages = append(detail.Messages, toolMsg)
				updateTimeBounds(&detail, ts)
				lastID = toolMsgID
				continue
			}
			if blockType == "image" {
				if source, ok := obj["source"].(map[string]any); ok {
					userBlocks = append(userBlocks, ContentBlock{Type: "image", Source: source})
				}
				continue
			}
			userBlocks = append(userBlocks, ContentBlock{Type: "text", Text: firstNonEmpty(stringValue(obj["text"]), extractText(obj["content"]))})
		}
		// If nativeContent was nil, content is a plain string
		if nativeContent == nil {
			if s := extractText(message["content"]); s != "" {
				userBlocks = append(userBlocks, ContentBlock{Type: "text", Text: s})
			}
		}
		userBlocks = nonEmptyBlocks(userBlocks)
		if len(userBlocks) == 0 {
			continue
		}
		msgID := stringValue(record["uuid"])
		if msgID == "" {
			msgID = fmt.Sprintf("claude-%d", len(detail.Messages))
		}
		m := Message{ID: msgID, ParentID: parentID, Role: "user", Content: userBlocks, Timestamp: ts}
		detail.Messages = append(detail.Messages, m)
		updateTimeBounds(&detail, ts)
		lastID = msgID
	}
	return finishDetail(detail)
}

func claudeToolNameByID(detail *Session, toolUseID string) string {
	for i := len(detail.Messages) - 1; i >= 0; i-- {
		for _, b := range detail.Messages[i].Content {
			if b.Type == "tool_use" && b.ID == toolUseID {
				return b.Name
			}
		}
	}
	return ""
}
