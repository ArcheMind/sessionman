package nativedirs

import (
	"os"
	"path/filepath"
	"testing"
)

// Missing directory entries are created in C so links never dangle and agent
// writes (e.g. skill installs) persist in C; missing file entries stay missing.
func TestPrepareCreatesMissingDirEntriesInC(t *testing.T) {
	cases := []struct {
		agent string
		dirs  []string
		files []string
	}{
		{"codex", []string{"skills", "rules", "themes"}, []string{"config.toml", "AGENTS.md", "AGENTS.override.md", "hooks.json"}},
		{"claude", []string{"agents", "commands", "skills", "hooks", "plugins", "output-styles"}, []string{"settings.json", "CLAUDE.md"}},
		{"pi", []string{"prompts", "themes", "skills", "extensions"}, []string{"settings.json", "models.json", "AGENTS.md"}},
	}
	for _, sMode := range []bool{true, false} {
		for _, tc := range cases {
			resetCache(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", codexAuth(t))
			dirs := Dirs{ConfigDir: t.TempDir()}
			if sMode {
				dirs.SessionDir = t.TempDir()
			}
			ad, err := Resolve(tc.agent, dirs)
			if err != nil {
				t.Fatal(err)
			}
			if err := Prepare(ad); err != nil {
				t.Fatalf("%s %+v: %v", tc.agent, dirs, err)
			}
			source := filepath.Join(dirs.ConfigDir, tc.agent)
			for _, name := range tc.dirs {
				if info, err := os.Stat(filepath.Join(source, name)); err != nil || !info.IsDir() {
					t.Errorf("%s %+v: %s not created as a directory in C: %v", tc.agent, dirs, name, err)
				}
			}
			for _, name := range tc.files {
				if _, err := os.Lstat(filepath.Join(source, name)); !os.IsNotExist(err) {
					t.Errorf("%s %+v: file entry %s was created in C: %v", tc.agent, dirs, name, err)
				}
			}
			for _, link := range ad.Symlinks {
				if filepath.Dir(link.Target) != source {
					continue
				}
				name := filepath.Base(link.Target)
				for _, dir := range tc.dirs {
					if dir == name {
						if _, err := os.Stat(link.Link); err != nil {
							t.Errorf("%s %+v: link %s dangles: %v", tc.agent, dirs, link.Link, err)
						}
					}
				}
			}
		}
	}
}

// Without C the config source is native, which sm does not populate.
func TestPrepareSessionOnlyCreatesNoNativeConfigDirs(t *testing.T) {
	resetCache(t)
	t.Setenv("HOME", t.TempDir())
	native := codexAuth(t)
	t.Setenv("CODEX_HOME", native)
	ad, err := Resolve("codex", Dirs{SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Prepare(ad); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(native, "skills")); !os.IsNotExist(err) {
		t.Errorf("S-only Prepare created native skills: %v", err)
	}
}

// A C entry that should be a directory but is a file is a conflict found
// before any write.
func TestPrepareCDirConflictWritesNothing(t *testing.T) {
	resetCache(t)
	t.Setenv("HOME", t.TempDir())
	s, c := t.TempDir(), t.TempDir()
	source := filepath.Join(c, "claude")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "skills"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ad, err := Resolve("claude", Dirs{SessionDir: s, ConfigDir: c})
	if err != nil {
		t.Fatal(err)
	}
	before := treeEntries(t, c)
	if err := Prepare(ad); err == nil {
		t.Fatal("expected conflict for file in place of a directory entry")
	}
	if got := treeEntries(t, s); len(got) != 0 {
		t.Errorf("conflicting Prepare wrote under S: %v", got)
	}
	if after := treeEntries(t, c); len(after) != len(before) {
		t.Errorf("conflicting Prepare wrote under C: before %v after %v", before, after)
	}
}
