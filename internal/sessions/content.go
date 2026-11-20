package sessions

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func newDetail(agent, source string) Session {
	return Session{Provider: agent, Source: source}
}

func updateTimeBounds(detail *Session, timestamp string) {
	if detail.StartedAt == "" || timestamp != "" && timestamp < detail.StartedAt {
		detail.StartedAt = timestamp
	}
	if timestamp > detail.UpdatedAt {
		detail.UpdatedAt = timestamp
	}
}

func finishDetail(detail Session) (Session, bool) {
	if detail.ID == "" {
		return Session{}, false
	}
	for i := range detail.Messages {
		if detail.Messages[i].Role == "assistant" {
			liftThinkTags(detail.Messages[i].Content)
		}
	}
	detail.MessageCount = len(detail.Messages)
	for _, message := range detail.Messages {
		text := textContent(message.Content)
		if message.Role == "user" && isSessionPreview(text) {
			detail.IsSubagent = detail.IsSubagent || hasSubagentMarker(text)
			if detail.Title == "" {
				detail.Title = oneLine(text, 96)
			}
			break
		}
	}
	return detail, true
}

// Exact markers keep ordinary text and body whitespace intact.
func liftThinkTags(blocks []ContentBlock) {
	for i, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if rest, ok := strings.CutSuffix(b.Text, importedNoteSuffix); ok {
			blocks[i] = ContentBlock{Type: "thinking", Text: rest}
			continue
		}
		inner, ok := strings.CutPrefix(strings.TrimSpace(b.Text), "<think>")
		if !ok {
			continue
		}
		inner, ok = strings.CutSuffix(inner, "</think>")
		if !ok || strings.Contains(inner, "<think>") || strings.Contains(inner, "</think>") {
			continue
		}
		blocks[i] = ContentBlock{Type: "thinking", Text: strings.TrimSuffix(strings.TrimPrefix(inner, "\n"), "\n")}
	}
}

func extractText(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []any:
		var parts []string
		for _, child := range item {
			if object, ok := child.(map[string]any); ok {
				kind := stringValue(object["type"])
				if kind == "thinking" || kind == "tool_use" || kind == "toolCall" {
					continue
				}
				parts = append(parts, firstNonEmpty(stringValue(object["text"]), stringValue(object["output"]), extractText(object["content"])))
				continue
			}
			parts = append(parts, extractText(child))
		}
		return strings.Join(nonEmpty(parts), "\n")
	case map[string]any:
		return firstNonEmpty(stringValue(item["text"]), stringValue(item["output"]), extractText(item["content"]))
	default:
		return ""
	}
}

func TextContent(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// MessageText returns the plain text of any message: block text for
// conversational roles, the derived result text for tool messages.
func MessageText(m Message) string {
	if m.Role == "tool" {
		return resultText(m.Result)
	}
	return TextContent(m.Content)
}

func textContent(blocks []ContentBlock) string {
	return TextContent(blocks)
}

func mapContentBlocks(value any) []ContentBlock {
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
			if s, ok := item.(string); ok {
				blocks = append(blocks, ContentBlock{Type: "text", Text: s})
			}
			continue
		}
		kind := stringValue(obj["type"])
		switch kind {
		case "thinking":
			blocks = append(blocks, ContentBlock{
				Type:      "thinking",
				Text:      stringValue(obj["thinking"]),
				Signature: stringValue(obj["signature"]),
			})
		case "tool_use":
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    stringValue(obj["id"]),
				Name:  stringValue(obj["name"]),
				Input: obj["input"],
			})
		case "text":
			blocks = append(blocks, ContentBlock{Type: "text", Text: stringValue(obj["text"])})
		default:
			if text := firstNonEmpty(stringValue(obj["text"]), stringValue(obj["output"])); text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		}
	}
	return blocks
}

func nonEmptyBlocks(blocks []ContentBlock) []ContentBlock {
	result := blocks[:0]
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) == "" {
			continue
		}
		result = append(result, b)
	}
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func timeValue(value any) string {
	switch item := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, item); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return item
	case float64:
		seconds := int64(item) / 1000
		nanos := (int64(item) % 1000) * int64(time.Millisecond)
		return time.Unix(seconds, nanos).UTC().Format(time.RFC3339Nano)
	case json.Number:
		value, _ := strconv.ParseInt(item.String(), 10, 64)
		return timeValue(float64(value))
	default:
		return ""
	}
}

func cleanPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}

func isSessionTitle(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.HasPrefix(value, "# AGENTS.md instructions") && !strings.HasPrefix(value, "<environment_context>")
}

func isSessionPreview(value string) bool {
	if !isSessionTitle(value) || isClaudeLocalCommandText(value) {
		return false
	}
	value = strings.TrimSpace(value)
	if value == "[Request interrupted by user]" || value == "[Request interrupted by user for tool use]" {
		return false
	}
	for _, tag := range []string{"task-notification", "subagent_notification"} {
		if strings.HasPrefix(value, "<"+tag+">") && strings.HasSuffix(value, "</"+tag+">") {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nonEmpty(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
