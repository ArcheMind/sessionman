package sessions

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func discoverAgyIn(root string, includeSubagents bool) ([]Summary, error) {
	paths, err := filepath.Glob(filepath.Join(root, "conversations", "*.db"))
	if err != nil {
		return nil, err
	}
	var items []Summary
	var failures []error
	for _, path := range paths {
		item, err := agySummaryFile(path)
		if err != nil {
			failures = append(failures, fmt.Errorf("read agy session %s: %w", path, err))
			continue
		}
		if includeSubagents || !item.IsSubagent {
			items = append(items, item)
		}
	}
	titles, err := agyTitles(filepath.Join(root, "conversation_summaries.db"), "")
	if err != nil {
		return items, errors.Join(append(failures, err)...)
	}
	for i := range items {
		if title := titles[items[i].ID]; title != "" {
			items[i].Title = title
		}
	}
	return items, errors.Join(failures...)
}

func readAgySession(path string) (Session, error) {
	detail, err := ParseAgySessionFile(path)
	if err != nil {
		return detail, err
	}
	titles, err := agyTitles(filepath.Join(filepath.Dir(filepath.Dir(path)), "conversation_summaries.db"), detail.ID)
	if err != nil {
		return detail, err
	}
	if title := titles[detail.ID]; title != "" {
		detail.Title = title
	}
	return detail, nil
}

func agyTitles(path, id string) (map[string]string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := agyOpen(path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `SELECT conversation_id,title FROM conversation_summaries`
	var args []any
	if id != "" {
		query += ` WHERE conversation_id=?`
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("read agy summaries %s: %w", path, err)
	}
	defer rows.Close()
	titles := make(map[string]string)
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		titles[id] = title
	}
	return titles, rows.Err()
}

func summarizeAgy(item Summary, _ bool) (Summary, error) {
	item.UserPrompts = nil
	if !item.collectUserPrompts {
		return item, nil
	}
	db, err := agyOpen(item.Source, true)
	if err != nil {
		return item, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT step_payload,step_format FROM steps WHERE step_type=14 ORDER BY idx`)
	if err != nil {
		return item, fmt.Errorf("read agy user prompts %s: %w", item.Source, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var format int
		if err := rows.Scan(&raw, &format); err != nil {
			return item, err
		}
		if format != 0 {
			return item, fmt.Errorf("unsupported agy step format %d", format)
		}
		step, err := agyDecode(raw)
		if err != nil {
			return item, fmt.Errorf("decode agy user prompt: %w", err)
		}
		user, err := agyDecode(step.bytes(19))
		if err != nil {
			return item, fmt.Errorf("decode agy user prompt payload: %w", err)
		}
		if text := user.text(2); text != "" {
			item.UserPrompts = append(item.UserPrompts, strings.ToLower(text))
		}
	}
	return item, rows.Err()
}

func agySummaryFile(path string) (Summary, error) {
	item := Summary{Agent: "agy", Source: path}
	db, err := agyOpen(path, true)
	if err != nil {
		return item, err
	}
	defer db.Close()
	item, err = agyHeader(db, path)
	if err != nil {
		return item, err
	}
	if err := agyTimeline(db, &item); err != nil {
		return item, err
	}
	return item, nil
}

func agyHeader(db *sql.DB, path string) (Summary, error) {
	item := Summary{Agent: "agy", Source: path}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return item, err
	}
	if version != 1 {
		return item, fmt.Errorf("unsupported agy SQLite user_version %d", version)
	}
	if err := db.QueryRow(`SELECT cascade_id FROM trajectory_meta LIMIT 1`).Scan(&item.ID); err != nil {
		return item, fmt.Errorf("read agy trajectory identity: %w", err)
	}
	var raw []byte
	err := db.QueryRow(`SELECT data FROM trajectory_metadata_blob WHERE id='main'`).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return item, err
	}
	if len(raw) > 0 {
		meta, err := agyDecode(raw)
		if err != nil {
			return item, err
		}
		item.Workspace = agyWorkspace(meta.text(7))
		item.StartedAt, err = agyTime(meta.bytes(2))
		if err != nil {
			return item, err
		}
		item.IsSubagent = meta.text(5) != "" || meta.integer(17) > 0
	}
	return item, nil
}

func agyTimeline(db *sql.DB, item *Summary) error {
	rows, err := db.Query(`SELECT idx,step_payload,step_format FROM steps ORDER BY idx`)
	if err != nil {
		return err
	}
	defer rows.Close()
	item.MessageCount, item.TurnCount = 0, 0
	item.FirstUserMessage, item.LatestUserMessage = "", ""
	item.UserPrompts = nil
	for rows.Next() {
		var idx, format int
		var raw []byte
		if err := rows.Scan(&idx, &raw, &format); err != nil {
			return err
		}
		if format != 0 {
			return fmt.Errorf("unsupported agy step format %d", format)
		}
		p, err := agyDecode(raw)
		if err != nil {
			return fmt.Errorf("agy step %d: %w", idx, err)
		}
		meta, err := agyDecode(p.bytes(5))
		if err != nil {
			return fmt.Errorf("agy step %d metadata: %w", idx, err)
		}
		kind := p.integer(1)
		visible := kind == 14 || kind == 15 || len(meta.bytes(4)) > 0
		if !visible && len(p.bytes(114)) > 0 {
			system, err := agyDecode(p.bytes(114))
			if err != nil {
				return fmt.Errorf("agy step %d: %w", idx, err)
			}
			visible = system.text(1) != ""
		}
		if !visible {
			continue
		}
		if err := agySummaryTimestamp(item, meta); err != nil {
			return fmt.Errorf("agy step %d: %w", idx, err)
		}
		item.MessageCount++
		if kind == 14 {
			user, err := agyDecode(p.bytes(19))
			if err != nil {
				return fmt.Errorf("agy step %d user: %w", idx, err)
			}
			text := user.text(2)
			item.addUserMessage(text)
			if item.Title == "" && isSessionPreview(text) {
				item.Title = oneLine(text, 96)
			}
		}
	}
	return rows.Err()
}

func agySummaryTimestamp(item *Summary, meta agyProto) error {
	timestamp, err := agyTime(meta.bytes(1))
	if err != nil {
		return err
	}
	if item.StartedAt == "" || timestamp != "" && timestamp < item.StartedAt {
		item.StartedAt = timestamp
	}
	if timestamp > item.UpdatedAt {
		item.UpdatedAt = timestamp
	}
	return nil
}
