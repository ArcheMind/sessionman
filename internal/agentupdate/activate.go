package agentupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func activate(root, agent, executable string) (func() error, error) {
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		return nil, err
	}
	name := agent
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(bin, name)
	previous, err := os.Readlink(target)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("managed CLI path is not a symlink: %w", err)
	}
	if err := replaceLink(target, executable); err != nil {
		return nil, err
	}
	return func() error {
		if previous == "" {
			return os.Remove(target)
		}
		return replaceLink(target, previous)
	}, nil
}

func replaceLink(target, value string) error {
	f, err := os.CreateTemp(filepath.Dir(target), ".activate-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Close()
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	if err := os.Symlink(value, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}
