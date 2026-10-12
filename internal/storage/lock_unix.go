//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package storage

import (
	"os"
	"syscall"
)

func lockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }() // Read-only descriptor; Sync's error is returned below.
	return dir.Sync()
}
