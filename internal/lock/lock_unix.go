//go:build unix

// Package lock provides an advisory exclusive lock so overlapping cron runs
// can never write to the same archive concurrently.
package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("another backup run is active")

// Lock is a held lock file.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive non-blocking lock on path.
// The file stays in place; the lock is tied to the open file description
// and is released on Release or process exit (even on crash, by the OS).
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f, path: path}, nil
}

// Release unlocks.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
