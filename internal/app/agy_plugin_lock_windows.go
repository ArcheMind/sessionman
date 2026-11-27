//go:build windows

package app

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockAgyPluginFile(f *os.File) error {
	var overlap windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlap)
}

func unlockAgyPluginFile(f *os.File) {
	var overlap windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlap)
}
