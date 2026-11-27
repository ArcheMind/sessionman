package app

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArcheMind/sessionman/internal/runtime"
)

func TestSessionObserverModulesFollowNativeSessionChanges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	root := t.TempDir()
	report := filepath.Join(root, "session-id")
	for name, source := range map[string]string{"pi": piSessionObserver, "opencode": openCodeSessionObserver, "tui": openCodeTUIObserver} {
		if err := os.WriteFile(filepath.Join(root, name+".mjs"), []byte(sessionObserverModule(report)+source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
const root = ` + jsonString(root) + `;
const read = () => readFileSync(` + jsonString(report) + `, 'utf8');
const load = name => import(pathToFileURL(root + '/' + name + '.mjs').href);
const { default: pi } = await load('pi');
let started;
pi({ on: (event, callback) => { assert.equal(event, 'session_start'); started = callback; } });
for (const id of ['initial', 'switched']) {
  started({}, { sessionManager: { getSessionId: () => id } });
}
assert.equal(read(), 'switched');
const { SessionMan } = await load('opencode');
const pending = new Map();
const hooks = await SessionMan({ client: { session: { get: ({ path }) => new Promise(resolve => pending.set(path.id, resolve)) } } });
await hooks.event({ event: { type: 'session.created', properties: { info: { id: 'parent' } } } });
await hooks.event({ event: { type: 'session.created', properties: { info: { id: 'child', parentID: 'parent' } } } });
assert.equal(read(), 'parent');
const older = hooks['chat.message']({ sessionID: 'older' });
const newer = hooks['chat.message']({ sessionID: 'newer' });
pending.get('newer')({ data: { id: 'newer' } });
await newer;
pending.get('older')({ data: { id: 'older' } });
await older;
assert.equal(read(), 'newer');
const { default: tui } = await load('tui');
let dispose;
const api = {
  route: { current: { name: 'session', params: { sessionID: 'initial' } } },
  state: { session: { get: id => ({ id }) } },
  lifecycle: { onDispose: callback => { dispose = callback; } },
};
await tui.tui(api);
api.route.current.params.sessionID = 'navigated-without-message';
await dispose();
assert.equal(read(), 'navigated-without-message');
api.route.current = { name: 'home' };
await dispose();
assert.equal(read(), '');
`
	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native observer module behavior: %v\n%s", err, output)
	}
}

func TestOpenCodeTUIObserverPreservesExplicitConfigAndRelativePaths(t *testing.T) {
	root, temporary := t.TempDir(), t.TempDir()
	original := filepath.Join(root, "custom-tui.jsonc")
	content := `{"theme":"custom", "plugin":["./custom-plugin.js"], "custom":{"value":"{file:./theme.txt}"},}`
	if err := os.WriteFile(original, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	plan := runtime.CommandPlan{Env: map[string]string{"OPENCODE_TUI_CONFIG": original}}
	if err := prepareOpenCodeTUIObserver(&plan, temporary); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plan.Env["OPENCODE_TUI_CONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Theme  string   `json:"theme"`
		Plugin []string `json:"plugin"`
		Custom struct {
			Value string `json:"value"`
		} `json:"custom"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	plugin, err := url.Parse(config.Plugin[0])
	if err != nil || plugin.Path != filepath.Join(root, "custom-plugin.js") || len(config.Plugin) != 2 || config.Theme != "custom" || config.Custom.Value != "{file:"+filepath.ToSlash(filepath.Join(root, "theme.txt"))+"}" {
		t.Fatalf("explicit TUI config changed meaning: %+v, %v", config, err)
	}
	data, err = os.ReadFile(original)
	if err != nil || string(data) != content {
		t.Fatalf("source config modified: %q, %v", data, err)
	}
}
