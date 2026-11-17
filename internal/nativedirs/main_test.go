package nativedirs

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// testenv imports this package, so the same isolation is inlined here.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "sm-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, nativePrefix) {
			os.Unsetenv(name)
		}
	}
	for _, name := range IsolatedEnvNames() {
		os.Unsetenv(name)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
