package clibin_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ArcheMind/agentx/internal/clibin"
)

func TestSpecReleaseComparisonUsesNumericStableVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.9.0", "0.10.0", -1},
		{"0.154.0", "0.154.0", 0},
		{"1.0.0", "0.999.999", 1},
		{"2.10.0", "2.9.99", 1},
	} {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			got, err := clibin.CompareVersions(tc.a, tc.b)
			if err != nil || got != tc.want {
				t.Fatalf("CompareVersions(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
			}
		})
	}
	for _, invalid := range []string{"", "development", "0.155.0-beta.1", "0.155.0-rc.2"} {
		if _, err := clibin.CompareVersions(invalid, "0.154.0"); err == nil {
			t.Errorf("unresolved or prerelease version %q must not compare as stable", invalid)
		}
	}
}

func TestSpecStableVersionRecognizesCLIOutputWithoutAcceptingPrereleases(t *testing.T) {
	for _, raw := range []string{"0.154.0", "v0.154.0", "codex-cli 0.154.0"} {
		got, err := clibin.StableVersion(raw)
		if err != nil || got != "0.154.0" {
			t.Errorf("StableVersion(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"codex-cli 0.155.0-alpha.1", "codex-cli 0.155.0-rc.1", "unknown", ""} {
		if _, err := clibin.StableVersion(raw); err == nil {
			t.Errorf("%q is not a confirmed stable release", raw)
		}
	}
}

func TestSpecNewestInstalledStableCodexWinsOverBundledAndPrereleaseCopies(t *testing.T) {
	versions := map[string]string{
		"/bundle/codex":  "codex-cli 0.120.0",
		"/local/codex":   "codex-cli 0.154.0",
		"/preview/codex": "codex-cli 0.155.0-beta.1",
	}
	path, version, err := clibin.Select(context.Background(),
		[]string{"/bundle/codex", "/broken/codex", "/preview/codex", "/local/codex"},
		func(_ context.Context, path string) (string, error) {
			if value, ok := versions[path]; ok {
				return value, nil
			}
			return "", errors.New("executable cannot start")
		})
	if err != nil || path != "/local/codex" || version != "0.154.0" {
		t.Fatalf("selected %q version %q (%v); want newest usable stable local Codex", path, version, err)
	}
}

func TestSpecEqualStableVersionsRetainPreferenceAndUnknownVersionsStayUnresolved(t *testing.T) {
	path, _, err := clibin.Select(context.Background(), []string{"/preferred", "/other"},
		func(context.Context, string) (string, error) { return "1.2.3", nil })
	if err != nil || path != "/preferred" {
		t.Fatalf("equal versions changed the existing executable preference: %q, %v", path, err)
	}
	for _, paths := range [][]string{nil, {"/unknown"}} {
		if _, _, err := clibin.Select(context.Background(), paths,
			func(context.Context, string) (string, error) { return "development", nil }); err == nil {
			t.Error("absence of a confirmed usable stable candidate must remain unresolved")
		}
	}
}

func TestSpecWithoutManagedRootPreservesPATHSelectionWithoutProbing(t *testing.T) {
	root := t.TempDir()
	firstDir := filepath.Join(root, "first")
	secondDir := filepath.Join(root, "second")
	first := specCodexExecutable(t, firstDir, "0.0.1")
	specCodexExecutable(t, secondDir, "9.9.9")
	t.Setenv("SM_CLI_INSTALL_ROOT", "")
	t.Setenv("PATH", firstDir+string(os.PathListSeparator)+secondDir)
	var probes []string
	selected, err := clibin.Resolve(context.Background(), "codex", "", func(path, _ string, _ error) {
		probes = append(probes, path)
	})
	if err != nil || selected.Path != first || selected.Version != "" {
		t.Fatalf("without opt-in the original PATH selection must remain untouched: %+v, %v", selected, err)
	}
	if len(probes) != 0 {
		t.Fatalf("without a managed root resolving must not run version probes: %v", probes)
	}
	for _, dir := range []string{firstDir, secondDir} {
		if _, err := os.Stat(filepath.Join(dir, "probed")); !os.IsNotExist(err) {
			t.Fatalf("PATH executable was run during ordinary resolution: %s (%v)", dir, err)
		}
	}
}

func TestSpecManagedRootLimitsCodexCandidatesToManagedAndExplicitPATH(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	pathDir := filepath.Join(root, "restricted-path")
	managedPath := specCodexExecutable(t, filepath.Join(managed, "bin"), "0.0.2")
	path := specCodexExecutable(t, pathDir, "0.0.1")
	t.Setenv("SM_CLI_INSTALL_ROOT", "")
	t.Setenv("PATH", pathDir)
	var mu sync.Mutex
	var probes []string
	selected, err := clibin.Resolve(context.Background(), "codex", managed, func(path, _ string, _ error) {
		mu.Lock()
		defer mu.Unlock()
		probes = append(probes, path)
	})
	if err != nil || selected.Path != managedPath || selected.Version != "0.0.2" {
		t.Fatalf("managed selection escaped the explicitly supplied candidates: %+v, %v", selected, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(probes) != 2 {
		t.Fatalf("expected both supplied candidates to be checked: %v", probes)
	}
	for _, probe := range probes {
		if probe != managedPath && probe != path {
			t.Errorf("resolver probed a path outside the caller's installation scope: %s", probe)
		}
	}
}

func TestSpecEnvironmentRootOptsIntoSelectionAndExplicitRootTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	envRoot := filepath.Join(root, "environment")
	explicitRoot := filepath.Join(root, "explicit")
	pathDir := filepath.Join(root, "path")
	envPath := specCodexExecutable(t, filepath.Join(envRoot, "bin"), "3.0.0")
	explicitPath := specCodexExecutable(t, filepath.Join(explicitRoot, "bin"), "2.0.0")
	specCodexExecutable(t, pathDir, "1.0.0")
	t.Setenv("SM_CLI_INSTALL_ROOT", envRoot)
	t.Setenv("PATH", pathDir)
	selected, err := clibin.Resolve(context.Background(), "codex", "", nil)
	if err != nil || selected.Path != envPath {
		t.Fatalf("the caller's environment root did not opt into managed selection: %+v, %v", selected, err)
	}
	selected, err = clibin.Resolve(context.Background(), "codex", explicitRoot, nil)
	if err != nil || selected.Path != explicitPath {
		t.Fatalf("explicit installation root did not override the environment root: %+v, %v", selected, err)
	}
}

func specCodexExecutable(t *testing.T, directory, version string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "codex")
	script := "#!/bin/sh\nprintf 'probed' > '" + filepath.Join(directory, "probed") + "'\nprintf 'codex-cli " + version + "\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
