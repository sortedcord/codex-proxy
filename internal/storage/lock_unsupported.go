//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package storage

import (
	"errors"
	"os"
)

func lockFile(*os.File) error    { return errors.New("storage locking is unsupported on this platform") }
func syncDirectory(string) error { return errors.New("directory sync is unsupported on this platform") }
