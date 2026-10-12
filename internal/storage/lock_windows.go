package storage

import (
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := lockFileEx.Call(
		file.Fd(),
		3, // LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result == 0 {
		return err
	}
	return nil
}

// Go cannot sync Windows directories. File contents are synced before rename;
// POSIX adds directory durability. Windows deployments must protect files by ACL.
func syncDirectory(string) error { return nil }
