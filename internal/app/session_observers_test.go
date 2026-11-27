package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionObserverModulesFollowNativeSessionChanges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	root := t.TempDir()
	report := filepath.Join(root, "session-id")
	if err := os.WriteFile(filepath.Join(root, "pi.mjs"), []byte(sessionObserverModule(report)+piSessionObserver), 0600); err != nil {
		t.Fatal(err)
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
`
	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native observer module behavior: %v\n%s", err, output)
	}
}
