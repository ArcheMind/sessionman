package sessions

import "path/filepath"

type agentSpec struct {
	id        string
	name      string
	binary    string
	root      func(string) string
	discover  func(home string, includeSubagents bool) ([]Summary, error)
	summarize func(Summary, bool) (Summary, error)
	read      func(string) (Session, error)
	parse     func(string, []map[string]any) (Session, bool)
}

var agentSpecs = []agentSpec{
	{id: "agy", name: "Antigravity CLI", binary: "agy", root: agyRoot,
		discover: discoverAgy, summarize: summarizeAgy, read: readAgySession},
	jsonlSpec("claude", "Claude Code", func(home string) string { return filepath.Join(home, ".claude", "projects") }, discoverClaude, parseClaude),
	jsonlSpec("codex", "Codex CLI", func(home string) string { return filepath.Join(home, ".codex", "sessions") }, discoverCodex, parseCodex),
	{id: "opencode", name: "OpenCode", binary: "opencode", root: openCodeRoot,
		discover: discoverOpenCode, summarize: summarizeOpenCode, read: readOpenCodeSession},
	jsonlSpec("pi", "Pi Coding Agent", func(home string) string { return filepath.Join(home, ".pi", "agent", "sessions") }, discoverPi, parsePi),
}

func jsonlSpec(id, name string, root func(string) string, discover func(string, bool) ([]Summary, error), parse func(string, []map[string]any) (Session, bool)) agentSpec {
	return agentSpec{
		id: id, name: name, binary: id, root: root, discover: discover, parse: parse,
		summarize: summarizeJSONL,
		read:      func(path string) (Session, error) { return readJSONLSession(id, path, parse) },
	}
}

func agentSpecByID(id string) *agentSpec {
	for index := range agentSpecs {
		if agentSpecs[index].id == id {
			return &agentSpecs[index]
		}
	}
	return nil
}
