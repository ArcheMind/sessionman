package nativedirs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArcheMind/sessionman/internal/hookmount"
)

// hookMount carries what composing an agent root needs for unified hooks:
// the hooks defined in C and the sm binary the generated bridges call.
type hookMount struct {
	hooks      []hookmount.Hook
	executable string
}

// loadHookMount returns nil when C is unset: unified hooks then do not exist.
func loadHookMount(dirs Dirs) (*hookMount, error) {
	if dirs.ConfigDir == "" {
		return nil, nil
	}
	result, err := hookmount.Load(dirs.ConfigDir)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve SessionMan executable: %w", err)
	}
	return &hookMount{hooks: result.Hooks, executable: executable}, nil
}

// unlink drops a config link that a generated entry replaces.
func (ad *AgentDirs) unlink(home, name string) {
	link := filepath.Join(home, name)
	kept := ad.Symlinks[:0]
	for _, symlink := range ad.Symlinks {
		if symlink.Link != link {
			kept = append(kept, symlink)
		}
	}
	ad.Symlinks = kept
}

func (ad *AgentDirs) generate(home, name string, content []byte) {
	ad.Generated = append(ad.Generated, name)
	ad.PatchFiles = append(ad.PatchFiles, PatchFile{filepath.Join(home, name), content})
}

// mount replaces the hook-bearing entries of a composed root with generated
// ones; without hooks the entries stay plain links to C.
func (m *hookMount) mount(ad *AgentDirs, agent, home, configSource string) error {
	if m == nil {
		return nil
	}
	ad.GenHome = home
	if len(m.hooks) == 0 {
		return nil
	}
	switch agent {
	case "codex":
		if !hookmount.HasAgentHooks("codex", m.hooks) {
			return nil
		}
		content, err := hookmount.CodexHooks(m.executable, m.hooks)
		if err != nil {
			return err
		}
		ad.unlink(home, "hooks.json")
		ad.generate(home, "hooks.json", content)
		// Codex 0.170+ skips hooks.json unless persisted trust exists or hook trust
		// is bypassed; sm generated this hooks.json from the caller's vetted C dir.
		ad.Args = append(ad.Args, "--dangerously-bypass-hook-trust")
	case "claude":
		base, err := os.ReadFile(filepath.Join(configSource, "settings.json"))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read claude settings.json: %w", err)
		}
		content, err := hookmount.ClaudeSettings(m.executable, base, m.hooks)
		if err != nil {
			return err
		}
		ad.unlink(home, "settings.json")
		ad.generate(home, "settings.json", content)
	case "pi":
		source, err := hookmount.PiExtension(m.executable, m.hooks)
		if err != nil {
			return err
		}
		const generated = "sessionman-hooks.ts"
		ad.unlink(home, "extensions")
		ad.Generated = append(ad.Generated, "extensions")
		entries, err := os.ReadDir(filepath.Join(configSource, "extensions"))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read pi extensions: %w", err)
		}
		for _, entry := range entries {
			if entry.Name() == generated || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			ad.Symlinks = append(ad.Symlinks, Symlink{
				filepath.Join(home, "extensions", entry.Name()),
				filepath.Join(configSource, "extensions", entry.Name()),
			})
		}
		ad.PatchFiles = append(ad.PatchFiles, PatchFile{filepath.Join(home, "extensions", generated), []byte(source)})
	default:
		return fmt.Errorf("agent %s has no hook mount", agent)
	}
	return nil
}

// mountAgy writes sm's agy plugin into the composed gemini root; the MCP
// registration is present with or without hooks.
func (m *hookMount) mountAgy(ad *AgentDirs, geminiDir string) error {
	if m == nil {
		return nil
	}
	files, err := hookmount.AgyPlugin(m.executable, m.hooks)
	if err != nil {
		return err
	}
	ad.GenHome = geminiDir
	const name = "config/plugins/sessionman"
	ad.Generated = append(ad.Generated, name)
	for file, content := range files {
		ad.PatchFiles = append(ad.PatchFiles, PatchFile{filepath.Join(geminiDir, filepath.FromSlash(name), file), content})
	}
	return nil
}
