//go:build !windows

package agentupdate

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

const updatesSupported = true

func withLock(path string, action func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("another update is already running: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return action()
}
