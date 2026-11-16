package sessions

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func discoverCodex(home string, _ bool) ([]Summary, error) {
	return discoverCodexIn(filepath.Join(home, ".codex", "sessions"), false)
}

func discoverCodexIn(root string, _ bool) ([]Summary, error) {
	return discoverCodexRollouts(root)
}

func discoverCodexRollouts(root string) ([]Summary, error) {
	items, err := discoverJSONL("codex", filepath.Join(root, "*", "*", "*", "*.jsonl"))
	best := make(map[string]Summary)
	for _, item := range items {
		previous, ok := best[item.ID]
		if !ok || item.UpdatedAt > previous.UpdatedAt ||
			(item.UpdatedAt == previous.UpdatedAt && item.Source > previous.Source) {
			best[item.ID] = item
		}
	}
	items = make([]Summary, 0, len(best))
	for _, item := range best {
		items = append(items, item)
	}
	return items, err
}

func parseCodex(path string, records []map[string]any) (Session, bool) {
	detail := newDetail("codex", path)
	// Track function_call names by call_id for tool result messages
	callNames := map[string]string{}
	var lastID string
	for _, record := range records {
		kind := stringValue(record["type"])
		payload, _ := record["payload"].(map[string]any)
		if kind == "session_meta" {
			if detail.ID == "" {
				detail.ID = firstNonEmpty(stringValue(payload["id"]), stringValue(payload["session_id"]))
				detail.Workspace = stringValue(payload["cwd"])
				detail.StartedAt = timeValue(payload["timestamp"])
				detail.IsSubagent = codexSubagentSource(payload["source"])
			}
			continue
		}
		if kind != "response_item" {
			continue
		}
		ts := timeValue(record["timestamp"])
		payloadType := stringValue(payload["type"])

		if payloadType == "function_call" || payloadType == "custom_tool_call" {
			callID := stringValue(payload["call_id"])
			name := stringValue(payload["name"])
			callNames[callID] = name
			rawInput := stringValue(payload["arguments"])
			if payloadType == "custom_tool_call" {
				rawInput = stringValue(payload["input"])
			}
			var input any
			if rawInput != "" {
				var parsed map[string]any
				if json.Unmarshal([]byte(rawInput), &parsed) == nil && parsed != nil {
					input = parsed
				} else {
					input = map[string]any{"raw": rawInput}
				}
			}
			block := ContentBlock{Type: "tool_use", ID: callID, Name: name, Input: input}
			// Append to last assistant message if possible
			if n := len(detail.Messages); n > 0 && detail.Messages[n-1].Role == "assistant" {
				detail.Messages[n-1].Content = append(detail.Messages[n-1].Content, block)
			} else {
				msgID := fmt.Sprintf("codex-%d", len(detail.Messages))
				detail.Messages = append(detail.Messages, Message{
					ID: msgID, ParentID: lastID, Role: "assistant",
					Content: []ContentBlock{block}, Timestamp: ts,
				})
				updateTimeBounds(&detail, ts)
				lastID = msgID
			}
			continue
		}

		if payloadType == "reasoning" {
			var summary []string
			if items, ok := payload["summary"].([]any); ok {
				for _, item := range items {
					if obj, ok := item.(map[string]any); ok {
						if text := stringValue(obj["text"]); text != "" {
							summary = append(summary, text)
						}
					}
				}
			}
			block := ContentBlock{Type: "thinking", Text: strings.Join(summary, "\n"), Signature: stringValue(payload["encrypted_content"]), ID: stringValue(payload["id"])}
			if n := len(detail.Messages); n > 0 && detail.Messages[n-1].Role == "assistant" {
				detail.Messages[n-1].Content = append(detail.Messages[n-1].Content, block)
			} else {
				msgID := fmt.Sprintf("codex-%d", len(detail.Messages))
				detail.Messages = append(detail.Messages, Message{
					ID: msgID, ParentID: lastID, Role: "assistant",
					Content: []ContentBlock{block}, Timestamp: ts,
				})
				updateTimeBounds(&detail, ts)
				lastID = msgID
			}
			continue
		}

		if payloadType == "function_call_output" || payloadType == "custom_tool_call_output" {
			callID := stringValue(payload["call_id"])
			msgID := fmt.Sprintf("codex-%d", len(detail.Messages))
			m := Message{
				ID: msgID, ParentID: lastID, Role: "tool",
				Result:    payload["output"],
				Timestamp: ts, ToolUseID: callID, ToolName: callNames[callID],
			}
			detail.Messages = append(detail.Messages, m)
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		if payloadType != "message" {
			continue
		}
		role := stringValue(payload["role"])
		if role != "user" && role != "assistant" && role != "developer" {
			continue
		}
		if role == "developer" {
			role = "system"
		}
		blocks := codexContentBlocks(payload["content"], role)
		if len(blocks) == 0 {
			continue
		}
		msgID := stringValue(payload["id"])
		if msgID == "" {
			msgID = fmt.Sprintf("codex-%d", len(detail.Messages))
		}
		m := Message{ID: msgID, ParentID: lastID, Role: role, Content: blocks, Timestamp: ts}
		detail.Messages = append(detail.Messages, m)
		updateTimeBounds(&detail, ts)
		lastID = msgID
	}
	return finishDetail(detail)
}

func codexSubagentSource(value any) bool {
	source, _ := value.(map[string]any)
	_, ok := source["subagent"]
	return ok
}

func codexContentBlocks(value any, role string) []ContentBlock {
	items, ok := value.([]any)
	if !ok {
		if s, ok := value.(string); ok && s != "" {
			return []ContentBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []ContentBlock
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind := stringValue(obj["type"])
		text := stringValue(obj["text"])
		switch kind {
		case "input_text", "output_text":
			if _, present := obj["text"].(string); present {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		case "input_image":
			if source := parseCodexImageURL(stringValue(obj["image_url"])); source != nil {
				blocks = append(blocks, ContentBlock{Type: "image", Source: source})
			}
		default:
			if text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		}
	}
	return blocks
}

func parseCodexImageURL(url string) map[string]any {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		if url != "" {
			return map[string]any{"type": "url", "url": url}
		}
		return nil
	}
	rest := url[len(prefix):]
	semi := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semi < 0 || comma < 0 || comma < semi {
		return nil
	}
	mediaType := rest[:semi]
	data := rest[comma+1:]
	return map[string]any{"type": "base64", "media_type": mediaType, "data": data}
}
