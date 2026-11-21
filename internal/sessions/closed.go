package sessions

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ClosedSession struct {
	Agent    string    `json:"agent"`
	ID       string    `json:"session_id"`
	ClosedAt time.Time `json:"closed_at"`
}

func (s Service) closedPath() string {
	return filepath.Join(s.HomeDir, ".sessionman", "closed-sessions.db")
}

func (s Service) closedDatabase(readOnly bool) (*sql.DB, error) {
	path := filepath.ToSlash(s.closedPath())
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(s.closedPath()), 0700); err != nil {
			return nil, err
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	params := url.Values{"_pragma": {"busy_timeout(5000)"}}
	if readOnly {
		params.Set("mode", "ro")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}
	return sql.Open("sqlite", u.String())
}

func (s Service) SaveClosed(agent, id string, closedAt time.Time) error {
	if agentSpecByID(agent) == nil || id == "" || closedAt.IsZero() {
		return fmt.Errorf("closed session requires a supported agent, session ID and timestamp")
	}
	db, err := s.closedDatabase(false)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS closed_sessions (agent TEXT NOT NULL, session_id TEXT NOT NULL, closed_at INTEGER NOT NULL, PRIMARY KEY (agent, session_id))`); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO closed_sessions (agent, session_id, closed_at) VALUES (?, ?, ?) ON CONFLICT (agent, session_id) DO UPDATE SET closed_at=MAX(closed_sessions.closed_at, excluded.closed_at)`, agent, id, closedAt.UnixNano())
	return err
}

func (s Service) LatestClosed() (ClosedSession, bool, error) {
	_, err := os.Stat(s.closedPath())
	if os.IsNotExist(err) {
		return ClosedSession{}, false, nil
	}
	if err != nil {
		return ClosedSession{}, false, err
	}
	db, err := s.closedDatabase(true)
	if err != nil {
		return ClosedSession{}, false, err
	}
	defer db.Close()
	var latest ClosedSession
	var nanoseconds int64
	err = db.QueryRow(`SELECT agent, session_id, closed_at FROM closed_sessions ORDER BY closed_at DESC, agent, session_id LIMIT 1`).Scan(&latest.Agent, &latest.ID, &nanoseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return ClosedSession{}, false, nil
	}
	latest.ClosedAt = time.Unix(0, nanoseconds).UTC()
	return latest, err == nil, err
}
