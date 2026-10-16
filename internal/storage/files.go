// Package storage provides private atomic files and process lifetime locks.
package storage

import (
	"github.com/terastal-hq/proxylib/privatefile"
)

// MaxHistorySamples is the default for the deprecated, ignored history-max-samples flag.
const MaxHistorySamples = 10000

// CommitError means rename succeeded but directory durability could not be confirmed.
type CommitError = privatefile.CommitError

var (
	InspectFile = privatefile.InspectFile
	PrivateDir  = privatefile.EnsurePrivateDir
	AtomicWrite = privatefile.AtomicWrite
)

// Lock represents an advisory exclusive file lock.
type Lock = privatefile.Lock

// Acquire opens and locks the target path exclusively with non-blocking mode.
func Acquire(path string) (*Lock, error) {
	return privatefile.AcquireLock(path)
}
