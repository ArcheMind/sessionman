package sessions

import (
	"encoding/json"
	"fmt"
)

func countSessionTurns(item Summary) (int, error) {
	count := 0
	switch item.Agent {
	case "agy":
		db, err := agyOpen(item.Source, true)
		if err != nil {
			return 0, err
		}
		defer db.Close()
		rows, err := db.Query(`SELECT step_payload,step_format FROM steps ORDER BY idx`)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var format int
			if err := rows.Scan(&raw, &format); err != nil {
				return 0, err
			}
			if format != 0 {
				return 0, fmt.Errorf("unsupported agy step format %d", format)
			}
			p, err := agyDecode(raw)
			if err != nil {
				return 0, err
			}
			if p.integer(1) != 14 {
				continue
			}
			user, err := agyDecode(p.bytes(19))
			if err != nil {
				return 0, err
			}
			if isSessionTitle(user.text(2)) {
				count++
			}
		}
		return count, rows.Err()
	case "opencode":
		_, messages, _, err := openCodeIndex(item.Source)
		if err != nil {
			return 0, err
		}
		for _, message := range messages {
			if message.role != "user" {
				continue
			}
			_, _, text, err := openCodePartSummary(item, message)
			if err != nil {
				return 0, err
			}
			if isSessionTitle(text) {
				count++
			}
		}
		return count, nil
	case "claude", "codex", "pi":
		decode := jsonlRecordDecoder(item.Agent)
		err := scanJSONLReverse(item.Source, func(line []byte) bool {
			var envelope struct {
				Type    string
				Payload struct{ Type, Role string }
				Message struct{ Role string }
			}
			if json.Unmarshal(line, &envelope) != nil {
				return false
			}
			user := item.Agent == "claude" && envelope.Type == "user" ||
				item.Agent == "codex" && envelope.Type == "response_item" && envelope.Payload.Type == "message" && envelope.Payload.Role == "user" ||
				item.Agent == "pi" && envelope.Type == "message" && envelope.Message.Role == "user"
			if !user {
				return false
			}
			var record map[string]any
			if json.Unmarshal(line, &record) != nil {
				return false
			}
			for _, message := range decode(record).Messages {
				if message.Role == "user" && isSessionTitle(TextContent(message.Content)) {
					count++
				}
			}
			return false
		})
		return count, err
	default:
		return item.TurnCount, nil
	}
}
