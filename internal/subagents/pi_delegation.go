package subagents

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed agentx_bridge.ts
var piDelegationBridge []byte

// PiBridgePath is the stable path of the delegation bridge extension under home.
func PiBridgePath(home string) string {
	return filepath.Join(home, ".local", "state", "sessionman", "extensions", "sessionman-bridge.ts")
}

// WritePiBridge writes the delegation bridge extension to path.
func WritePiBridge(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating bridge directory: %w", err)
	}
	if err := os.WriteFile(path, piDelegationBridge, 0o600); err != nil {
		return fmt.Errorf("writing delegation bridge: %w", err)
	}
	return nil
}
