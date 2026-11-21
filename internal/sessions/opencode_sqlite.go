package sessions

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func openCodeSQLiteURL(path, id string) string {
	u := openCodeFileURL(path)
	u.Fragment = id
	return u.String()
}

func openCodeFileURL(path string) url.URL {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return url.URL{Scheme: "file", Path: path}
}

func openCodeSQLiteSource(source string) (string, string, bool) {
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "file" || u.Fragment == "" || !strings.HasSuffix(u.Path, ".db") {
		return "", "", false
	}
	path := u.Path
	if len(path) > 2 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), u.Fragment, true
}

func openCodeDatabase(path string) (*sql.DB, error) {
	u := openCodeFileURL(path)
	u.RawQuery = "mode=ro"
	return sql.Open("sqlite", u.String())
}

func discoverOpenCodeSQLite(path string, includeSubagents bool) ([]Summary, error) {
	db, err := openCodeDatabase(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, COALESCE(parent_id,''), directory, title, time_created, time_updated FROM session ORDER BY time_updated DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("read opencode sessions %s: %w", path, err)
	}
	defer rows.Close()
	var items []Summary
	for rows.Next() {
		var item Summary
		var parent string
		var created, updated int64
		if err := rows.Scan(&item.ID, &parent, &item.Workspace, &item.Title, &created, &updated); err != nil {
			return items, err
		}
		item.Agent, item.Source, item.IsSubagent = "opencode", openCodeSQLiteURL(path, item.ID), parent != ""
		item.StartedAt, item.UpdatedAt = openCodeTime(created), openCodeTime(updated)
		if includeSubagents || !item.IsSubagent {
			items = append(items, item)
		}
	}
	return items, rows.Err()
}

func openCodeSQLiteIndex(path, id string) (Summary, []openCodeMessage, bool, error) {
	item := Summary{Agent: "opencode", Source: openCodeSQLiteURL(path, id)}
	db, err := openCodeDatabase(path)
	if err != nil {
		return item, nil, false, err
	}
	defer db.Close()
	var parent string
	var created, updated int64
	err = db.QueryRow(`SELECT id, COALESCE(parent_id,''), directory, title, time_created, time_updated FROM session WHERE id=?`, id).Scan(&item.ID, &parent, &item.Workspace, &item.Title, &created, &updated)
	if err != nil {
		return item, nil, false, err
	}
	item.IsSubagent = parent != ""
	item.StartedAt, item.UpdatedAt = openCodeTime(created), openCodeTime(updated)
	rows, err := db.Query(`SELECT id, time_created, data FROM message WHERE session_id=? ORDER BY time_created, id`, id)
	if err != nil {
		return item, nil, false, err
	}
	defer rows.Close()
	var messages []openCodeMessage
	for rows.Next() {
		var message openCodeMessage
		var created int64
		var data []byte
		if err := rows.Scan(&message.id, &created, &data); err != nil {
			return item, nil, false, err
		}
		var info struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(data, &info); err != nil {
			return item, nil, false, err
		}
		message.role, message.timestamp = info.Role, openCodeTime(created)
		messages = append(messages, message)
	}
	return item, messages, true, rows.Err()
}

func openCodeTime(milliseconds int64) string {
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339Nano)
}

func openCodeParts(item Summary, message openCodeMessage) ([][]byte, error) {
	var parts [][]byte
	if path, id, ok := openCodeSQLiteSource(item.Source); ok {
		db, err := openCodeDatabase(path)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		rows, err := db.Query(`SELECT data FROM part WHERE session_id=? AND message_id=? ORDER BY time_created, id`, id, message.id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				return nil, err
			}
			parts = append(parts, data)
		}
		return parts, rows.Err()
	}
	paths, err := openCodePartPaths(item, message)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		parts = append(parts, data)
	}
	return parts, nil
}
