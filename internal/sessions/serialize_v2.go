package sessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
)

// PreparePiProtocolTranscript lifts s1 / non-delta user / world.current entries
// into Pi's systemPrompt argument, matching PreparePiTranscript for 2.0 sessions.
func PreparePiProtocolTranscript(detail ProtocolSession, appended string) (ProtocolSession, string) {
	var entries []ProtocolMessage
	var instructions []string
	for _, e := range detail.Messages {
		if e.Source == transcript.SourceS1 ||
			(e.Source == transcript.SourceUser && e.Mutability != transcript.MutabilityDelta) ||
			(e.Source == transcript.SourceWorld && e.Mutability == transcript.MutabilityCurrent) {
			if text := transcript.TextContent(e); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		entries = append(entries, e)
	}
	if appended != "" {
		instructions = append(instructions, appended)
	}
	detail.Messages = RechainProtocolMessages(entries)
	detail.MessageCount = len(entries)
	return detail, strings.Join(instructions, "\n\n")
}

func protocolMessageParentID(e ProtocolMessage) string {
	if e.ParentID == nil {
		return ""
	}
	return *e.ParentID
}

func isSameProviderThinking(detail ProtocolSession, entry ProtocolMessage, b ContentBlock) bool {
	if b.Signature == "" {
		return false
	}
	provider := detail.Provider
	if entry.Producer != nil && entry.Producer.Provider != "" {
		provider = entry.Producer.Provider
	}
	return provider != "" && provider == detail.Provider
}

func isClaudeLocalCommandProtocolMessage(e ProtocolMessage) bool {
	if e.Source != transcript.SourceUser || e.Mutability != transcript.MutabilityDelta || len(e.Content) == 0 {
		return false
	}
	for _, b := range e.Content {
		if b.Type != transcript.BlockText {
			return false
		}
	}
	return isClaudeLocalCommandText(transcript.TextContent(e))
}

func materializeProtocolMessages(detail ProtocolSession) []ProtocolMessage {
	raw := transcript.Materialize(detail)
	out := make([]ProtocolMessage, 0, len(raw))
	for _, e := range raw {
		if isClaudeLocalCommandProtocolMessage(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// RechainProtocolMessages rewrites parentId positionally across materialized entries so
// delta-appended or filtered transcripts always form a single ancestor chain.
func RechainProtocolMessages(entries []ProtocolMessage) []ProtocolMessage {
	out := make([]ProtocolMessage, len(entries))
	previous := ""
	for i, e := range entries {
		if previous == "" {
			e.ParentID = nil
		} else {
			p := previous
			e.ParentID = &p
		}
		previous = e.ID
		out[i] = e
	}
	return out
}

// SanitizeProtocolToolIDs rewrites tool ids native APIs would reject into
// deterministic API-safe ids on the 2.0 continue path.
func SanitizeProtocolToolIDs(entries []ProtocolMessage) []ProtocolMessage {
	out := make([]ProtocolMessage, len(entries))
	for i, e := range entries {
		if e.ToolResult != nil && e.ToolResult.ToolUseID != "" && !safeToolID.MatchString(e.ToolResult.ToolUseID) {
			tr := *e.ToolResult
			tr.ToolUseID = sanitizedToolID(tr.ToolUseID)
			e.ToolResult = &tr
		}
		if len(e.Content) > 0 {
			rewritten := false
			blocks := make([]ContentBlock, len(e.Content))
			for j, b := range e.Content {
				if b.Type == transcript.BlockToolUse && b.ID != "" && !safeToolID.MatchString(b.ID) {
					b.ID = sanitizedToolID(b.ID)
					rewritten = true
				}
				blocks[j] = b
			}
			if rewritten {
				e.Content = blocks
			}
		}
		out[i] = e
	}
	return out
}

// SanitizeProtocolIDs maps entry ids Codex would reject to "msg_" + 60 hex chars.
func SanitizeProtocolIDs(entries []ProtocolMessage) []ProtocolMessage {
	out := make([]ProtocolMessage, len(entries))
	for i, e := range entries {
		e.ID = sanitizedMessageID(e.ID)
		if e.ParentID != nil {
			p := sanitizedMessageID(*e.ParentID)
			e.ParentID = &p
		}
		out[i] = e
	}
	return out
}

func isHarnessVisibleChannel(e ProtocolMessage) bool {
	if e.ToolResult != nil {
		return false
	}
	switch e.Source {
	case transcript.SourceS1:
		return true
	case transcript.SourceUser:
		return e.Mutability == transcript.MutabilityConstant || e.Mutability == transcript.MutabilityCurrent
	case transcript.SourceWorld:
		return e.Mutability == transcript.MutabilityCurrent || e.Mutability == transcript.MutabilityDelta
	default:
		return false
	}
}

// SerializeClaudeProtocol converts a Protocol 2.0 Session into Claude Code's
// native JSONL records following the verified 2.0 channel and foreign-thinking
// rules (s1/world.content -> user(isMeta:true); foreign/unsigned thinking omitted).
func SerializeClaudeProtocol(detail ProtocolSession) []map[string]any {
	var records []map[string]any
	claudeEnvelope := func(recType, uuid, parentUuid, ts string) map[string]any {
		return map[string]any{
			"type": recType, "sessionId": detail.ID,
			"cwd": detail.Workspace, "timestamp": ts,
			"uuid": uuid, "parentUuid": parentUuid,
			"version": "1", "userType": "external",
			"isSidechain": false, "entrypoint": "cli",
		}
	}
	lastUUID := ""
	nextParent := func(e ProtocolMessage) string {
		if lastUUID != "" {
			return lastUUID
		}
		return protocolMessageParentID(e)
	}

	for _, e := range materializeProtocolMessages(detail) {
		if isHarnessVisibleChannel(e) {
			content := claudeSerializeUserContent(e.Content)
			if content == nil {
				continue
			}
			rec := claudeEnvelope("user", e.ID, nextParent(e), e.Timestamp)
			rec["isMeta"] = true
			rec["message"] = map[string]any{
				"role":    "user",
				"content": content,
			}
			records = append(records, rec)
			lastUUID = e.ID
			continue
		}

		if e.Source == transcript.SourceUser && e.Mutability == transcript.MutabilityDelta {
			rec := claudeEnvelope("user", e.ID, nextParent(e), e.Timestamp)
			rec["message"] = map[string]any{
				"role":    "user",
				"content": claudeSerializeUserContent(e.Content),
			}
			records = append(records, rec)
			lastUUID = e.ID
			continue
		}

		if e.Source == transcript.SourceWorld && e.ToolResult != nil {
			result := map[string]any{
				"type":        "tool_result",
				"tool_use_id": e.ToolResult.ToolUseID,
			}
			if e.ToolResult.Result != nil {
				result["content"] = claudeToolResultContent(e.ToolResult.Result)
			}
			if e.ToolResult.IsError {
				result["is_error"] = true
			}
			rec := claudeEnvelope("user", e.ID, nextParent(e), e.Timestamp)
			rec["message"] = map[string]any{
				"role":    "user",
				"content": []any{result},
			}
			records = append(records, rec)
			lastUUID = e.ID
			continue
		}

		if e.Source == transcript.SourceS2 {
			var blocks []any
			for _, b := range e.Content {
				switch b.Type {
				case transcript.BlockThinking:
					// Only same-provider signed thinking may enter Claude native thinking blocks;
					// foreign or unsigned thinking is omitted to prevent [reasoning_extraction] refusals.
					if e.Producer != nil && e.Producer.Provider == "claude" && b.Signature != "" {
						blocks = append(blocks, map[string]any{
							"type":      "thinking",
							"thinking":  b.Text,
							"signature": b.Signature,
						})
					}
				case transcript.BlockText:
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				case transcript.BlockToolUse:
					block := map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name}
					if b.Input != nil {
						block["input"] = b.Input
					}
					blocks = append(blocks, block)
				}
			}
			if len(blocks) == 0 {
				continue
			}
			msg := map[string]any{"role": "assistant", "content": blocks}
			if e.Producer != nil && e.Producer.Model != "" {
				msg["model"] = e.Producer.Model
			}
			if e.StopReason != "" {
				msg["stop_reason"] = e.StopReason
			}
			rec := claudeEnvelope("assistant", e.ID, nextParent(e), e.Timestamp)
			rec["message"] = msg
			records = append(records, rec)
			lastUUID = e.ID
		}
	}
	return records
}

// SerializeCodexProtocol converts a Protocol 2.0 Session into Codex CLI JSONL records.
func SerializeCodexProtocol(detail ProtocolSession) []map[string]any {
	meta := map[string]any{
		"id":             detail.ID,
		"session_id":     detail.ID,
		"cwd":            detail.Workspace,
		"timestamp":      detail.StartedAt,
		"originator":     "sessionman",
		"cli_version":    "sessionman",
		"source":         "cli",
		"model_provider": "openai",
	}
	if detail.IsSubagent {
		meta["source"] = map[string]any{"subagent": map[string]any{}}
	}
	records := []map[string]any{{
		"type":      "session_meta",
		"timestamp": detail.StartedAt,
		"payload":   meta,
	}}

	turnID := ""
	turnOpen := false
	openTurn := func(ts string) {
		turnID = "turn-" + codexItemID()
		turnOpen = true
		records = append(records, codexEvent(ts, map[string]any{
			"type": "turn_started", "turn_id": turnID,
		}))
	}
	closeTurn := func(ts string) {
		if !turnOpen {
			return
		}
		records = append(records, codexEvent(ts, map[string]any{
			"type": "turn_complete", "turn_id": turnID,
		}))
		turnOpen = false
	}
	emitDeveloper := func(ts, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		records = append(records, map[string]any{
			"type":      "response_item",
			"timestamp": ts,
			"payload": map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": []any{map[string]any{"type": "input_text", "text": text}},
			},
		})
	}

	for _, e := range materializeProtocolMessages(detail) {
		if isHarnessVisibleChannel(e) {
			var content []any
			for _, b := range e.Content {
				if b.Type == transcript.BlockText {
					content = append(content, map[string]any{"type": "input_text", "text": b.Text})
				}
			}
			if len(content) > 0 {
				records = append(records, map[string]any{
					"type":      "response_item",
					"timestamp": e.Timestamp,
					"payload": map[string]any{
						"type":    "message",
						"role":    "developer",
						"content": content,
					},
				})
			}
			continue
		}

		if e.Source == transcript.SourceUser && e.Mutability == transcript.MutabilityDelta {
			closeTurn(e.Timestamp)
			openTurn(e.Timestamp)
			var content []any
			for _, b := range e.Content {
				switch b.Type {
				case transcript.BlockText:
					content = append(content, map[string]any{"type": "input_text", "text": b.Text})
				case transcript.BlockImage:
					if url := codexImageURL(b.Source); url != "" {
						content = append(content, map[string]any{"type": "input_image", "image_url": url})
					}
				}
			}
			records = append(records, map[string]any{
				"type":      "response_item",
				"timestamp": e.Timestamp,
				"payload": map[string]any{
					"type":    "message",
					"role":    "user",
					"content": content,
				},
			})
			records = append(records, codexEvent(e.Timestamp, map[string]any{
				"type": "user_message", "message": transcript.TextContent(e),
			}))
			continue
		}

		if e.Source == transcript.SourceWorld && e.ToolResult != nil {
			outputType := "function_call_output"
			if codexCustomTools[e.ToolResult.ToolName] {
				outputType = "custom_tool_call_output"
			}
			records = append(records, map[string]any{
				"type":      "response_item",
				"timestamp": e.Timestamp,
				"payload": map[string]any{
					"type":    outputType,
					"call_id": e.ToolResult.ToolUseID,
					"output":  codexOutput(e.ToolResult.Result),
				},
			})
			continue
		}

		if e.Source == transcript.SourceS2 {
			var textBlocks []any
			var agentText string
			flushText := func() {
				if len(textBlocks) == 0 {
					return
				}
				records = append(records, map[string]any{
					"type":      "response_item",
					"timestamp": e.Timestamp,
					"payload": map[string]any{
						"type":    "message",
						"role":    "assistant",
						"id":      e.ID,
						"content": textBlocks,
					},
				})
				records = append(records, codexEvent(e.Timestamp, map[string]any{
					"type": "agent_message", "message": agentText,
				}))
				textBlocks, agentText = nil, ""
			}
			for _, b := range e.Content {
				switch b.Type {
				case transcript.BlockText:
					textBlocks = append(textBlocks, map[string]any{"type": "output_text", "text": b.Text})
					if agentText != "" {
						agentText += "\n"
					}
					agentText += b.Text
				case transcript.BlockThinking:
					flushText()
					if b.Text != "" {
						emitDeveloper(e.Timestamp, b.Text)
					}
				case transcript.BlockToolUse:
					flushText()
					payload := map[string]any{"call_id": b.ID, "name": b.Name}
					if codexCustomTools[b.Name] {
						payload["type"] = "custom_tool_call"
						payload["input"] = toolInputString(b.Input)
					} else {
						payload["type"] = "function_call"
						payload["arguments"] = toolInputString(b.Input)
					}
					records = append(records, map[string]any{"type": "response_item", "timestamp": e.Timestamp, "payload": payload})
				}
			}
			flushText()
		}
	}
	closeTurn(detail.UpdatedAt)
	return records
}

// SerializePiProtocol converts a Protocol 2.0 Session into Pi JSONL records.
func SerializePiProtocol(detail ProtocolSession) []map[string]any {
	records := []map[string]any{{
		"type":      "session",
		"version":   3,
		"id":        detail.ID,
		"cwd":       detail.Workspace,
		"timestamp": detail.StartedAt,
	}}
	lastID := ""
	envelope := func(id string, e ProtocolMessage, msg map[string]any) map[string]any {
		var parent any
		if lastID != "" {
			parent = lastID
		} else if p := protocolMessageParentID(e); p != "" {
			parent = p
		}
		if parsed, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
			msg["timestamp"] = parsed.UnixMilli()
		}
		recTS := e.Timestamp
		if e.RecordedAt != "" {
			recTS = e.RecordedAt
		}
		lastID = id
		return map[string]any{
			"type": "message", "id": id, "parentId": parent,
			"timestamp": recTS, "message": msg,
		}
	}

	for _, e := range materializeProtocolMessages(detail) {
		if isHarnessVisibleChannel(e) {
			text := transcript.TextContent(e)
			if strings.TrimSpace(text) != "" {
				records = append(records, envelope(e.ID, e, map[string]any{"role": "system", "content": text}))
			}
			continue
		}

		if e.Source == transcript.SourceUser && e.Mutability == transcript.MutabilityDelta {
			var content []any
			for _, b := range e.Content {
				if b.Type == transcript.BlockText {
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				}
			}
			records = append(records, envelope(e.ID, e, map[string]any{"role": "user", "content": content}))
			continue
		}

		if e.Source == transcript.SourceWorld && e.ToolResult != nil {
			var content any = []any{}
			if isPiBlockArray(e.ToolResult.Result) {
				content = e.ToolResult.Result
			} else if text := resultText(e.ToolResult.Result); text != "" {
				content = []any{map[string]any{"type": "text", "text": text}}
			}
			records = append(records, envelope(e.ID, e, map[string]any{
				"role":       "toolResult",
				"toolCallId": e.ToolResult.ToolUseID,
				"toolName":   e.ToolResult.ToolName,
				"content":    content,
				"isError":    e.ToolResult.IsError,
			}))
			continue
		}

		if e.Source == transcript.SourceS2 {
			var content []any
			thoughtIdx := 0
			for _, b := range e.Content {
				switch b.Type {
				case transcript.BlockThinking:
					if isSameProviderThinking(detail, e, b) && e.Producer != nil && e.Producer.Provider == "pi" {
						content = append(content, map[string]any{
							"type":              "thinking",
							"thinking":          b.Text,
							"thinkingSignature": b.Signature,
						})
					} else if b.Text != "" {
						thoughtIdx++
						records = append(records, envelope(fmt.Sprintf("%s-thought-%d", e.ID, thoughtIdx), e, map[string]any{
							"role": "system", "content": b.Text,
						}))
					}
				case transcript.BlockText:
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				case transcript.BlockToolUse:
					args := b.Input
					if args == nil {
						args = map[string]any{}
					}
					content = append(content, map[string]any{
						"type": "toolCall", "id": b.ID, "name": b.Name, "arguments": args,
					})
				}
			}
			if len(content) == 0 {
				continue
			}
			msg := map[string]any{
				"role":    "assistant",
				"content": content,
				"usage": map[string]any{
					"input": 0, "output": 0,
					"cacheRead": 0, "cacheWrite": 0,
					"reasoning": 0, "totalTokens": 0,
					"cost": map[string]any{
						"input": 0, "output": 0,
						"cacheRead": 0, "cacheWrite": 0,
						"total": 0,
					},
				},
			}
			if e.StopReason != "" {
				msg["stopReason"] = e.StopReason
			}
			if e.Producer != nil && e.Producer.Model != "" {
				msg["model"] = e.Producer.Model
			}
			records = append(records, envelope(e.ID, e, msg))
		}
	}
	return records
}

// SerializeAgyProtocol converts a Protocol 2.0 Session into AGY Step protobuf payloads.
func SerializeAgyProtocol(detail ProtocolSession) ([][]byte, error) {
	var steps [][]byte
	for _, e := range materializeProtocolMessages(detail) {
		stamp := agyTimestamp(e.Timestamp)
		meta := agyJoin(agyBytes(1, stamp), agyBytes(6, stamp), agyBytes(7, stamp), agyBytes(8, stamp), agyBytes(32, stamp))
		if isHarnessVisibleChannel(e) {
			text := transcript.TextContent(e)
			if strings.TrimSpace(text) != "" {
				steps = append(steps, agyJoin(agyInt(1, 101), agyInt(4, 3), agyBytes(5, meta), agyBytes(114, agyText(1, text))))
			}
			continue
		}
		if e.Source == transcript.SourceUser && e.Mutability == transcript.MutabilityDelta {
			body := agyText(2, transcript.TextContent(e))
			for _, b := range e.Content {
				if b.Type != transcript.BlockImage {
					continue
				}
				source, ok := b.Source.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("agy image source must be an object")
				}
				data, err := base64.StdEncoding.DecodeString(stringValue(source["data"]))
				if err != nil {
					return nil, err
				}
				mime := stringValue(source["media_type"])
				if len(data) == 0 || mime == "" {
					return nil, fmt.Errorf("agy image requires inline base64 data and media_type")
				}
				body = append(body, agyBytes(9, agyJoin(agyText(1, mime), agyBytes(2, data)))...)
			}
			steps = append(steps, agyJoin(agyInt(1, 14), agyInt(4, 3), agyBytes(5, meta), agyBytes(19, body)))
			continue
		}
		if e.Source == transcript.SourceWorld && e.ToolResult != nil {
			body := agyBytes(2, agyText(1, resultText(e.ToolResult.Result)))
			call := agyJoin(agyText(1, e.ToolResult.ToolUseID), agyText(2, e.ToolResult.ToolName), agyText(3, "{}"))
			meta = append(meta, agyBytes(4, call)...)
			status := uint64(3)
			var errorDetails []byte
			if e.ToolResult.IsError {
				status = 7
				errorDetails = agyBytes(31, agyText(1, resultText(e.ToolResult.Result)))
			}
			steps = append(steps, agyJoin(agyInt(1, 132), agyInt(4, status), agyBytes(5, meta), errorDetails, agyBytes(140, body)))
			continue
		}
		if e.Source == transcript.SourceS2 {
			var pending []byte
			pendingHasText, pendingHasCalls := false, false
			flush := func() {
				if len(pending) > 0 {
					steps = append(steps, agyJoin(agyInt(1, 15), agyInt(4, 3), agyBytes(5, meta), agyBytes(20, pending)))
					pending = nil
					pendingHasText, pendingHasCalls = false, false
				}
			}
			for _, b := range e.Content {
				switch b.Type {
				case transcript.BlockText:
					if pendingHasText || pendingHasCalls {
						flush()
					}
					pending = append(pending, agyText(1, b.Text)...)
					pendingHasText = true
				case transcript.BlockThinking:
					flush()
					if b.Text != "" {
						steps = append(steps, agyJoin(agyInt(1, 101), agyInt(4, 3), agyBytes(5, meta), agyBytes(114, agyText(1, b.Text))))
					}
				case transcript.BlockToolUse:
					args, err := json.Marshal(b.Input)
					if err != nil {
						return nil, err
					}
					pending = append(pending, agyBytes(7, agyJoin(agyText(1, b.ID), agyText(2, b.Name), agyText(3, string(args))))...)
					pendingHasCalls = true
				}
			}
			flush()
		}
	}
	return steps, nil
}

// WriteProtocolSessionFile materializes a Protocol 2.0 session into the target agent's native store.
func WriteProtocolSessionFile(detail ProtocolSession, agentID, dir, homeDir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create resume directory: %w", err)
	}
	switch agentID {
	case "claude":
		newID, path := claudeSessionPath(detail.Workspace, homeDir)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("create claude session directory: %w", err)
		}
		detail.ID = newID
		return newID, writeJSONLFile(path, SerializeClaudeProtocol(detail))
	case "pi":
		newID, workspace, path, err := piSessionPath(detail.Workspace, homeDir)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("create pi session directory: %w", err)
		}
		detail.ID = newID
		detail.Workspace = workspace
		return path, writeJSONLFile(path, SerializePiProtocol(detail))
	case "opencode":
		// OpenCode remains on the v1 adapter until its SQLite/file serializer migrates.
		return "", fmt.Errorf("opencode protocol 2.0 write not yet enabled")
	default:
		return "", fmt.Errorf("unsupported resume target %q", agentID)
	}
}

// WriteProtocolCodexSessionFile writes a Protocol 2.0 session into Codex's rollout store.
func WriteProtocolCodexSessionFile(detail ProtocolSession, codexHome, provider string) (string, error) {
	if strings.TrimSpace(provider) == "" {
		return "", fmt.Errorf("Codex model provider is required")
	}
	id, path := codexSessionPath(codexHome, time.Now())
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create codex session directory: %w", err)
	}
	detail.ID = id
	records := SerializeCodexProtocol(detail)
	if len(records) > 0 {
		if payload, ok := records[0]["payload"].(map[string]any); ok {
			payload["model_provider"] = provider
		}
	}
	return id, writeJSONLFile(path, records)
}

// WriteProtocolAgyDatabaseFile writes a Protocol 2.0 session as a standalone AGY SQLite database.
func WriteProtocolAgyDatabaseFile(detail ProtocolSession, path string) error {
	steps, err := SerializeAgyProtocol(detail)
	if err != nil {
		return err
	}
	return writeAgyStepsDatabase(detail.ID, detail.Workspace, detail.StartedAt, steps, path)
}

// WriteProtocolAgySessionFile writes a Protocol 2.0 session into AGY's SQLite store.
func WriteProtocolAgySessionFile(detail ProtocolSession, appDataDir string) (string, error) {
	id, path := agySessionPath(appDataDir)
	detail.ID = id
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	steps, err := SerializeAgyProtocol(detail)
	if err != nil {
		return "", err
	}
	if err := writeAgyStepsDatabase(detail.ID, detail.Workspace, detail.StartedAt, steps, path); err != nil {
		return "", err
	}
	if err := upsertAgySummary(appDataDir, id, detail.Workspace, detail.Title, "", steps); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}
