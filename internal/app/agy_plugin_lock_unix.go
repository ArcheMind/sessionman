//go:build !windows

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockAgyPluginFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func unlockAgyPluginFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
