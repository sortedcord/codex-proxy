// Package storage provides private atomic files and process lifetime locks.
package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// CommitError means rename succeeded but directory durability could not be confirmed.
// Callers must reflect the replacement in memory, not pretend the old file survived.
type CommitError struct{ Err error }

func (e *CommitError) Error() string { return "file replaced but durability is uncertain" }
func (e *CommitError) Unwrap() error { return e.Err }

func InspectFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("storage path must be a regular file, not a symlink")
	}
	return nil
}

func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("storage directory must be a directory, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return errors.New("storage directory must be private (chmod 700)")
	}
	return nil
}

func AtomicWrite(path string, data []byte) error {
	if err := InspectFile(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".codex-proxy-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := InspectFile(path); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return &CommitError{Err: err}
	}
	return nil
}

// Lock files are never unlinked: replacing a locked inode would admit a second owner.
type Lock struct{ file *os.File }

func Acquire(path string) (*Lock, error) {
	if err := InspectFile(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, errors.New("storage is already owned by another proxy or locking is unavailable")
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error { return l.file.Close() }
