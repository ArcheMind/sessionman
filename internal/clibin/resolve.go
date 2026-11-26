package clibin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Candidate struct{ Path, Version string }
type ProbeLog func(path, output string, err error)

var selections = struct {
	sync.Mutex
	items map[string]Candidate
}{items: map[string]Candidate{}}
var logMutex sync.Mutex

func Probe(ctx context.Context, path string, log ProbeLog) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = 250 * time.Millisecond
	output, err := cmd.CombinedOutput()
	if log != nil {
		logMutex.Lock()
		log(path, string(output), err)
		logMutex.Unlock()
	}
	if err != nil {
		return "", fmt.Errorf("probe %s: %w", path, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func Select(ctx context.Context, paths []string, probe func(context.Context, string) (string, error)) (path, version string, err error) {
	type result struct {
		version string
		err     error
	}
	results := make([]result, len(paths))
	var wg sync.WaitGroup
	for i, candidate := range paths {
		wg.Add(1)
		go func(i int, candidate string) {
			defer wg.Done()
			value, err := probe(ctx, candidate)
			if err == nil {
				value, err = StableVersion(value)
			}
			results[i] = result{value, err}
		}(i, candidate)
	}
	wg.Wait()
	for i, result := range results {
		if result.err != nil {
			continue
		}
		cmp := 1
		if version != "" {
			cmp, _ = CompareVersions(result.version, version)
		}
		if cmp > 0 {
			path, version = paths[i], result.version
		}
	}
	if path == "" {
		return "", "", fmt.Errorf("no installed stable CLI version could be verified")
	}
	return path, version, nil
}

func ManagedRoot(root string) string {
	if root == "" {
		root = os.Getenv("SM_CLI_INSTALL_ROOT")
	}
	if root != "" {
		root, _ = filepath.Abs(root)
	}
	return root
}

func LookPath(agent string) (string, error) {
	candidate, err := Resolve(context.Background(), agent, "", nil)
	return candidate.Path, err
}

func Resolve(ctx context.Context, agent, root string, log ProbeLog) (Candidate, error) {
	root = ManagedRoot(root)
	if root == "" {
		path, err := exec.LookPath(agent)
		return Candidate{Path: path}, err
	}
	paths := candidates(agent, root)
	if len(paths) == 0 {
		return Candidate{}, exec.ErrNotFound
	}
	if agent != "codex" {
		return Candidate{Path: paths[0]}, nil
	}
	var key strings.Builder
	key.WriteString(root + "\x00" + os.Getenv("PATH"))
	for _, path := range paths {
		info, _ := os.Stat(path)
		real, _ := filepath.EvalSymlinks(path)
		fmt.Fprintf(&key, "\x00%s:%s", path, real)
		if info != nil {
			fmt.Fprintf(&key, ":%d:%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	selections.Lock()
	cached, ok := selections.items[key.String()]
	selections.Unlock()
	if ok {
		return cached, nil
	}
	type observed struct {
		raw string
		err error
	}
	observations := map[string]observed{}
	var observationLock sync.Mutex
	probe := func(ctx context.Context, path string) (string, error) {
		raw, err := Probe(ctx, path, log)
		observationLock.Lock()
		observations[path] = observed{raw, err}
		observationLock.Unlock()
		return raw, err
	}
	path, version, err := Select(ctx, paths, probe)
	if err != nil {
		// An opaque custom executable remains usable, but a known prerelease is never selected automatically.
		for _, candidate := range paths {
			result := observations[candidate]
			raw := result.raw
			if result.err != nil {
				continue
			}
			match := versionPattern.FindStringSubmatch(raw)
			if len(match) == 2 && strings.Contains(strings.SplitN(match[1], "+", 2)[0], "-") {
				continue
			}
			return Candidate{Path: candidate, Version: raw}, nil
		}
		return Candidate{}, err
	}
	selected := Candidate{Path: path, Version: version}
	selections.Lock()
	selections.items[key.String()] = selected
	selections.Unlock()
	return selected, nil
}

func candidates(agent, root string) []string {
	name := agent
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var paths []string
	seen := map[string]bool{}
	add := func(path string) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0) {
			return
		}
		path, _ = filepath.Abs(path)
		real, err := filepath.EvalSymlinks(path)
		if err != nil || seen[real] {
			return
		}
		seen[real] = true
		paths = append(paths, path)
	}
	if root != "" {
		add(filepath.Join(root, "bin", name))
	}
	if agent != "codex" {
		if path, err := exec.LookPath(agent); err == nil {
			add(path)
		}
		return paths
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			add(filepath.Join(dir, name))
		}
	}
	return paths
}
