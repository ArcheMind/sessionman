package nativedirs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
	resolvedMu   sync.Mutex
)

// KnownAgents lists agents the resolver handles.
var KnownAgents = []string{"pi", "codex", "claude", "opencode", "dsh", "agy"}

// ApplyToProcess resolves all agents once and writes the results into sm's own
// process environment so every code path (session lookup, history import, list)
// and every child process sees the same directories. Call once from main after
// parsing --session-dir / --config-dir.
func ApplyToProcess(dirs Dirs) error {
	if dirs.Empty() {
		return nil
	}
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	resolved = make(map[string]*AgentDirs, len(KnownAgents))
	resolvedErrs = make(map[string]error, len(KnownAgents))
	for _, agent := range KnownAgents {
		ad, err := Resolve(agent, dirs)
		if err != nil {
			resolvedErrs[agent] = err
			resolved[agent] = &AgentDirs{Env: map[string]string{}, SessionRoots: []string{}}
			continue
		}
		if err := Prepare(ad); err != nil {
			return fmt.Errorf("prepare %s directories: %w", agent, err)
		}
		resolved[agent] = ad
		for k, v := range ad.Env {
			os.Setenv(k, v)
		}
		for _, k := range ad.UnsetEnv {
			os.Unsetenv(k)
		}
		for k, v := range NativeEnvExport(ad) {
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

// CachedError returns any error from ApplyToProcess for this agent.
func CachedError(agent string) error {
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if resolvedErrs == nil {
		return nil
	}
	return resolvedErrs[agent]
}

// ResolvedArgs returns the agent-specific CLI args (--gemini_dir, --patch, etc.)
// from the cached resolution. Returns nil when no cache exists or dirs are empty.
func ResolvedArgs(agent string) []string {
	if ad := Cached(agent); ad != nil {
		return ad.Args
	}
	return nil
}

// GeminiDir returns the resolved gemini directory (native or overridden by C).
// When fallbackHome is non-empty, it is used as the base for the native default;
// otherwise the user home directory is used.
func GeminiDir(fallbackHome string) string {
	if ad := Cached("agy"); ad != nil && ad.ConfigDir != "" {
		return filepath.Dir(ad.ConfigDir)
	}
	if fallbackHome == "" {
		fallbackHome = userHome()
	}
	return filepath.Join(fallbackHome, ".gemini")
}

// AgentDirs is the resolved directory configuration for one agent.
type AgentDirs struct {
	Env          map[string]string
	UnsetEnv     []string
	Args         []string
	Symlinks     []Symlink
	PatchFiles   []PatchFile
	SessionRoots []string
	ConfigDir    string
	NativeHome   string
	Limitation   string
}

type Symlink struct{ Link, Target string }
type PatchFile struct{ Path string; Content []byte }

// nativeHome resolves the native location for an agent's home env var,
// preventing shadow-of-shadow: SM_NATIVE_<VAR> >> <VAR> >> default.
func nativeHome(envKey, defaultRel string) string {
	if v := os.Getenv("SM_NATIVE_" + envKey); v != "" {
		return v
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, defaultRel)
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
	case "opencode":
		return resolveOpenCode(dirs)
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

// NativeEnvExport returns the SM_NATIVE_<key> export entries for all env vars
// this resolution overrides. The caller should set these on the child process
// so nested SM launches recover the true native location.
func NativeEnvExport(ad *AgentDirs) map[string]string {
	out := map[string]string{}
	nativeKeys := map[string]string{
		"CODEX_HOME":           "SM_NATIVE_CODEX_HOME",
		"CLAUDE_CONFIG_DIR":    "SM_NATIVE_CLAUDE_CONFIG_DIR",
		"PI_CODING_AGENT_DIR":  "SM_NATIVE_PI_CODING_AGENT_DIR",
	}
	for key, nativeKey := range nativeKeys {
		if _, overridden := ad.Env[key]; overridden {
			if os.Getenv(nativeKey) == "" {
				out[nativeKey] = ad.NativeHome
			}
		}
	}
	return out
}

// EnsureSymlinks creates all symlinks idempotently. When a symlink already
// points to a different target, it fails with a collision error (D9a) instead
// of silently re-pointing, unless allowRepoint is true.
func EnsureSymlinks(links []Symlink) error {
	for _, link := range links {
		if err := os.MkdirAll(filepath.Dir(link.Link), 0700); err != nil {
			return fmt.Errorf("create parent for symlink %s: %w", link.Link, err)
		}
		existing, err := os.Readlink(link.Link)
		if err == nil {
			if existing == link.Target {
				continue
			}
			return fmt.Errorf("symlink collision: %s already points to %s, refusing to re-point to %s; use a different --session-dir or --config-dir", link.Link, existing, link.Target)
		} else if !os.IsNotExist(err) {
			info, statErr := os.Lstat(link.Link)
			if statErr != nil {
				return fmt.Errorf("check symlink target %s: %w", link.Link, statErr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue // real file already exists; do not replace
			}
		}
		if err := os.Symlink(link.Target, link.Link); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create symlink %s -> %s: %w", link.Link, link.Target, err)
		}
	}
	return nil
}

// EnsurePatchFiles writes all patch files idempotently.
func EnsurePatchFiles(patches []PatchFile) error {
	for _, p := range patches {
		if err := os.MkdirAll(filepath.Dir(p.Path), 0700); err != nil {
			return fmt.Errorf("create parent for patch %s: %w", p.Path, err)
		}
		if err := os.WriteFile(p.Path, p.Content, 0600); err != nil {
			return fmt.Errorf("write patch %s: %w", p.Path, err)
		}
	}
	return nil
}

// EnsureDirs creates the directories needed by symlink targets and env-pointed dirs.
func EnsureDirs(ad *AgentDirs) error {
	dirs := map[string]bool{}
	for _, root := range ad.SessionRoots {
		dirs[root] = true
	}
	for _, link := range ad.Symlinks {
		dirs[filepath.Dir(link.Link)] = true
	}
	for key, value := range ad.Env {
		if strings.HasSuffix(key, "_DIR") || strings.HasSuffix(key, "_HOME") || key == "CLAUDE_CONFIG_DIR" {
			dirs[value] = true
		}
	}
	for dir := range dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// Prepare creates all directories, symlinks, and patch files for the resolution.
func Prepare(ad *AgentDirs) error {
	if err := EnsureDirs(ad); err != nil {
		return err
	}
	if err := EnsureSymlinks(ad.Symlinks); err != nil {
		return err
	}
	return EnsurePatchFiles(ad.PatchFiles)
}

// MergeEnv returns the agent env overlay including SM_NATIVE_* recovery vars.
func MergeEnv(ad *AgentDirs) (map[string]string, []string) {
	env := map[string]string{}
	for k, v := range ad.Env {
		env[k] = v
	}
	for k, v := range NativeEnvExport(ad) {
		env[k] = v
	}
	return env, ad.UnsetEnv
}

func resolvePi(dirs Dirs) (*AgentDirs, error) {
	native := nativeHome("PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent"))
	nativeSessions := filepath.Join(native, "sessions")
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = []string{nativeSessions}
		return ad, nil
	}

	if dirs.ConfigDir != "" {
		configDir := filepath.Join(dirs.ConfigDir, "pi")
		ad.Env["PI_CODING_AGENT_DIR"] = configDir
		ad.ConfigDir = configDir
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(configDir, "auth.json"), filepath.Join(native, "auth.json")},
			Symlink{filepath.Join(configDir, "mcp-auth.json"), filepath.Join(native, "mcp-auth.json")},
		)
	}

	if dirs.SessionDir != "" {
		sessionDir := filepath.Join(dirs.SessionDir, "pi")
		ad.Env["PI_CODING_AGENT_SESSION_DIR"] = sessionDir
		ad.SessionRoots = []string{sessionDir}
	} else {
		// C only: pin sessions to native so they don't follow PI_CODING_AGENT_DIR
		ad.Env["PI_CODING_AGENT_SESSION_DIR"] = nativeSessions
		ad.SessionRoots = []string{nativeSessions}
	}

	return ad, nil
}

func resolveCodex(dirs Dirs) (*AgentDirs, error) {
	native := nativeHome("CODEX_HOME", ".codex")
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = []string{filepath.Join(native, "sessions")}
		return ad, nil
	}

	configEntries := []string{
		"config.toml", "AGENTS.md", "AGENTS.override.md",
		"skills", "hooks.json", "rules", "themes",
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
		for _, entry := range configEntries {
			ad.Symlinks = append(ad.Symlinks, Symlink{
				filepath.Join(codexHome, entry), filepath.Join(configSource, entry),
			})
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
		for _, entry := range configEntries {
			ad.Symlinks = append(ad.Symlinks, Symlink{
				filepath.Join(shadow, entry), filepath.Join(configSource, entry),
			})
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
	native := nativeHome("CLAUDE_CONFIG_DIR", ".claude")
	ad := &AgentDirs{Env: map[string]string{}, NativeHome: native, ConfigDir: native}

	if dirs.Empty() {
		ad.SessionRoots = []string{filepath.Join(native, "projects")}
		return ad, nil
	}

	configEntries := []string{
		"settings.json", "CLAUDE.md", "agents", "commands",
		"skills", "hooks", "plugins", "output-styles",
	}
	// .claude.json native location: inside native config dir if it was set,
	// otherwise ~/.claude.json (home root).
	nativeClaudeJSON := filepath.Join(native, ".claude.json")
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" && os.Getenv("SM_NATIVE_CLAUDE_CONFIG_DIR") == "" {
		nativeClaudeJSON = filepath.Join(userHome(), ".claude.json")
	}

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
		for _, entry := range configEntries {
			ad.Symlinks = append(ad.Symlinks, Symlink{
				filepath.Join(claudeHome, entry), filepath.Join(configSource, entry),
			})
		}
		// .claude.json → config source or native
		ad.Symlinks = append(ad.Symlinks, Symlink{
			filepath.Join(claudeHome, ".claude.json"),
			func() string {
				if dirs.ConfigDir != "" {
					return filepath.Join(configSource, ".claude.json")
				}
				return nativeClaudeJSON
			}(),
		})
	} else {
		// C only: shadow home with projects/ pointing to native
		shadow := shadowDir(dirs.ConfigDir, "claude")
		ad.Env["CLAUDE_CONFIG_DIR"] = shadow
		ad.SessionRoots = []string{filepath.Join(native, "projects")}
		for _, entry := range configEntries {
			ad.Symlinks = append(ad.Symlinks, Symlink{
				filepath.Join(shadow, entry), filepath.Join(configSource, entry),
			})
		}
		ad.Symlinks = append(ad.Symlinks,
			Symlink{filepath.Join(shadow, ".claude.json"), filepath.Join(configSource, ".claude.json")},
			Symlink{filepath.Join(shadow, "projects"), filepath.Join(native, "projects")},
		)
	}

	return ad, nil
}

func resolveOpenCode(dirs Dirs) (*AgentDirs, error) {
	home := userHome()
	native := filepath.Join(home, ".opencode")
	ad := &AgentDirs{
		Env: map[string]string{}, NativeHome: native, ConfigDir: native,
		SessionRoots: []string{filepath.Join(home, ".local", "share", "opencode", "project")},
	}

	if dirs.ConfigDir != "" {
		return nil, fmt.Errorf("opencode does not support --config-dir: only XDG_CONFIG_HOME would work and it affects all child processes")
	}

	if dirs.SessionDir != "" {
		dbDir := filepath.Join(dirs.SessionDir, "opencode")
		ad.Env["OPENCODE_DB"] = filepath.Join(dbDir, "opencode.db")
		ad.SessionRoots = []string{dbDir}
	}

	return ad, nil
}

func resolveDSH(dirs Dirs) (*AgentDirs, error) {
	dshHome := os.Getenv("DSH_HOME")
	if dshHome == "" {
		dshHome = filepath.Join(userHome(), ".dsh")
	}
	ad := &AgentDirs{
		Env: map[string]string{}, NativeHome: dshHome, ConfigDir: dshHome,
		SessionRoots: []string{dshHome},
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
	native := filepath.Join(userHome(), ".gemini")
	ad := &AgentDirs{
		Env: map[string]string{}, NativeHome: native, ConfigDir: filepath.Join(native, "config"),
		SessionRoots: []string{filepath.Join(native, "antigravity-cli")},
	}

	if dirs.Empty() {
		return ad, nil
	}

	geminiDir := native
	if dirs.ConfigDir != "" {
		geminiDir = filepath.Join(dirs.ConfigDir, "agy")
		ad.Args = append(ad.Args, "--gemini_dir="+geminiDir)
		ad.ConfigDir = filepath.Join(geminiDir, "config")
	}

	if dirs.SessionDir != "" {
		sessionDir := filepath.Join(dirs.SessionDir, "agy")
		// --app_data_dir must be relative to gemini_dir; absolute is rejected
		rel, err := filepath.Rel(geminiDir, sessionDir)
		if err != nil {
			return nil, fmt.Errorf("agy --app_data_dir cannot be expressed relative to gemini_dir %s: %w", geminiDir, err)
		}
		ad.Args = append(ad.Args, "--app_data_dir="+rel)
		ad.SessionRoots = []string{filepath.Join(sessionDir, "conversations")}
		ad.Limitation = "agy: CLI settings.json lives with the session dir"
	} else if dirs.ConfigDir != "" {
		// C only: gemini_dir moved; pin --app_data_dir to native so sessions stay native
		nativeRel, err := filepath.Rel(geminiDir, filepath.Join(native, "antigravity-cli"))
		if err != nil {
			return nil, fmt.Errorf("agy --app_data_dir cannot be expressed relative to gemini_dir %s: %w", geminiDir, err)
		}
		ad.Args = append(ad.Args, "--app_data_dir="+nativeRel)
		ad.SessionRoots = []string{filepath.Join(native, "antigravity-cli")}
	}

	return ad, nil
}

// HelpText returns the per-agent limitation documentation.
func HelpText() string {
	return `Native directory parameters:
  --session-dir <path>   Override session storage for all agents (absolute path)
  --config-dir <path>    Override user config for all agents (absolute path)

Both set SM_SESSION_DIR / SM_CONFIG_DIR and are inherited by nested sm launches.
Neither set: pure native behaviour; each agent uses its own default directories.

Per-agent limitations:
  codex   C-only: session_index.jsonl diverges after thread delete;
          keychain-based auth not supported (file auth required)
  opencode  --config-dir unsupported (XDG_CONFIG_HOME leaks to children)
  dsh     AGENTS.md, skills, presets not movable
  agy     CLI settings.json travels with the session dir`
}
