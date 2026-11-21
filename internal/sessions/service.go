package sessions

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ArcheMind/agentx/internal/nativedirs"
	transcript "github.com/ArcheMind/sessionman-protocol/go"
)

type Agent struct {
	ID        string `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Installed bool   `json:"installed" yaml:"installed"`
	Root      string `json:"session_root" yaml:"session_root"`
}

// Transcript shapes are owned by the shared protocol module; agentx defines
// no message shapes of its own.
type (
	ContentBlock = transcript.ContentBlock
	Message      = transcript.Message
	Session      = transcript.Session
)

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
	return Service{HomeDir: home, LookPath: exec.LookPath}
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
		return exec.LookPath(binary)
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
