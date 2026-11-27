package sessions

import (
	"encoding/json"
	"fmt"
	"strings"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
)

// ParseNativeProtocolFile reads one native session file directly into Protocol 2.0
// (entries[] + runtimeEvents[]), classifying records by origin and stripping carrier scaffolding.
func ParseNativeProtocolFile(agentID, path string) (ProtocolSession, error) {
	switch agentID {
	case "agy":
		legacy, err := readAgySession(path)
		if err != nil {
			return ProtocolSession{}, err
		}
		return upgradeAgyLegacy(legacy)
	case "claude", "codex", "pi":
		records, err := readJSONL(path)
		if err != nil {
			return ProtocolSession{}, fmt.Errorf("read %s session %s: %w", agentID, path, err)
		}
		ps, ok := ParseProtocolRecords(agentID, records, path)
		if !ok {
			return ProtocolSession{}, fmt.Errorf("parse %s session %s: not a recognizable session", agentID, path)
		}
		if item, _, err := readJSONLMetadata(agentID, path); err == nil {
			if item.Workspace != "" {
				ps.Workspace = item.Workspace
			}
			ps.IsSubagent = item.IsSubagent
			if item.StartedAt != "" {
				ps.StartedAt = item.StartedAt
			}
			if item.UpdatedAt != "" {
				ps.UpdatedAt = item.UpdatedAt
			}
		}
		ps.Source = path
		return ps, nil
	default:
		legacy, err := ParseNativeSessionFile(agentID, path)
		if err != nil {
			return ProtocolSession{}, err
		}
		return ToProtocolSession(legacy)
	}
}

// ParseProtocolRecords parses native JSONL records for a provider into Protocol 2.0.
func ParseProtocolRecords(agentID string, records []map[string]any, source string) (ProtocolSession, bool) {
	switch agentID {
	case "claude":
		return parseClaudeProtocol(source, records)
	case "codex":
		return parseCodexProtocol(source, records)
	case "pi":
		legacy, ok := parsePi(source, records)
		if !ok {
			return ProtocolSession{}, false
		}
		ps, err := ToProtocolSession(legacy)
		if err != nil {
			return ProtocolSession{}, false
		}
		return ps, true
	default:
		legacy, ok := ParseRecords(agentID, records)
		if !ok {
			return ProtocolSession{}, false
		}
		ps, err := ToProtocolSession(legacy)
		if err != nil {
			return ProtocolSession{}, false
		}
		return ps, true
	}
}

func upgradeAgyLegacy(legacy Session) (ProtocolSession, error) {
	ps, err := ToProtocolSession(legacy)
	if err != nil {
		return ProtocolSession{}, err
	}
	for i := range ps.Entries {
		e := &ps.Entries[i]
		if e.Source != transcript.SourceS2 {
			continue
		}
		// AGY sometimes emits an unclosed "<think>..." prefix inside field 1 text.
		// Lift it into a thinking block at the import boundary so downstream target
		// serializers handle it via foreign-thinking policy instead of leaking CoT.
		var normalized []ContentBlock
		for _, b := range e.Content {
			if b.Type == transcript.BlockText && strings.HasPrefix(strings.TrimSpace(b.Text), "<think>") && !strings.Contains(b.Text, "</think>") {
				trimmed := strings.TrimPrefix(strings.TrimSpace(b.Text), "<think>")
				trimmed = strings.TrimPrefix(trimmed, "\n")
				if trimmed != "" {
					normalized = append(normalized, ContentBlock{Type: transcript.BlockThinking, Text: trimmed})
				}
				continue
			}
			normalized = append(normalized, b)
		}
		e.Content = normalized
	}
	return ps, nil
}

func newProtocolSession(provider, source string) ProtocolSession {
	return ProtocolSession{
		ProtocolVersion: transcript.ProtocolVersion,
		Provider:        provider,
		Source:          source,
	}
}

func finishProtocolSession(ps ProtocolSession) (ProtocolSession, bool) {
	if ps.ID == "" {
		return ProtocolSession{}, false
	}
	for i := range ps.Entries {
		if ps.Entries[i].Source == transcript.SourceS2 {
			liftThinkTags(ps.Entries[i].Content)
		}
	}
	ps.EntryCount = len(ps.Entries)
	for _, e := range ps.Entries {
		if e.Source == transcript.SourceUser && e.Mutability == transcript.MutabilityDelta {
			text := transcript.TextContent(e)
			if isSessionPreview(text) {
				ps.IsSubagent = ps.IsSubagent || hasSubagentMarker(text)
				if ps.Title == "" {
					ps.Title = oneLine(text, 96)
				}
				break
			}
		}
	}
	return ps, true
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func updateProtocolTimeBounds(ps *ProtocolSession, ts string) {
	if ps.StartedAt == "" || (ts != "" && ts < ps.StartedAt) {
		ps.StartedAt = ts
	}
	if ts > ps.UpdatedAt {
		ps.UpdatedAt = ts
	}
}

func parseClaudeProtocol(path string, records []map[string]any) (ProtocolSession, bool) {
	ps := newProtocolSession("claude", path)
	var lastID string
	toolNames := map[string]string{}

	for idx, record := range records {
		kind := stringValue(record["type"])
		if sidechain, _ := record["isSidechain"].(bool); sidechain {
			ps.IsSubagent = true
			if agentID := stringValue(record["agentId"]); agentID != "" {
				ps.ID = agentID
			}
		} else if ps.ID == "" {
			ps.ID = stringValue(record["sessionId"])
		}
		if ps.Workspace == "" {
			ps.Workspace = stringValue(record["cwd"])
		}
		ts := timeValue(record["timestamp"])

		if kind == "attachment" {
			att, _ := record["attachment"].(map[string]any)
			attType := stringValue(att["type"])
			switch attType {
			case "relevant_memories":
				if content := extractText(att["content"]); content != "" {
					id := firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-att-%d", idx))
					ps.Entries = append(ps.Entries, Entry{
						ID: id, ParentID: strPtr(lastID), Source: transcript.SourceS1,
						Mutability: transcript.MutabilityCurrent, Slot: "relevant-memories",
						Content:   []ContentBlock{{Type: transcript.BlockText, Text: content}},
						Timestamp: ts,
					})
					lastID = id
				}
			case "instructions":
				if content := extractText(att["content"]); content != "" {
					id := firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-att-%d", idx))
					ps.Entries = append(ps.Entries, Entry{
						ID: id, ParentID: strPtr(lastID), Source: transcript.SourceWorld,
						Mutability: transcript.MutabilityConstant, Slot: "instructions",
						Content:   []ContentBlock{{Type: transcript.BlockText, Text: content}},
						Timestamp: ts,
					})
					lastID = id
				}
			}
			// All other attachments (environment, model, skill_listing, auto_mode, ...) are carrier scaffolding.
			continue
		}

		if kind != "user" && kind != "assistant" && kind != "system" {
			continue
		}
		message, _ := record["message"].(map[string]any)
		parentID := firstNonEmpty(stringValue(record["parentUuid"]), lastID)

		if kind == "system" {
			subtype := stringValue(record["subtype"])
			if strings.HasPrefix(subtype, "model_refusal") {
				ps.RuntimeEvents = append(ps.RuntimeEvents, RuntimeEvent{
					ID:           firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-rt-%d", idx)),
					AfterEntryID: lastID,
					Provider:     "claude",
					Kind:         transcript.RuntimeSafetyRefusal,
					Message:      firstNonEmpty(stringValue(record["content"]), subtype),
					Timestamp:    ts,
				})
				continue
			}
			text := stringValue(record["content"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgID := firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-%d", len(ps.Entries)))
			ps.Entries = append(ps.Entries, Entry{
				ID: msgID, ParentID: strPtr(parentID), Source: transcript.SourceS1, Mutability: transcript.MutabilityDelta,
				Content: []ContentBlock{{Type: transcript.BlockText, Text: text}}, Timestamp: ts,
			})
			updateProtocolTimeBounds(&ps, ts)
			lastID = msgID
			continue
		}

		if kind == "assistant" {
			model := stringValue(message["model"])
			stopReason := stringValue(message["stop_reason"])
			isErrorMsg, _ := record["isApiErrorMessage"].(bool)
			blocks := mapContentBlocks(message["content"])
			textSummary := ""
			for _, b := range blocks {
				if b.Type == transcript.BlockToolUse {
					toolNames[b.ID] = b.Name
				} else if b.Type == transcript.BlockText {
					textSummary += b.Text
				}
			}
			if isErrorMsg || model == "<synthetic>" || stopReason == "refusal" || strings.Contains(textSummary, "safeguards flagged this message") {
				rtKind := transcript.RuntimeProviderError
				code := ""
				if stopReason == "refusal" || strings.Contains(textSummary, "safeguards flagged") || strings.Contains(textSummary, "Terms of Service") {
					rtKind = transcript.RuntimeSafetyRefusal
				}
				if strings.Contains(textSummary, "[reasoning_extraction]") {
					code = "reasoning_extraction"
				}
				ps.RuntimeEvents = append(ps.RuntimeEvents, RuntimeEvent{
					ID:           firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-rt-%d", idx)),
					AfterEntryID: lastID,
					Provider:     "claude",
					Model:        model,
					Kind:         rtKind,
					Code:         code,
					Message:      textSummary,
					Timestamp:    ts,
				})
				continue
			}
			if len(blocks) == 0 {
				continue
			}
			msgID := firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-%d", len(ps.Entries)))
			ps.Entries = append(ps.Entries, Entry{
				ID: msgID, ParentID: strPtr(parentID), Source: transcript.SourceS2, Mutability: transcript.MutabilityDelta,
				Content: blocks, Timestamp: ts,
				Producer:   &Producer{Provider: "claude", Model: model},
				StopReason: stopReason,
			})
			updateProtocolTimeBounds(&ps, ts)
			lastID = msgID
			continue
		}

		// kind == "user"
		isMeta, _ := record["isMeta"].(bool)
		isCompact, _ := record["isCompactSummary"].(bool)
		nativeContent, _ := message["content"].([]any)
		var userBlocks []ContentBlock
		for _, item := range nativeContent {
			obj, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					userBlocks = append(userBlocks, ContentBlock{Type: transcript.BlockText, Text: s})
				}
				continue
			}
			blockType := stringValue(obj["type"])
			if blockType == "tool_result" {
				toolUseID := stringValue(obj["tool_use_id"])
				toolMsgID := fmt.Sprintf("claude-%d", len(ps.Entries))
				isError, _ := obj["is_error"].(bool)
				ps.Entries = append(ps.Entries, Entry{
					ID: toolMsgID, ParentID: strPtr(lastID), Source: transcript.SourceWorld, Mutability: transcript.MutabilityDelta,
					ToolResult: &ToolResult{
						ToolUseID: toolUseID,
						ToolName:  toolNames[toolUseID],
						Result:    obj["content"],
						IsError:   isError,
					},
					Timestamp: ts,
				})
				updateProtocolTimeBounds(&ps, ts)
				lastID = toolMsgID
				continue
			}
			if blockType == "image" {
				if source, ok := obj["source"].(map[string]any); ok {
					userBlocks = append(userBlocks, ContentBlock{Type: transcript.BlockImage, Source: source})
				}
				continue
			}
			userBlocks = append(userBlocks, ContentBlock{Type: transcript.BlockText, Text: firstNonEmpty(stringValue(obj["text"]), extractText(obj["content"]))})
		}
		if nativeContent == nil {
			if s := extractText(message["content"]); s != "" {
				userBlocks = append(userBlocks, ContentBlock{Type: transcript.BlockText, Text: s})
			}
		}
		userBlocks = nonEmptyBlocks(userBlocks)
		if len(userBlocks) == 0 {
			continue
		}
		msgID := firstNonEmpty(stringValue(record["uuid"]), fmt.Sprintf("claude-%d", len(ps.Entries)))
		entry := Entry{
			ID: msgID, ParentID: strPtr(parentID), Source: transcript.SourceUser, Mutability: transcript.MutabilityDelta,
			Content: userBlocks, Timestamp: ts,
		}
		if isCompact {
			entry.Source = transcript.SourceS1
			entry.Mutability = transcript.MutabilityCurrent
			entry.Slot = "compaction-summary"
		} else if isMeta {
			entry.Source = transcript.SourceS1
		}
		ps.Entries = append(ps.Entries, entry)
		updateProtocolTimeBounds(&ps, ts)
		lastID = msgID
	}
	return finishProtocolSession(ps)
}

func isCodexCarrierDeveloper(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"<permissions instructions>",
		"<collaboration_mode>",
		"<skills_instructions>",
		"<apps_instructions>",
		"<tools>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func parseCodexProtocol(path string, records []map[string]any) (ProtocolSession, bool) {
	ps := newProtocolSession("codex", path)
	callNames := map[string]string{}
	var lastID string

	for idx, record := range records {
		kind := stringValue(record["type"])
		payload, _ := record["payload"].(map[string]any)
		if kind == "session_meta" {
			if ps.ID == "" {
				ps.ID = firstNonEmpty(stringValue(payload["id"]), stringValue(payload["session_id"]))
				ps.Workspace = stringValue(payload["cwd"])
				ps.StartedAt = timeValue(payload["timestamp"])
				ps.IsSubagent = codexSubagentSource(payload["source"])
			}
			continue
		}
		ts := timeValue(record["timestamp"])
		if kind == "compacted" {
			if msg := stringValue(payload["message"]); msg != "" {
				id := fmt.Sprintf("codex-compact-%d", idx)
				ps.Entries = append(ps.Entries, Entry{
					ID: id, ParentID: strPtr(lastID), Source: transcript.SourceS1,
					Mutability: transcript.MutabilityCurrent, Slot: "compaction-summary",
					Content: []ContentBlock{{Type: transcript.BlockText, Text: msg}}, Timestamp: ts,
				})
				lastID = id
			}
			continue
		}
		if kind != "response_item" {
			continue
		}
		payloadType := stringValue(payload["type"])
		switch payloadType {
		case "function_call", "custom_tool_call":
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
			block := ContentBlock{Type: transcript.BlockToolUse, ID: callID, Name: name, Input: input}
			if n := len(ps.Entries); n > 0 && ps.Entries[n-1].Source == transcript.SourceS2 {
				ps.Entries[n-1].Content = append(ps.Entries[n-1].Content, block)
			} else {
				msgID := fmt.Sprintf("codex-%d", len(ps.Entries))
				ps.Entries = append(ps.Entries, Entry{
					ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceS2, Mutability: transcript.MutabilityDelta,
					Content: []ContentBlock{block}, Timestamp: ts, Producer: &Producer{Provider: "codex"},
				})
				updateProtocolTimeBounds(&ps, ts)
				lastID = msgID
			}
		case "reasoning":
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
			block := ContentBlock{
				Type: transcript.BlockThinking, Text: strings.Join(summary, "\n"),
				Signature: stringValue(payload["encrypted_content"]), ID: stringValue(payload["id"]),
			}
			if n := len(ps.Entries); n > 0 && ps.Entries[n-1].Source == transcript.SourceS2 {
				ps.Entries[n-1].Content = append(ps.Entries[n-1].Content, block)
			} else {
				msgID := fmt.Sprintf("codex-%d", len(ps.Entries))
				ps.Entries = append(ps.Entries, Entry{
					ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceS2, Mutability: transcript.MutabilityDelta,
					Content: []ContentBlock{block}, Timestamp: ts, Producer: &Producer{Provider: "codex"},
				})
				updateProtocolTimeBounds(&ps, ts)
				lastID = msgID
			}
		case "function_call_output", "custom_tool_call_output":
			callID := stringValue(payload["call_id"])
			msgID := fmt.Sprintf("codex-%d", len(ps.Entries))
			ps.Entries = append(ps.Entries, Entry{
				ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceWorld, Mutability: transcript.MutabilityDelta,
				ToolResult: &ToolResult{ToolUseID: callID, ToolName: callNames[callID], Result: payload["output"]},
				Timestamp:  ts,
			})
			updateProtocolTimeBounds(&ps, ts)
			lastID = msgID
		case "message":
			role := stringValue(payload["role"])
			if role != "user" && role != "assistant" && role != "developer" {
				continue
			}
			blocks := codexContentBlocks(payload["content"], role)
			if len(blocks) == 0 {
				continue
			}
			joined := textContent(blocks)
			trimmed := strings.TrimSpace(joined)
			if role == "user" {
				if strings.HasPrefix(trimmed, "<environment_context>") {
					continue
				}
				msgID := firstNonEmpty(stringValue(payload["id"]), fmt.Sprintf("codex-%d", len(ps.Entries)))
				entry := Entry{
					ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceUser, Mutability: transcript.MutabilityDelta,
					Content: blocks, Timestamp: ts,
				}
				if strings.HasPrefix(trimmed, "# AGENTS.md instructions") {
					entry.Source = transcript.SourceWorld
					entry.Mutability = transcript.MutabilityConstant
					entry.Slot = "agents-md"
				} else if strings.HasPrefix(trimmed, "<subagent_notification>") {
					entry.Source = transcript.SourceWorld
				}
				ps.Entries = append(ps.Entries, entry)
				updateProtocolTimeBounds(&ps, ts)
				lastID = msgID
				continue
			}
			if role == "developer" {
				if strings.HasPrefix(trimmed, "<turn_aborted>") {
					ps.RuntimeEvents = append(ps.RuntimeEvents, RuntimeEvent{
						ID: fmt.Sprintf("codex-rt-%d", idx), AfterEntryID: lastID,
						Provider: "codex", Kind: transcript.RuntimeTurnAborted, Message: trimmed, Timestamp: ts,
					})
					continue
				}
				if isCodexCarrierDeveloper(trimmed) {
					continue
				}
				msgID := firstNonEmpty(stringValue(payload["id"]), fmt.Sprintf("codex-%d", len(ps.Entries)))
				ps.Entries = append(ps.Entries, Entry{
					ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceS1, Mutability: transcript.MutabilityDelta,
					Content: blocks, Timestamp: ts,
				})
				updateProtocolTimeBounds(&ps, ts)
				lastID = msgID
				continue
			}
			msgID := firstNonEmpty(stringValue(payload["id"]), fmt.Sprintf("codex-%d", len(ps.Entries)))
			ps.Entries = append(ps.Entries, Entry{
				ID: msgID, ParentID: strPtr(lastID), Source: transcript.SourceS2, Mutability: transcript.MutabilityDelta,
				Content: blocks, Timestamp: ts, Producer: &Producer{Provider: "codex"},
			})
			updateProtocolTimeBounds(&ps, ts)
			lastID = msgID
		}
	}
	return finishProtocolSession(ps)
}
