package nativedirs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Dirs holds SM's two optional, orthogonal directory parameters.
type Dirs struct {
	SessionDir string // SM_SESSION_DIR; empty = native
	ConfigDir  string // SM_CONFIG_DIR; empty = native
}

func FromEnv() Dirs {
	return Dirs{
		SessionDir: os.Getenv("SM_SESSION_DIR"),
		ConfigDir:  os.Getenv("SM_CONFIG_DIR"),
	}
}

func (d Dirs) Empty() bool { return d.SessionDir == "" && d.ConfigDir == "" }

var (
	resolved     map[string]*AgentDirs
	resolvedErrs map[string]error
	prepared     map[string]error
	resolvedMu   sync.Mutex
)

// KnownAgents lists agents the resolver handles.
var KnownAgents = []string{"pi", "codex", "claude", "dsh", "agy"}

type Scope string

const (
	ScopeSession Scope = "session"
	ScopeConfig  Scope = "config"
	ScopeOther   Scope = "other"
)

// EnvVar is one environment variable sm may set on an agent's behalf.
type EnvVar struct {
	Name  string
	Scope Scope
}

type agentTable struct {
	root    string // native root relative to home
	rootVar string // env var the user may set to relocate the native root
	env     []EnvVar
}

// tables is the single source for every agent env var sm sets, restores and unsets.
var tables = map[string]agentTable{
	"pi":    {root: filepath.Join(".pi", "agent"), rootVar: "PI_CODING_AGENT_DIR", env: []EnvVar{{"PI_CODING_AGENT_DIR", ScopeSession}}},
	"codex": {root: ".codex", rootVar: "CODEX_HOME", env: []EnvVar{{"CODEX_HOME", ScopeSession}, {"CODEX_SQLITE_HOME", ScopeSession}}},
	"claude": {root: ".claude", rootVar: "CLAUDE_CONFIG_DIR", env: []EnvVar{
		{"CLAUDE_CONFIG_DIR", ScopeSession}, {"CLAUDE_SECURESTORAGE_CONFIG_DIR", ScopeOther},
	}},
	"dsh": {root: ".dsh", rootVar: "DSH_HOME"},
	"agy": {root: ".gemini"},
}

const nativePrefix = "SM_NATIVE_"

// AgentEnv returns the env vars sm may set for agent.
func AgentEnv(agent string) []EnvVar { return tables[agent].env }

// IsolatedEnvNames lists every env var that changes where sm or an agent
// reads native directories.
func IsolatedEnvNames() []string {
	names := []string{"SM_SESSION_DIR", "SM_CONFIG_DIR"}
	for _, agent := range KnownAgents {
		t := tables[agent]
		if t.rootVar != "" {
			names = append(names, t.rootVar, nativePrefix+t.rootVar)
		}
		for _, v := range t.env {
			if v.Name != t.rootVar {
				names = append(names, v.Name, nativePrefix+v.Name)
			}
		}
	}
	return names
}

// originalEnv returns the user's own value of name, before any sm (this one
// or a parent) overrode it.
func originalEnv(name string) (string, bool) {
	if marker, ok := os.LookupEnv(nativePrefix + name); ok {
		return decodeNative(marker)
	}
	return os.LookupEnv(name)
}

func encodeNative(value string, set bool) string {
	if set {
		return "set:" + value
	}
	return "unset"
}

func decodeNative(marker string) (string, bool) {
	if value, set := strings.CutPrefix(marker, "set:"); set {
		return value, true
	}
	return "", false
}

// NativeRoot is the single native root of agent: the user's own root env var
// when set, else the default under home (the user home when empty).
func NativeRoot(agent, home string) string {
	t := tables[agent]
	if t.rootVar != "" {
		if v, _ := originalEnv(t.rootVar); v != "" {
			return v
		}
	}
	if home == "" {
		home = userHome()
	}
	return filepath.Join(home, t.root)
}

// ClaudeJSONPath is the native .claude.json: claude keeps it beside home
// unless the user relocated CLAUDE_CONFIG_DIR.
func ClaudeJSONPath(home string) string {
	if v, _ := originalEnv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, ".claude.json")
	}
	if home == "" {
		home = userHome()
	}
	return filepath.Join(home, ".claude.json")
}

func nativeSessionRoots(agent, home string) []string {
	root := NativeRoot(agent, home)
	switch agent {
	case "pi", "codex":
		return []string{filepath.Join(root, "sessions")}
	case "claude":
		return []string{filepath.Join(root, "projects")}
	case "agy":
		return []string{filepath.Join(root, "antigravity-cli")}
	}
	return []string{root}
}

// SessionRoots returns where agent's sessions live: the S/C resolution when
// active, else the native roots.
func SessionRoots(agent, home string) []string {
	if ad := Cached(agent); ad != nil && len(ad.SessionRoots) > 0 {
		return ad.SessionRoots
	}
	return nativeSessionRoots(agent, home)
}

// restoreEnv undoes every override a parent sm recorded with SM_NATIVE_*:
// the user's value comes back when they had one, otherwise the var is unset.
func restoreEnv() {
	for _, agent := range KnownAgents {
		for _, v := range tables[agent].env {
			marker, ok := os.LookupEnv(nativePrefix + v.Name)
			if !ok {
				continue
			}
			if value, set := decodeNative(marker); set {
				os.Setenv(v.Name, value)
			} else {
				os.Unsetenv(v.Name)
			}
			os.Unsetenv(nativePrefix + v.Name)
		}
	}
}

// ApplyToProcess resolves all agents once and writes the results into sm's own
// process environment so every code path (session lookup, history import, list)
// and every child process sees the same directories. It writes nothing to disk;
// PrepareAgent does that for the one agent an invocation targets.
func ApplyToProcess(dirs Dirs) error {
	if dirs.SessionDir != "" && !filepath.IsAbs(dirs.SessionDir) {
		return fmt.Errorf("SM_SESSION_DIR=%q must be absolute", dirs.SessionDir)
	}
	if dirs.ConfigDir != "" && !filepath.IsAbs(dirs.ConfigDir) {
		return fmt.Errorf("SM_CONFIG_DIR=%q must be absolute", dirs.ConfigDir)
	}
	restoreEnv()
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	resolved, resolvedErrs, prepared = nil, nil, nil
	if dirs.Empty() {
		return nil
	}
	resolved = make(map[string]*AgentDirs, len(KnownAgents))
	resolvedErrs = make(map[string]error, len(KnownAgents))
	prepared = make(map[string]error, len(KnownAgents))
	for _, agent := range KnownAgents {
		ad, err := Resolve(agent, dirs)
		if err != nil {
			resolvedErrs[agent] = err
			resolved[agent] = &AgentDirs{Env: map[string]string{}, SessionRoots: []string{}}
			continue
		}
		resolved[agent] = ad
		for k, v := range MergeEnv(ad) {
			os.Setenv(k, v)
		}
	}
	return nil
}

// Cached returns the cached AgentDirs from ApplyToProcess, or nil if not cached.
func Cached(agent string) *AgentDirs {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if resolved == nil {
		return nil
	}
	return resolved[agent]
}

// CachedError returns any resolution error from ApplyToProcess for this agent.
func CachedError(agent string) error {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if resolvedErrs == nil {
		return nil
	}
	return resolvedErrs[agent]
}

// Ready reports whether agent can run under the active S/C. With prepare it
// also creates agent's directories, symlinks and patches (once per process);
// callers pass prepare only when they are about to launch or write for agent.
func Ready(agent string, prepare bool) error {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if resolved == nil {
		return nil
	}
	if err := resolvedErrs[agent]; err != nil {
		return err
	}
	if !prepare {
		return nil
	}
	if err, done := prepared[agent]; done {
		return err
	}
	err := Prepare(resolved[agent])
	if err != nil {
		err = fmt.Errorf("prepare %s directories: %w", agent, err)
	}
	prepared[agent] = err
	return err
}

// ResolvedArgs returns the agent-specific CLI args (--gemini_dir, --patch, etc.)
// from the cached resolution. Returns nil when no cache exists or dirs are empty.
func ResolvedArgs(agent string) []string {
	if ad := Cached(agent); ad != nil {
		return ad.Args
	}
	return nil
}

// AgyAppDataDir returns the resolved agy app_data_dir (the directory
// containing conversations/).
func AgyAppDataDir(fallbackHome string) string {
	return SessionRoots("agy", fallbackHome)[0]
}

// CodexHome returns the resolved CODEX_HOME.
func CodexHome() string {
	if ad := Cached("codex"); ad != nil {
		if v := ad.Env["CODEX_HOME"]; v != "" {
			return v
		}
	}
	return NativeRoot("codex", "")
}

// AgentDirs is the resolved directory configuration for one agent.
type AgentDirs struct {
	Env      map[string]string
	Args     []string
	Symlinks []Symlink
	// CDirs are directory entries Prepare creates in C so config links never
	// dangle and the agent's own writes through them (e.g. skill installs) land in C.
	CDirs      []string
	PatchFiles []PatchFile
	// GenHome is the composed root whose entries sm may generate; Generated
	// lists those entries (relative to GenHome) that are generated this run.
	GenHome      string
	Generated    []string
	SessionRoots []string
	ConfigDir    string
	NativeHome   string
	AppDataDir   string
	Limitation   string
}

type Symlink struct{ Link, Target string }

// configEntry is one user config entry an agent home links to its config
// source; dir marks entries that are directories.
type configEntry struct {
	name string
	dir  bool
}

// linkConfig links each entry under home to source. Only C gets missing
// directories created: C is the user's portable config, native is not sm's.
func (ad *AgentDirs) linkConfig(home, source string, entries []configEntry, fromC bool) {
	for _, entry := range entries {
		ad.Symlinks = append(ad.Symlinks, Symlink{filepath.Join(home, entry.name), filepath.Join(source, entry.name)})
		if fromC && entry.dir {
			ad.CDirs = append(ad.CDirs, filepath.Join(source, entry.name))
		}
	}
}

type PatchFile struct {
	Path    string
	Content []byte
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

func shadowDir(configDir, agent string) string {
	h := sha256.Sum256([]byte(configDir))
	return filepath.Join(userHome(), ".local", "state", "sessionman", "shadow", hex.EncodeToString(h[:8]), agent)
}

// Resolve returns the full directory configuration for one agent given SM's
// global S/C parameters. When both are empty, the result is an identity
// (empty env, native session roots, native config dir).
func Resolve(agent string, dirs Dirs) (*AgentDirs, error) {
	if dirs.SessionDir != "" && !filepath.IsAbs(dirs.SessionDir) {
		return nil, fmt.Errorf("SM_SESSION_DIR=%q must be absolute", dirs.SessionDir)
	}
	if dirs.ConfigDir != "" && !filepath.IsAbs(dirs.ConfigDir) {
		return nil, fmt.Errorf("SM_CONFIG_DIR=%q must be absolute", dirs.ConfigDir)
	}
	switch agent {
	case "pi":
		return resolvePi(dirs)
	case "codex":
		return resolveCodex(dirs)
	case "claude":
		return resolveClaude(dirs)
	case "dsh":
		return resolveDSH(dirs)
	case "agy":
		return resolveAgy(dirs)
	default:
		return &AgentDirs{
			Env: map[string]string{}, NativeHome: userHome(),
			SessionRoots: []string{},
		}, nil
	}
}

// NativeEnvExport returns an SM_NATIVE_<var> marker for every env var this
// resolution overrides, recording the user's own value (or its absence) so
// nested sm launches recover and restore it.
func NativeEnvExport(ad *AgentDirs) map[string]string {
	out := map[string]string{}
	for key := range ad.Env {
		value, set := originalEnv(key)
		out[nativePrefix+key] = encodeNative(value, set)
	}
	return out
}

// MergeEnv returns the agent env overlay including SM_NATIVE_* recovery markers.
func MergeEnv(ad *AgentDirs) map[string]string {
	env := NativeEnvExport(ad)
	for k, v := range ad.Env {
		env[k] = v
	}
	return env
}

func symlinkCollision(link, existing, target string) error {
	return fmt.Errorf("symlink collision: %s already points to %s, refusing to re-point to %s; use a different --session-dir or --config-dir", link, existing, target)
}

// dirConflict reports a path that must be a directory but is something else.
func dirConflict(dir string) error {
	for path := dir; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", path)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("check directory %s: %w", path, err)
		}
		if parent := filepath.Dir(path); parent == path {
			return nil
		}
	}
}

// symlinkNeeded reports whether link must be created; an existing link to a
// different target is a conflict, a real file is kept as is.
func symlinkNeeded(link Symlink) (bool, error) {
	info, err := os.Lstat(link.Link)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check symlink %s: %w", link.Link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	existing, err := os.Readlink(link.Link)
	if err != nil {
		return false, fmt.Errorf("check symlink %s: %w", link.Link, err)
	}
	if existing != link.Target {
		return false, symlinkCollision(link.Link, existing, link.Target)
	}
	return false, nil
}

func (ad *AgentDirs) dirs() []string {
	set := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir != "" && !set[dir] {
			set[dir] = true
			out = append(out, dir)
		}
	}
	for _, root := range ad.SessionRoots {
		add(root)
	}
	for _, link := range ad.Symlinks {
		add(filepath.Dir(link.Link))
	}
	for _, dir := range ad.CDirs {
		add(dir)
	}
	for _, patch := range ad.PatchFiles {
		add(filepath.Dir(patch.Path))
	}
	for key, value := range ad.Env {
		if strings.HasSuffix(key, "_DIR") || strings.HasSuffix(key, "_HOME") {
			add(value)
		}
	}
	return out
}

const generatedMarker = ".sessionman-generated"

// staleGenerated lists entries a previous run generated that this run no
// longer does; they must give way to the links that replace them.
func (ad *AgentDirs) staleGenerated() []string {
	if ad.GenHome == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(ad.GenHome, generatedMarker))
	if err != nil {
		return nil
	}
	var previous []string
	if json.Unmarshal(data, &previous) != nil {
		return nil
	}
	current := map[string]bool{}
	for _, name := range ad.Generated {
		current[name] = true
	}
	var stale []string
	for _, name := range previous {
		if !current[name] {
			stale = append(stale, filepath.Join(ad.GenHome, name))
		}
	}
	return stale
}

// removeUnlessLink removes a generated entry so it can be rewritten; a link
// is removed as a link, never followed into C.
func removeUnlessLink(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove generated entry %s: %w", path, err)
	}
	return nil
}

// Prepare validates the full plan (directories, symlinks, patch files) and
// only then creates it, so a conflict leaves the filesystem untouched.
func Prepare(ad *AgentDirs) error {
	dirs := ad.dirs()
	for _, dir := range dirs {
		if err := dirConflict(dir); err != nil {
			return err
		}
	}
	stale := map[string]bool{}
	for _, path := range ad.staleGenerated() {
		stale[path] = true
	}
	var links []Symlink
	for _, link := range ad.Symlinks {
		if stale[link.Link] {
			links = append(links, link)
			continue
		}
		needed, err := symlinkNeeded(link)
		if err != nil {
			return err
		}
		if needed {
			links = append(links, link)
		}
	}
	for _, patch := range ad.PatchFiles {
		if info, err := os.Lstat(patch.Path); err == nil && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("patch %s exists and is not a regular file", patch.Path)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("check patch %s: %w", patch.Path, err)
		}
	}
	for path := range stale {
		if err := removeUnlessLink(path); err != nil {
			return err
		}
	}
	for _, name := range ad.Generated {
		if err := removeUnlessLink(filepath.Join(ad.GenHome, name)); err != nil {
			return err
		}
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	for _, link := range links {
		if err := os.Symlink(link.Target, link.Link); err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("create symlink %s -> %s: %w", link.Link, link.Target, err)
			}
			if _, err := symlinkNeeded(link); err != nil {
				return err
			}
		}
	}
	for _, patch := range ad.PatchFiles {
		if err := os.WriteFile(patch.Path, patch.Content, 0600); err != nil {
			return fmt.Errorf("write patch %s: %w", patch.Path, err)
		}
	}
	return ad.recordGenerated()
}

func (ad *AgentDirs) recordGenerated() error {
	if ad.GenHome == "" {
		return nil
	}
	marker := filepath.Join(ad.GenHome, generatedMarker)
	if len(ad.Generated) == 0 {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", marker, err)
		}
		return nil
	}
	names := append([]string(nil), ad.Generated...)
	sort.Strings(names)
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ad.GenHome, 0700); err != nil {
		return fmt.Errorf("create directory %s: %w", ad.GenHome, err)
	}
	if err := os.WriteFile(marker, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", marker, err)
	}
	return nil
}

func resolvePi(dirs Dirs) (*AgentDirs, error) {
	native := NativeRoot("pi", "")
	nativeSessions := nativeSessionRoots("pi", "")[0]
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = []string{nativeSessions}
		return ad, nil
	}

	mount, err := loadHookMount(dirs)
	if err != nil {
		return nil, err
	}
	piConfigEntries := []configEntry{
		{"settings.json", false}, {"models.json", false}, {"AGENTS.md", false},
		{"prompts", true}, {"themes", true}, {"skills", true}, {"extensions", true},
	}
	configSource := native
	if dirs.ConfigDir != "" {
		configSource = filepath.Join(dirs.ConfigDir, "pi")
		ad.ConfigDir = configSource
	}

	if dirs.SessionDir != "" {
		piHome := filepath.Join(dirs.SessionDir, "pi")
		ad.Env["PI_CODING_AGENT_DIR"] = piHome
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(piHome, "auth.json"), filepath.Join(native, "auth.json")},
			Symlink{filepath.Join(piHome, "mcp-auth.json"), filepath.Join(native, "mcp-auth.json")},
		)
		ad.linkConfig(piHome, configSource, piConfigEntries, dirs.ConfigDir != "")
		if err := mount.mount(ad, "pi", piHome, configSource); err != nil {
			return nil, err
		}
		if err := mountUnifiedSkills(ad, dirs.ConfigDir, piHome, configSource, "skills"); err != nil {
			return nil, err
		}
		ad.SessionRoots = []string{filepath.Join(piHome, "sessions")}
	} else {
		// C only: shadow home; sessions stay native
		shadow := shadowDir(dirs.ConfigDir, "pi")
		ad.Env["PI_CODING_AGENT_DIR"] = shadow
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(shadow, "auth.json"), filepath.Join(native, "auth.json")},
			Symlink{filepath.Join(shadow, "mcp-auth.json"), filepath.Join(native, "mcp-auth.json")},
		)
		ad.linkConfig(shadow, configSource, piConfigEntries, true)
		if err := mount.mount(ad, "pi", shadow, configSource); err != nil {
			return nil, err
		}
		if err := mountUnifiedSkills(ad, dirs.ConfigDir, shadow, configSource, "skills"); err != nil {
			return nil, err
		}
		ad.Symlinks = append(ad.Symlinks, Symlink{
			filepath.Join(shadow, "sessions"), nativeSessions,
		})
		ad.SessionRoots = []string{nativeSessions}
	}

	return ad, nil
}

func resolveCodex(dirs Dirs) (*AgentDirs, error) {
	native := NativeRoot("codex", "")
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = nativeSessionRoots("codex", "")
		return ad, nil
	}

	mount, err := loadHookMount(dirs)
	if err != nil {
		return nil, err
	}
	configEntries := []configEntry{
		{"config.toml", false}, {"AGENTS.md", false}, {"AGENTS.override.md", false},
		{"skills", true}, {"hooks.json", false}, {"rules", true}, {"themes", true},
	}
	configSource := native
	if dirs.ConfigDir != "" {
		configSource = filepath.Join(dirs.ConfigDir, "codex")
		ad.ConfigDir = configSource
	}

	// S moves CODEX_HOME, changing the keyring key; check in both S and C-only paths
	if _, err := os.Stat(filepath.Join(native, "auth.json")); os.IsNotExist(err) {
		return nil, fmt.Errorf("codex requires file-based auth (auth.json at %s); keychain-based login is not supported with --session-dir or --config-dir", native)
	}

	if dirs.SessionDir != "" {
		codexHome := filepath.Join(dirs.SessionDir, "codex")
		ad.Env["CODEX_HOME"] = codexHome
		ad.Env["CODEX_SQLITE_HOME"] = codexHome
		ad.SessionRoots = []string{filepath.Join(codexHome, "sessions")}
		// config entries symlinked into the session home
		ad.linkConfig(codexHome, configSource, configEntries, dirs.ConfigDir != "")
		if err := mount.mount(ad, "codex", codexHome, configSource); err != nil {
			return nil, err
		}
		if err := mountUnifiedSkills(ad, dirs.ConfigDir, codexHome, configSource, "skills"); err != nil {
			return nil, err
		}
		if err := mountUnifiedMCP(ad, "codex", dirs.ConfigDir, codexHome, configSource); err != nil {
			return nil, err
		}
		// auth stays native
		ad.Symlinks = append(ad.Symlinks, Symlink{
			filepath.Join(codexHome, "auth.json"), filepath.Join(native, "auth.json"),
		})
	} else {
		// C only: shadow home with sessions + index pointing to native
		shadow := shadowDir(dirs.ConfigDir, "codex")
		ad.Env["CODEX_HOME"] = shadow
		ad.Env["CODEX_SQLITE_HOME"] = native
		ad.SessionRoots = []string{filepath.Join(native, "sessions")}
		ad.Limitation = "Codex C-only: session_index.jsonl diverges after thread delete"
		// config entries from C
		ad.linkConfig(shadow, configSource, configEntries, true)
		if err := mount.mount(ad, "codex", shadow, configSource); err != nil {
			return nil, err
		}
		if err := mountUnifiedSkills(ad, dirs.ConfigDir, shadow, configSource, "skills"); err != nil {
			return nil, err
		}
		if err := mountUnifiedMCP(ad, "codex", dirs.ConfigDir, shadow, configSource); err != nil {
			return nil, err
		}
		// session data stays native
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(shadow, "sessions"), filepath.Join(native, "sessions")},
			Symlink{filepath.Join(shadow, "archived_sessions"), filepath.Join(native, "archived_sessions")},
			Symlink{filepath.Join(shadow, "session_index.jsonl"), filepath.Join(native, "session_index.jsonl")},
		)
		// auth stays native
		ad.Symlinks = append(ad.Symlinks, Symlink{
			filepath.Join(shadow, "auth.json"), filepath.Join(native, "auth.json"),
		})
	}

	return ad, nil
}

func resolveClaude(dirs Dirs) (*AgentDirs, error) {
	native := NativeRoot("claude", "")
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = nativeSessionRoots("claude", "")
		return ad, nil
	}

	mount, err := loadHookMount(dirs)
	if err != nil {
		return nil, err
	}
	configEntries := []configEntry{
		{"settings.json", false}, {"CLAUDE.md", false}, {"agents", true}, {"commands", true},
		{"skills", true}, {"hooks", true}, {"plugins", true}, {"output-styles", true},
	}
	nativeClaudeJSON := ClaudeJSONPath("")

	configSource := native
	if dirs.ConfigDir != "" {
		configSource = filepath.Join(dirs.ConfigDir, "claude")
		ad.ConfigDir = configSource
	}

	ad.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = ""

	if dirs.SessionDir != "" {
		claudeHome := filepath.Join(dirs.SessionDir, "claude")
		ad.Env["CLAUDE_CONFIG_DIR"] = claudeHome
		ad.SessionRoots = []string{filepath.Join(claudeHome, "projects")}
		ad.linkConfig(claudeHome, configSource, configEntries, dirs.ConfigDir != "")
		if err := mount.mount(ad, "claude", claudeHome, configSource); err != nil {
			return nil, err
		}
		// .claude.json holds account/state (credentials); always link to true native
		ad.Symlinks = append(ad.Symlinks, Symlink{
			filepath.Join(claudeHome, ".claude.json"), nativeClaudeJSON,
		})
	} else {
		// C only: shadow home with projects/ pointing to native
		shadow := shadowDir(dirs.ConfigDir, "claude")
		ad.Env["CLAUDE_CONFIG_DIR"] = shadow
		ad.SessionRoots = []string{filepath.Join(native, "projects")}
		ad.linkConfig(shadow, configSource, configEntries, true)
		if err := mount.mount(ad, "claude", shadow, configSource); err != nil {
			return nil, err
		}
		// .claude.json holds account/state (credentials); always link to true native
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(shadow, ".claude.json"), nativeClaudeJSON},
			Symlink{filepath.Join(shadow, "projects"), filepath.Join(native, "projects")},
		)
	}

	return ad, nil
}

func resolveDSH(dirs Dirs) (*AgentDirs, error) {
	dshHome := NativeRoot("dsh", "")
	ad := &AgentDirs{
		Env: map[string]string{}, NativeHome: dshHome, ConfigDir: dshHome,
		SessionRoots: nativeSessionRoots("dsh", ""),
	}
	ad.Limitation = "DSH: AGENTS.md, skills, presets not movable"

	if dirs.Empty() {
		return ad, nil
	}

	// Generate overlay YAML for --patch (top-level array of loader patch entries)
	var entries []string
	if dirs.SessionDir != "" {
		sessDir := filepath.Join(dirs.SessionDir, "dsh", "sessions")
		storDir := filepath.Join(dirs.SessionDir, "dsh", "storages")
		entries = append(entries,
			fmt.Sprintf("- id: session-persistence-jsonl\n  config:\n    root: %q", sessDir),
			fmt.Sprintf("- id: storage-json\n  config:\n    root: %q", storDir),
		)
		ad.SessionRoots = []string{sessDir}
	}
	if dirs.ConfigDir != "" {
		settingsPath := filepath.Join(dirs.ConfigDir, "dsh", "settings.yaml")
		entries = append(entries, fmt.Sprintf("- id: settings\n  config:\n    path: %q", settingsPath))
		ad.ConfigDir = filepath.Join(dirs.ConfigDir, "dsh")
	}
	if len(entries) > 0 {
		patchDir := filepath.Join(userHome(), ".local", "state", "sessionman", "dsh-patches")
		h := sha256.Sum256([]byte(dirs.SessionDir + "\x00" + dirs.ConfigDir))
		patchPath := filepath.Join(patchDir, hex.EncodeToString(h[:8])+".yaml")
		content := strings.Join(entries, "\n")
		ad.PatchFiles = append(ad.PatchFiles, PatchFile{patchPath, []byte(content)})
		ad.Args = append(ad.Args, "--patch", patchPath)
	}

	return ad, nil
}

func resolveAgy(dirs Dirs) (*AgentDirs, error) {
	native := NativeRoot("agy", "")
	nativeAppData := nativeSessionRoots("agy", "")[0]
	ad := &AgentDirs{
		Env: map[string]string{}, NativeHome: native, ConfigDir: filepath.Join(native, "config"),
		AppDataDir:   nativeAppData,
		SessionRoots: []string{nativeAppData},
	}

	if dirs.Empty() {
		return ad, nil
	}

	// C set: gemini_dir is an sm-owned shadow because it also holds sm's own
	// agy plugin (hooks and MCP), an sm artifact rather than user config; user config entries
	// from C are symlinked into it.
	geminiDir := native
	if dirs.ConfigDir != "" {
		mount, err := loadHookMount(dirs)
		if err != nil {
			return nil, err
		}
		shadow := shadowDir(dirs.ConfigDir, "agy")
		geminiDir = filepath.Join(shadow, ".gemini")
		ad.Args = append(ad.Args, "--gemini_dir="+geminiDir)
		ad.ConfigDir = filepath.Join(geminiDir, "config")
		configSource := filepath.Join(dirs.ConfigDir, "agy", "config")
		// plugins/ is not linked: it holds sm's plugin, not user config.
		ad.linkConfig(filepath.Join(geminiDir, "config"), configSource, []configEntry{
			{"settings.json", false},
			{"skills", true},
			{"rules", true},
			{"mcp_config.json", false},
		}, true)
		if err := mount.mountAgy(ad, geminiDir); err != nil {
			return nil, err
		}
	}

	if dirs.SessionDir != "" {
		sessionDir := filepath.Join(dirs.SessionDir, "agy")
		rel, err := filepath.Rel(geminiDir, sessionDir)
		if err != nil {
			return nil, fmt.Errorf("agy --app_data_dir cannot be expressed relative to gemini_dir %s: %w", geminiDir, err)
		}
		ad.Args = append(ad.Args, "--app_data_dir="+rel)
		ad.AppDataDir = sessionDir
		ad.SessionRoots = []string{sessionDir}
		ad.Limitation = "agy: CLI settings.json lives with the session dir"
	} else if dirs.ConfigDir != "" {
		nativeRel, err := filepath.Rel(geminiDir, nativeAppData)
		if err != nil {
			return nil, fmt.Errorf("agy --app_data_dir cannot be expressed relative to gemini_dir %s: %w", geminiDir, err)
		}
		ad.Args = append(ad.Args, "--app_data_dir="+nativeRel)
	}

	return ad, nil
}

// HelpText returns the per-agent limitation documentation.
func HelpText() string {
	var env strings.Builder
	for _, agent := range KnownAgents {
		for _, v := range tables[agent].env {
			fmt.Fprintf(&env, "\n  %-9s %s (%s)", agent, v.Name, v.Scope)
		}
	}
	return `Native directory isolation (global flags, must precede the subcommand):
  --session-dir <abs>   S: sessions go to <S>/<agent>   (env: SM_SESSION_DIR)
  --config-dir  <abs>   C: config is linked from <C>/<agent> (env: SM_CONFIG_DIR)
  Setting the env var is equivalent to passing the flag; nested sm inherits it.
  Credentials stay native and are linked: pi auth.json, mcp-auth.json;
  codex auth.json (file auth required); claude .claude.json.
  S layout: pi <S>/pi, codex <S>/codex, claude <S>/claude, agy <S>/agy, dsh <S>/dsh

Per-agent limitations:
  codex     C-only: session_index.jsonl diverges after thread delete;
            keychain-based auth not supported (file auth required)
  dsh       AGENTS.md, skills, presets not movable
  agy       CLI settings.json travels with the session dir

Agent env vars sm sets (a nested sm without S/C restores the user's value or unsets it):` + env.String()
}
