package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func addCodexSessionObserver(args []string, command string) ([]string, error) {
	hookConfig, end, err := codexHookCLIConfig(args)
	if err != nil {
		return args, err
	}
	groups, _ := hookConfig["SessionStart"].([]any)
	group := map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": command, "timeout": codexObserverTimeout,
	}}}
	key := codexSessionObserverKey(len(groups))
	groups = append(groups, group)

	states, _ := hookConfig["state"].(map[string]any)
	if states == nil {
		states = make(map[string]any)
	}
	states[key] = map[string]any{"trusted_hash": codexSessionObserverHash(command)}

	// Codex evaluates trust by the config layer, event, group and handler index.
	// Appending the observer preserves native trust entries for existing hooks.
	var overrides bytes.Buffer
	encoder := toml.NewEncoder(&overrides).SetTablesInline(true)
	if err := encoder.Encode(map[string]any{"hooks.SessionStart": groups, "hooks.state": states}); err != nil {
		return args, err
	}
	updated := append([]string(nil), args[:end]...)
	for line := range strings.SplitSeq(strings.TrimSpace(overrides.String()), "\n") {
		key, value, _ := strings.Cut(line, " = ")
		updated = append(updated, "-c", strings.Trim(key, "'\"")+"="+value)
	}
	return append(updated, args[end:]...), nil
}

func codexHookCLIConfig(args []string) (map[string]any, int, error) {
	layer := make(map[string]any)
	end := len(args)
	for index := 0; index < end; index++ {
		arg := args[index]
		if arg == "--" {
			end = index
			break
		}
		option := ""
		switch {
		case (arg == "-c" || arg == "--config") && index+1 < end:
			index++
			option = args[index]
		case strings.HasPrefix(arg, "-c="):
			option = strings.TrimPrefix(arg, "-c=")
		case strings.HasPrefix(arg, "--config="):
			option = strings.TrimPrefix(arg, "--config=")
		}
		key, value, ok := strings.Cut(option, "=")
		key = strings.TrimSpace(key)
		if !ok || (key != "hooks" && !strings.HasPrefix(key, "hooks.")) {
			continue
		}
		var parsed map[string]any
		if err := toml.Unmarshal([]byte("value = "+strings.TrimSpace(value)), &parsed); err != nil {
			return nil, end, fmt.Errorf("read Codex %s CLI override: %w", key, err)
		}
		// Codex splits override keys at literal dots.
		parts := strings.Split(key, ".")
		current := layer
		for _, part := range parts[:len(parts)-1] {
			child, _ := current[part].(map[string]any)
			if child == nil {
				child = make(map[string]any)
				current[part] = child
			}
			current = child
		}
		current[parts[len(parts)-1]] = parsed["value"]
	}
	hooks, _ := layer["hooks"].(map[string]any)
	return hooks, end, nil
}

const codexObserverTimeout = 3

func codexSessionObserverKey(group int) string {
	path := "/<session-flags>/config.toml"
	if runtime.GOOS == "windows" {
		path = `C:\<session-flags>\config.toml`
	}
	return fmt.Sprintf("%s:session_start:%d:0", path, group)
}

func codexSessionObserverHash(command string) string {
	// Codex hashes sorted JSON from the normalized TOML hook declaration.
	identity := map[string]any{
		"event_name": "session_start",
		"hooks": []any{map[string]any{
			"async": false, "command": command, "timeout": codexObserverTimeout, "type": "command",
		}},
	}
	var jsonData bytes.Buffer
	encoder := json.NewEncoder(&jsonData)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(identity)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(bytes.TrimSuffix(jsonData.Bytes(), []byte("\n"))))
}
