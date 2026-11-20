package sessions

import (
	"fmt"
	"path/filepath"
	"strings"
)

func discoverPiIn(root string, _ bool) ([]Summary, error) {
	return discoverJSONL("pi", filepath.Join(root, "*", "*.jsonl"))
}

func parsePi(path string, records []map[string]any) (Session, bool) {
	var addendum string
	return parsePiWithAddendum(path, records, &addendum)
}

func parsePiWithAddendum(path string, records []map[string]any, addendum *string) (Session, bool) {
	detail := newDetail("pi", path)
	var lastID string
	parents := map[string]string{}
	for _, record := range records {
		kind := stringValue(record["type"])
		if kind == "session" {
			detail.ID = stringValue(record["id"])
			detail.Workspace = stringValue(record["cwd"])
			detail.StartedAt = timeValue(record["timestamp"])
			continue
		}
		if kind != "message" {
			continue
		}
		message, _ := record["message"].(map[string]any)
		role := stringValue(message["role"])
		// Pi stores the tree id/parentId on the record. The record timestamp is write time;
		// the message's epoch-millis timestamp is event time. We keep both when they differ.
		eventTS := timeValue(message["timestamp"])
		recordTS := timeValue(record["timestamp"])
		ts := firstNonEmpty(eventTS, recordTS)
		recordedAt := ""
		if eventTS != "" && recordTS != "" && recordTS != eventTS {
			recordedAt = recordTS
		}
		parentID := stringValue(record["parentId"])
		for {
			parent, skipped := parents[parentID]
			if !skipped {
				break
			}
			parentID = parent
		}
		if parentID == "" {
			parentID = lastID
		}
		recordID := stringValue(record["id"])

		if role == "toolResult" {
			toolCallID := stringValue(message["toolCallId"])
			toolName := stringValue(message["toolName"])
			msgID := firstNonEmpty(recordID, fmt.Sprintf("pi-%d", len(detail.Messages)))
			isError, _ := message["isError"].(bool)
			m := Message{
				ID: msgID, ParentID: parentID, Role: "tool",
				Result:    message["content"],
				Timestamp: ts, RecordedAt: recordedAt,
				ToolUseID: toolCallID, ToolName: toolName, IsError: isError,
			}
			detail.Messages = append(detail.Messages, m)
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		if role == "assistant" {
			blocks := piAssistantBlocks(message["content"])
			if len(blocks) == 0 {
				continue
			}
			msgID := firstNonEmpty(recordID, fmt.Sprintf("pi-%d", len(detail.Messages)))
			m := Message{
				ID: msgID, ParentID: parentID, Role: "assistant",
				Content: blocks, Timestamp: ts, RecordedAt: recordedAt,
				Model:      stringValue(message["model"]),
				StopReason: stringValue(message["stopReason"]),
			}
			detail.Messages = append(detail.Messages, m)
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		if role == "system" {
			text := extractText(message["content"])
			if sections, structured := message["sections"].(map[string]any); structured {
				text = ""
				if value, changed := sections["addendum"]; changed {
					next := piAddendum(stringValue(value))
					if next != *addendum {
						text = next
					}
					*addendum = next
				}
			}
			if text == "" {
				if recordID != "" {
					parents[recordID] = parentID
				}
				continue
			}
			msgID := firstNonEmpty(recordID, fmt.Sprintf("pi-%d", len(detail.Messages)))
			m := Message{ID: msgID, ParentID: parentID, Role: "system", Content: []ContentBlock{{Type: "text", Text: text}}, Timestamp: ts, RecordedAt: recordedAt}
			detail.Messages = append(detail.Messages, m)
			updateTimeBounds(&detail, ts)
			lastID = msgID
			continue
		}

		if role != "user" {
			continue
		}
		var blocks []ContentBlock
		if text := extractText(message["content"]); text != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: text})
		}
		if len(blocks) == 0 {
			continue
		}
		msgID := firstNonEmpty(recordID, fmt.Sprintf("pi-%d", len(detail.Messages)))
		m := Message{ID: msgID, ParentID: parentID, Role: "user", Content: blocks, Timestamp: ts, RecordedAt: recordedAt}
		detail.Messages = append(detail.Messages, m)
		updateTimeBounds(&detail, ts)
		lastID = msgID
	}
	return finishDetail(detail)
}

func piAddendum(text string) string {
	if strings.HasPrefix(text, "<addendum>\n") && strings.HasSuffix(text, "\n</addendum>") {
		return strings.TrimSuffix(strings.TrimPrefix(text, "<addendum>\n"), "\n</addendum>")
	}
	return text
}

// PreparePiTranscript keeps imported instructions out of Pi's native tool-loadout records.
func PreparePiTranscript(detail Session, appended string) (Session, string) {
	var messages []Message
	var instructions []string
	for _, message := range detail.Messages {
		if message.Role == "system" {
			if text := TextContent(message.Content); text != "" {
				instructions = append(instructions, text)
			}
		} else {
			messages = append(messages, message)
		}
	}
	if appended != "" {
		instructions = append(instructions, appended)
	}
	detail.Messages = RechainMessages(messages)
	detail.MessageCount = len(messages)
	return detail, strings.Join(instructions, "\n\n")
}

func piAssistantBlocks(value any) []ContentBlock {
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
		switch kind {
		case "thinking":
			blocks = append(blocks, ContentBlock{
				Type:      "thinking",
				Text:      stringValue(obj["thinking"]),
				Signature: stringValue(obj["thinkingSignature"]),
			})
		case "toolCall":
			id := stringValue(obj["id"])
			name := stringValue(obj["name"])
			var input any
			if args, ok := obj["arguments"].(map[string]any); ok {
				input = args
			}
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    id,
				Name:  name,
				Input: input,
			})
		case "text":
			blocks = append(blocks, ContentBlock{Type: "text", Text: stringValue(obj["text"])})
		default:
			if text := stringValue(obj["text"]); text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		}
	}
	return blocks
}
