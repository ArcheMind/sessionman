package agentupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	platform "runtime"
	"strings"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

var packages = map[string]string{"claude": "@anthropic-ai/claude-code", "codex": "@openai/codex", "pi": "@earendil-works/pi-coding-agent"}

func (s Service) latest(ctx context.Context, agent string) (Release, error) {
	if name := packages[agent]; name != "" {
		data, err := s.fetch(ctx, "https://registry.npmjs.org/"+name+"/latest", 4<<20, true)
		if err != nil {
			return Release{}, err
		}
		var release Release
		if err := json.Unmarshal(data, &release); err != nil {
			return Release{}, err
		}
		return release, nil
	}
	platformID := platform.GOOS + "_" + platform.GOARCH
	if platform.GOOS == "linux" {
		matches, _ := filepath.Glob("/lib/ld-musl-*.so.1")
		if len(matches) > 0 {
			platformID += "_musl"
		}
	}
	data, err := s.fetch(ctx, "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/"+platformID+".json", 1<<20, true)
	if err != nil {
		return Release{}, err
	}
	var release Release
	if err := json.Unmarshal(data, &release); err != nil {
		return Release{}, err
	}
	parsed, err := url.Parse(release.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return Release{}, fmt.Errorf("invalid AGY release URL")
	}
	if checksum, err := hex.DecodeString(release.SHA512); err != nil || len(checksum) != sha512.Size {
		return Release{}, fmt.Errorf("invalid AGY release SHA512")
	}
	return release, nil
}

func (s Service) fetch(ctx context.Context, endpoint string, limit int64, text bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SessionMan-agent-updater")
	response, err := s.HTTPClient.Do(req)
	if err != nil {
		s.logHTTP(endpoint, nil, 0, text, err)
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("release response exceeds size limit")
	}
	if err == nil && response.StatusCode != http.StatusOK {
		err = fmt.Errorf("release request returned HTTP %d", response.StatusCode)
	}
	s.logHTTP(endpoint, data, response.StatusCode, text, err)
	return data, err
}

func (s Service) stage(ctx context.Context, agent string, release Release, dir string) (string, error) {
	if name := packages[agent]; name != "" {
		plan := runtime.CommandPlan{Executable: "npm", Args: []string{"install", "--global", "--prefix", dir, "--registry=https://registry.npmjs.org", "--engine-strict", "--no-audit", "--no-fund", name + "@" + release.Version}, Env: map[string]string{"NPM_CONFIG_UPDATE_NOTIFIER": "false"}}
		result, err := s.Runner.Execute(ctx, plan, runtime.ExecuteOptions{CancelProcessGroup: true})
		if err != nil {
			return "", fmt.Errorf("install %s: %w: %s", agent, err, runtime.RedactSecrets(result.Stderr))
		}
		if platform.GOOS == "windows" {
			return filepath.Join(dir, agent+".cmd"), nil
		}
		return filepath.Join(dir, "bin", agent), nil
	}
	data, err := s.fetch(ctx, release.URL, 256<<20, false)
	if err != nil {
		return "", err
	}
	checksum := sha512.Sum512(data)
	if !strings.EqualFold(hex.EncodeToString(checksum[:]), release.SHA512) {
		return "", fmt.Errorf("AGY release SHA512 mismatch")
	}
	if strings.HasSuffix(strings.SplitN(release.URL, "?", 2)[0], ".tar.gz") {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		defer gz.Close()
		tarReader := tar.NewReader(gz)
		found := false
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			if header.Name != "antigravity" && header.Name != "./antigravity" {
				continue
			}
			if header.Typeflag != tar.TypeReg || header.Size > 256<<20 {
				return "", fmt.Errorf("invalid AGY archive executable")
			}
			data, err = io.ReadAll(io.LimitReader(tarReader, (256<<20)+1))
			if err != nil {
				return "", err
			}
			found = true
			break
		}
		if !found {
			return "", fmt.Errorf("AGY archive has no antigravity executable")
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		return "", err
	}
	name := "agy"
	if platform.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	return path, os.WriteFile(path, data, 0755)
}

func (s Service) probeLog(path, output string, err error) {
	if !s.Debug {
		return
	}
	entry := map[string]any{"event": "external_call", "input": map[string]any{"executable": path, "args": []string{"--version"}}, "output": runtime.RedactSecrets(output)}
	if err != nil {
		entry["error"] = runtime.RedactSecrets(err.Error())
	}
	data, _ := json.Marshal(entry)
	fmt.Fprintln(s.Log, string(data))
}

func (s Service) logHTTP(endpoint string, data []byte, status int, isText bool, err error) {
	if !s.Debug {
		return
	}
	entry := map[string]any{"event": "agent_release_http", "input": endpoint, "status": status}
	if isText {
		entry["output"] = runtime.RedactSecrets(string(data))
	} else {
		sum := sha512.Sum512(data)
		entry["output"] = map[string]any{"bytes": len(data), "sha512": hex.EncodeToString(sum[:])}
	}
	if err != nil {
		entry["error"] = runtime.RedactSecrets(err.Error())
	}
	encoded, _ := json.Marshal(entry)
	fmt.Fprintln(s.Log, string(encoded))
}
