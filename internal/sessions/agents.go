package sessions

type agentSpec struct {
	id     string
	name   string
	binary string
	// discoverIn scans one session root from nativedirs.SessionRoots.
	discoverIn func(root string, includeSubagents bool) ([]Summary, error)
	summarize  func(Summary, bool) (Summary, error)
	read       func(string) (Session, error)
	parse      func(string, []map[string]any) (Session, bool)
}

var agentSpecs = []agentSpec{
	{id: "agy", name: "Antigravity CLI", binary: "agy",
		discoverIn: discoverAgyIn, summarize: summarizeAgy, read: readAgySession},
	jsonlSpec("claude", "Claude Code", discoverClaudeIn, parseClaude),
	jsonlSpec("codex", "Codex CLI", discoverCodexIn, parseCodex),
	{id: "opencode", name: "OpenCode", binary: "opencode",
		discoverIn: discoverOpenCodeIn, summarize: summarizeOpenCode, read: readOpenCodeSession},
	jsonlSpec("pi", "Pi Coding Agent", discoverPiIn, parsePi),
}

func jsonlSpec(id, name string, discoverIn func(string, bool) ([]Summary, error), parse func(string, []map[string]any) (Session, bool)) agentSpec {
	return agentSpec{
		id: id, name: name, binary: id, discoverIn: discoverIn, parse: parse,
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
