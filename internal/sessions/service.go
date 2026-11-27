package sessions

import (
	"errors"
	"fmt"
	"github.com/ArcheMind/sessionman/internal/clibin"
	"os"
	"strings"

	transcript "github.com/ArcheMind/sessionman-protocol/go"
	"github.com/ArcheMind/sessionman/internal/nativedirs"
)

type Agent struct {
	ID        string `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Installed bool   `json:"installed" yaml:"installed"`
	Root      string `json:"session_root" yaml:"session_root"`
}

// Transcript shapes are owned by the shared protocol module; sessionman defines
// no message shapes of its own. ProtocolSession/Entry/RuntimeEvent are the 2.0
// canonical shapes; Message/Session remain temporarily as v1 transition views
// until all native parsers and callers finish migrating to 2.0.
type (
	ContentBlock    = transcript.ContentBlock
	ProtocolSession = transcript.Session
	ProtocolMessage           = transcript.Message
	RuntimeEvent    = transcript.RuntimeEvent
	ToolResult      = transcript.ToolResult
	Producer        = transcript.Producer
)

// Message is the legacy v1 message shape retained during the 2.0 migration.
type Message struct {
	ID         string         `json:"id" yaml:"id"`
	ParentID   string         `json:"parentId,omitempty" yaml:"parentId,omitempty"`
	Role       string         `json:"role" yaml:"role"`
	Content    []ContentBlock `json:"content,omitempty" yaml:"content,omitempty"`
	Result     any            `json:"result,omitempty" yaml:"result,omitempty"`
	Timestamp  string         `json:"timestamp,omitempty" yaml:"timestamp,omitempty"`
	RecordedAt string         `json:"recordedAt,omitempty" yaml:"recordedAt,omitempty"`
	Model      string         `json:"model,omitempty" yaml:"model,omitempty"`
	StopReason string         `json:"stopReason,omitempty" yaml:"stopReason,omitempty"`
	ToolUseID  string         `json:"toolUseId,omitempty" yaml:"toolUseId,omitempty"`
	ToolName   string         `json:"toolName,omitempty" yaml:"toolName,omitempty"`
	IsError    bool           `json:"isError,omitempty" yaml:"isError,omitempty"`
}

// Session is the legacy v1 session envelope retained during the 2.0 migration.
type Session struct {
	ID           string    `json:"id" yaml:"id"`
	Provider     string    `json:"provider" yaml:"provider"`
	IsSubagent   bool      `json:"isSubagent,omitempty" yaml:"isSubagent,omitempty"`
	Workspace    string    `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Title        string    `json:"title,omitempty" yaml:"title,omitempty"`
	StartedAt    string    `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	UpdatedAt    string    `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	MessageCount int       `json:"messageCount" yaml:"messageCount"`
	Source       string    `json:"source,omitempty" yaml:"source,omitempty"`
	Messages     []Message `json:"messages" yaml:"messages"`
}

// ToProtocolSession converts a legacy v1 Session into a validated Protocol 2.0
// Session via transcript.UpgradeFromV1.
func ToProtocolSession(v1 Session) (ProtocolSession, error) {
	msgs := make([]transcript.V1Message, len(v1.Messages))
	for i, m := range v1.Messages {
		var parent *string
		if m.ParentID != "" {
			p := m.ParentID
			parent = &p
		}
		msgs[i] = transcript.V1Message{
			ID:         m.ID,
			ParentID:   parent,
			Role:       m.Role,
			Content:    m.Content,
			Result:     m.Result,
			Timestamp:  m.Timestamp,
			RecordedAt: m.RecordedAt,
			Model:      m.Model,
			StopReason: m.StopReason,
			ToolUseID:  m.ToolUseID,
			ToolName:   m.ToolName,
			IsError:    m.IsError,
		}
	}
	return transcript.UpgradeFromV1(transcript.V1Session{
		ID:           v1.ID,
		Provider:     v1.Provider,
		IsSubagent:   v1.IsSubagent,
		Workspace:    v1.Workspace,
		Title:        v1.Title,
		StartedAt:    v1.StartedAt,
		UpdatedAt:    v1.UpdatedAt,
		MessageCount: v1.MessageCount,
		Source:       v1.Source,
		Messages:     msgs,
	})
}

const SubagentMarker = "[sm:subagent]"

func hasSubagentMarker(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSuffix(line, "\r") == SubagentMarker
}

type Summary struct {
	TurnCount          int      `json:"-" yaml:"-"`
	ID                 string   `json:"id" yaml:"id"`
	Agent              string   `json:"agent" yaml:"agent"`
	IsSubagent         bool     `json:"isSubagent,omitempty" yaml:"isSubagent,omitempty"`
	Workspace          string   `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Title              string   `json:"title,omitempty" yaml:"title,omitempty"`
	FirstUserMessage   string   `json:"firstUserMessage,omitempty" yaml:"firstUserMessage,omitempty"`
	LatestUserMessage  string   `json:"latestUserMessage,omitempty" yaml:"latestUserMessage,omitempty"`
	UserPrompts        []string `json:"-" yaml:"-"`
	StartedAt          string   `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	UpdatedAt          string   `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	ClosedAt           string   `json:"-" yaml:"-"`
	MessageCount       int      `json:"messageCount" yaml:"messageCount"`
	Source             string   `json:"source" yaml:"source"`
	collectUserPrompts bool
}

func SummaryOf(detail Session) Summary {
	item := Summary{
		ID: detail.ID, Agent: detail.Provider, IsSubagent: detail.IsSubagent,
		Workspace: detail.Workspace, Title: detail.Title,
		StartedAt: detail.StartedAt, UpdatedAt: detail.UpdatedAt,
		MessageCount: detail.MessageCount, Source: detail.Source,
	}
	for _, message := range detail.Messages {
		if message.Role == "user" {
			item.addUserMessage(textContent(message.Content))
		}
	}
	return item
}

func (s *Summary) addUserMessage(text string) {
	if s.collectUserPrompts && text != "" {
		s.UserPrompts = append(s.UserPrompts, strings.ToLower(text))
	}
	if !isSessionTitle(text) {
		return
	}
	s.TurnCount++
	if !isSessionPreview(text) {
		return
	}
	if s.FirstUserMessage == "" && hasSubagentMarker(text) {
		s.IsSubagent = true
	}
	text = oneLine(text, 60)
	if s.FirstUserMessage == "" {
		s.FirstUserMessage = text
	}
	s.LatestUserMessage = text
}

type ListOptions struct {
	Agent            string
	Workspace        string
	All              bool
	IncludeSubagents bool
	Limit            int
	Sort             string
}

type Service struct {
	HomeDir  string
	LookPath func(string) (string, error)
}

func New() Service {
	home, _ := os.UserHomeDir()
	return Service{HomeDir: home, LookPath: clibin.LookPath}
}

func (s Service) Agents() []Agent {
	agents := make([]Agent, 0, len(agentSpecs))
	for _, spec := range agentSpecs {
		_, err := s.lookPath(spec.binary)
		root := nativedirs.SessionRoots(spec.id, s.HomeDir)[0]
		agents = append(agents, Agent{
			ID: spec.id, Name: spec.name, Installed: err == nil, Root: root,
		})
	}
	return agents
}

func (s Service) List(options ListOptions) ([]Summary, error) {
	if options.Agent != "" && agentSpecByID(options.Agent) == nil {
		return nil, fmt.Errorf("unknown session agent %q", options.Agent)
	}
	if options.Sort == "" {
		options.Sort = "date"
	}
	if options.Sort != "date" && options.Sort != "messages" && options.Sort != "agent" {
		return nil, fmt.Errorf("unsupported session sort %q", options.Sort)
	}
	if options.Limit < 0 {
		return nil, fmt.Errorf("session limit must not be negative")
	}

	workspace := options.Workspace
	if workspace == "" && !options.All {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("read working directory: %w", err)
		}
	}
	if workspace != "" {
		workspace = cleanPath(workspace)
	}

	items, err := s.catalog(options.Agent, options.IncludeSubagents)
	failures := catalogFailure(err)
	items = filterSessions(items, workspace, options.IncludeSubagents)
	if options.Sort == "messages" {
		items = summarizeSessions(items, true, 0, failures)
	}
	sortSessions(items, options.Sort)
	if options.Sort == "messages" {
		items = limitSessions(items, options.Limit)
	} else {
		items = summarizeSessions(items, true, options.Limit, failures)
	}
	return items, failures.err()
}

func (s Service) Info(id, source string) (Session, error) {
	matches, err := s.Matches(id, source)
	if err != nil {
		return Session{}, err
	}
	if len(matches) == 0 {
		return Session{}, fmt.Errorf("session %q was not found", id)
	}
	if len(matches) > 1 {
		return Session{}, fmt.Errorf("session ID %q is ambiguous; pass --source <agent>", id)
	}
	return matches[0], nil
}

func (s Service) Matches(id, source string) ([]Session, error) {
	if id == "" {
		return nil, errors.New("session ID is required")
	}
	if source != "" && agentSpecByID(source) == nil {
		return nil, fmt.Errorf("unknown session agent %q", source)
	}
	entries, err := s.catalog(source, true)
	if err != nil {
		if source == "" && IsCatalogError(err) {
			// Without --source, skip errored agents (D3)
			fmt.Fprintf(os.Stderr, "warning: partial session catalog: %s\n", err)
		} else {
			return nil, err
		}
	}
	var matches []Session
	for _, entry := range entries {
		if entry.ID == id || strings.HasPrefix(entry.ID, id) {
			detail, err := s.Read(entry)
			if err != nil {
				return nil, err
			}
			matches = append(matches, detail)
		}
	}
	return matches, nil
}

func (s Service) lookPath(binary string) (string, error) {
	if s.LookPath == nil {
		return clibin.LookPath(binary)
	}
	return s.LookPath(binary)
}

// Read loads the exact native source selected from a session summary.
func (s Service) Read(item Summary) (Session, error) {
	spec := agentSpecByID(item.Agent)
	if spec == nil {
		return Session{}, fmt.Errorf("unknown session agent %q", item.Agent)
	}
	if item.ID == "" || item.Source == "" {
		return Session{}, errors.New("session ID and source path are required")
	}
	detail, err := spec.read(item.Source)
	if err != nil {
		return Session{}, fmt.Errorf("read %s session %s: %w", item.Agent, item.Source, err)
	}
	if detail.ID != item.ID {
		return Session{}, fmt.Errorf("session source %s contains ID %q, expected %q", item.Source, detail.ID, item.ID)
	}
	detail.Provider = item.Agent
	detail.Source = item.Source
	return detail, nil
}

// ParseRecords parses raw records from a native agent format into a unified Session.
// For JSONL agents (claude, codex, pi), records is the list of parsed JSON objects.
// For opencode, this returns false since opencode uses a file-per-message layout.
func ParseRecords(agent string, records []map[string]any) (Session, bool) {
	spec := agentSpecByID(agent)
	if spec == nil || spec.parse == nil {
		return Session{}, false
	}
	return spec.parse("stdin", records)
}
