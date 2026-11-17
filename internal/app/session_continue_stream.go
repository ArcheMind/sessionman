package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ArcheMind/agentx/internal/nativedirs"
	"github.com/ArcheMind/agentx/internal/sessions"
)

// Native stdout wakes the store reader; the timer covers stores flushed after
// their corresponding stdout event, including while a native tool is running.
type continueTranscriptStream struct {
	mu                             sync.Mutex
	app                            App
	agent, model, workspace, token string
	prefix                         int
	cancel                         func()
	done, stopped                  chan struct{}
	err                            error
	native                         []byte
	sessionID, path                string
	info                           os.FileInfo
	offset                         int64
	pending                        []byte
	records                        []map[string]any
	emitted                        []json.RawMessage
}

func newContinueTranscriptStream(a App, agent, model, workspace, token string, prefix int, cancel func()) *continueTranscriptStream {
	s := &continueTranscriptStream{app: a, agent: agent, model: model, workspace: workspace, token: token, prefix: prefix, cancel: cancel, done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(s.stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				s.mu.Lock()
				if s.err == nil {
					s.fail(s.readStore())
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *continueTranscriptStream) fail(err error) {
	if err != nil && s.err == nil {
		s.err = err
		s.cancel()
	}
}

func (s *continueTranscriptStream) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.native = append(s.native, data...)
	for {
		index := bytes.IndexByte(s.native, '\n')
		if index < 0 {
			break
		}
		s.fail(s.observe(s.native[:index]))
		s.native = s.native[index+1:]
		if s.err != nil {
			return 0, s.err
		}
	}
	s.fail(s.readStore())
	if s.err != nil {
		return 0, s.err
	}
	return len(data), nil
}

func (s *continueTranscriptStream) observe(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	run, err := parseContinueStream(s.agent, s.model, expectedSession(s.agent, s.token), string(line))
	if err != nil {
		return err
	}
	if run.sessionID != "" {
		if s.sessionID != "" && s.sessionID != run.sessionID {
			return fmt.Errorf("%s changed session identity while streaming", s.agent)
		}
		s.sessionID = run.sessionID
	}
	return nil
}

func (s *continueTranscriptStream) resolvePath() (string, error) {
	token := s.token
	if s.agent != "pi" {
		if s.sessionID != "" {
			token = s.sessionID
		}
		if token == "" {
			return "", nil
		}
	}
	if s.agent == "codex" {
		// Rollout names contain the thread UUID. Avoid cataloging every session
		// while waiting for a newly started thread's file to become visible.
		codexHome := nativedirs.CodexHome()
		paths, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "*"+token+"*.jsonl"))
		if err != nil || len(paths) == 0 {
			return "", err
		}
		if len(paths) != 1 {
			return "", fmt.Errorf("multiple Codex rollouts for streaming thread %s", token)
		}
		return paths[0], nil
	}
	return s.app.continueSessionFile(s.agent, s.workspace, token)
}

func (s *continueTranscriptStream) readStore() error {
	if s.path == "" {
		path, err := s.resolvePath()
		if err != nil || path == "" {
			return err
		}
		s.path = path
	}
	file, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open streaming %s store: %w", s.agent, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if s.info != nil && (!os.SameFile(s.info, info) || info.Size() < s.offset) {
		s.offset, s.pending, s.records = 0, nil, nil
	}
	s.info = info
	if _, err := file.Seek(s.offset, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	s.offset += int64(len(data))
	s.pending = append(s.pending, data...)
	changed := false
	for {
		index := bytes.IndexByte(s.pending, '\n')
		if index < 0 {
			break
		}
		var record map[string]any
		// Match the canonical parser's treatment of invalid native lines, but
		// retain an unfinished last line until its remaining bytes are written.
		if json.Unmarshal(s.pending[:index], &record) == nil {
			s.records = append(s.records, record)
			changed = true
		}
		s.pending = s.pending[index+1:]
	}
	if !changed {
		return nil
	}
	full, ok := sessions.ParseRecords(s.agent, s.records)
	if !ok || len(full.Messages) < s.prefix {
		return nil
	}
	if s.agent != "pi" && s.sessionID != "" && full.ID != s.sessionID {
		return fmt.Errorf("%s streaming store identity %s differs from %s", s.agent, full.ID, s.sessionID)
	}
	delta := continueDelta(full.Messages, s.prefix)
	// The Codex converter can still append reasoning/tool blocks to the
	// trailing assistant. A following message closes that canonical record.
	if s.agent == "codex" && len(delta) > 0 && delta[len(delta)-1].Role == "assistant" {
		delta = delta[:len(delta)-1]
	}
	return s.emit(namespaceDelta(delta, continueRunID(s.agent, s.token, full.ID)))
}

func (s *continueTranscriptStream) emit(messages []sessions.Message) error {
	if len(messages) < len(s.emitted) {
		return fmt.Errorf("%s streaming transcript lost previously emitted messages", s.agent)
	}
	for i, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if i < len(s.emitted) {
			if !bytes.Equal(encoded, s.emitted[i]) {
				return fmt.Errorf("%s streaming message %s changed after emission", s.agent, message.ID)
			}
			continue
		}
		if err := json.NewEncoder(s.app.Stdout).Encode(struct {
			Type    string           `json:"type"`
			Message sessions.Message `json:"message"`
		}{"message", message}); err != nil {
			return err
		}
		s.emitted = append(s.emitted, encoded)
	}
	return nil
}

func (s *continueTranscriptStream) stop() error {
	close(s.done)
	<-s.stopped
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && strings.TrimSpace(string(s.native)) != "" {
		s.fail(s.observe(s.native))
	}
	return s.err
}

func (s *continueTranscriptStream) finish(out sessions.Session) error {
	if err := s.emit(out.Messages); err != nil {
		return err
	}
	return json.NewEncoder(s.app.Stdout).Encode(struct {
		Type    string           `json:"type"`
		Session sessions.Session `json:"session"`
	}{"result", out})
}
