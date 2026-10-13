package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicPrivateFileAndLifetimeLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private.json")
	if err := AtomicWrite(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "two" {
		t.Fatal("replacement failed")
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("file is not private")
	}
	lock, err := Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path + ".lock"); err == nil {
		t.Fatal("second owner acquired lock")
	}
	_ = lock.Close()
	lock, err = Acquire(path + ".lock")
	if err != nil {
		t.Fatal("lock was not released", err)
	}
	_ = lock.Close()
}

func TestStorageRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := AtomicWrite(path, []byte("bad")); err == nil {
		t.Fatal("symlink was replaced")
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("symlink lock accepted")
	}
	body, _ := os.ReadFile(target)
	if string(body) != "private" {
		t.Fatal("symlink target changed")
	}
}
