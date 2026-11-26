//go:build windows

package agentupdate

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

const updatesSupported = false

func withLock(path string, action func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	var overlap windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap); err != nil {
		return fmt.Errorf("another update is already running: %w", err)
	}
	defer windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlap)
	return action()
}
