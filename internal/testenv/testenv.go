// Package testenv makes package tests hermetic with respect to native
// agent directories: a temporary HOME and none of the caller's sm or agent
// directory env vars.
package testenv

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ArcheMind/agentx/internal/nativedirs"
)

// Isolate points HOME at a fresh temporary directory and clears every
// inherited directory env var; the returned func removes the directory.
func Isolate() (func(), error) {
	home, err := os.MkdirTemp("", "sm-test-home-")
	if err != nil {
		return nil, fmt.Errorf("create test home: %w", err)
	}
	os.Setenv("HOME", home)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "SM_NATIVE_") {
			os.Unsetenv(name)
		}
	}
	for _, name := range nativedirs.IsolatedEnvNames() {
		os.Unsetenv(name)
	}
	return func() { os.RemoveAll(home) }, nil
}

// Run isolates the environment, runs the package tests and returns the exit code.
func Run(m *testing.M) int {
	cleanup, err := Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()
	return m.Run()
}
