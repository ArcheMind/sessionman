package sessions

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArcheMind/agentx/internal/nativedirs"
)

// claudeProjectsRoot returns the claude projects directory, following
// CLAUDE_CONFIG_DIR set by the nativedirs resolver.
func claudeProjectsRoot(homeDir string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	return filepath.Join(homeDir, ".claude", "projects")
}

// piSessionRoot returns the pi session directory, following
// PI_CODING_AGENT_DIR set by the nativedirs resolver.
func piSessionRoot(homeDir string) string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(homeDir, ".pi", "agent", "sessions")
}

// PlanSessionFile returns the resume token and the path a write for agentID
// would create, touching nothing; dry runs report it instead of writing.
func PlanSessionFile(detail Session, agentID, dir, homeDir string) (token, path string, err error) {
	switch agentID {
	case "agy":
		id, path := agySessionPath(nativedirs.AgyAppDataDir(homeDir))
		return id, path, nil
	case "codex":
		id, path := codexSessionPath(nativedirs.CodexHome(), time.Now())
		return id, path, nil
	case "claude":
		id, path := claudeSessionPath(detail.Workspace, homeDir)
		return id, path, nil
	case "pi":
		_, _, path, err := piSessionPath(detail.Workspace, homeDir)
		return path, path, err
	case "opencode":
		base := filepath.Join(dir, newUUID())
		return base, base, nil
	default:
		return "", "", fmt.Errorf("unsupported resume target %q", agentID)
	}
}

func claudeSessionPath(workspace, homeDir string) (id, path string) {
	id = newUUID()
	return id, filepath.Join(claudeProjectsRoot(homeDir), strings.ReplaceAll(workspace, "/", "-"), id+".jsonl")
}

func piSessionPath(workspace, homeDir string) (id, resolvedWorkspace, path string, err error) {
	if workspace == "" {
		if workspace, err = os.Getwd(); err != nil {
			return "", "", "", fmt.Errorf("resolve pi session workspace: %w", err)
		}
	}
	id = newUUID()
	return id, workspace, filepath.Join(piSessionRoot(homeDir), piSessionDirName(workspace), id+".jsonl"), nil
}

func codexSessionPath(codexHome string, now time.Time) (id, path string) {
	id = newUUID()
	name := fmt.Sprintf("rollout-%s-%s.jsonl", now.Format("2006-01-02T15-04-05"), id)
	return id, filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"), name)
}

func WriteSessionFile(detail Session, agentID, dir, homeDir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create resume directory: %w", err)
	}
	id := detail.ID
	if id == "" {
		id = "resume"
	}

	switch agentID {
	case "agy":
		return WriteAgySessionFile(detail, nativedirs.AgyAppDataDir(homeDir))
	case "claude":
		newID, path := claudeSessionPath(detail.Workspace, homeDir)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("create claude session directory: %w", err)
		}
		detail.ID = newID
		return newID, writeJSONLFile(path, SerializeClaude(detail))
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
		return path, writeJSONLFile(path, SerializePi(detail))
	case "opencode":
		base := filepath.Join(dir, newUUID())
		info, entries := SerializeOpenCode(detail, base)
		infoPath := filepath.Join(base, "session.json")
		if err := os.MkdirAll(filepath.Dir(infoPath), 0700); err != nil {
			return "", err
		}
		if err := writeJSONFile(infoPath, info); err != nil {
			return "", err
		}
		for _, entry := range entries {
			if err := os.MkdirAll(filepath.Dir(entry.Path), 0700); err != nil {
				return "", err
			}
			if err := writeJSONFile(entry.Path, entry.Content); err != nil {
				return "", err
			}
		}
		return base, nil
	default:
		return "", fmt.Errorf("unsupported resume target %q", agentID)
	}
}

// piSessionDirName mirrors pi's getDefaultSessionDirPath (pi-coding-agent
// dist/core/session-manager.js) so pi's own session picker finds the file.
func piSessionDirName(workspace string) string {
	if len(workspace) > 0 && (workspace[0] == '/' || workspace[0] == '\\') {
		workspace = workspace[1:]
	}
	return "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(workspace) + "--"
}

func WriteCodexSessionFile(detail Session, codexHome, provider string) (string, error) {
	if strings.TrimSpace(provider) == "" {
		return "", fmt.Errorf("Codex model provider is required")
	}
	id, path := codexSessionPath(codexHome, time.Now())
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create codex session directory: %w", err)
	}
	detail.ID = id
	records := SerializeCodex(detail)
	records[0]["payload"].(map[string]any)["model_provider"] = provider
	return id, writeJSONLFile(path, records)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeJSONLFile(path string, records []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, data any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}
