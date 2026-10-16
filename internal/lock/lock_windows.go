//go:build windows

package lock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("another backup run is active")

// Lock is a held lock file.
type Lock struct {
	path string
}

// Acquire uses exclusive file creation. A lock file older than 3 hours is
// considered stale (crashed run) and replaced.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Sync()
		f.Close()
		return &Lock{path: path}, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}
	fi, statErr := os.Stat(path)
	if statErr == nil && time.Since(fi.ModTime()) > 3*time.Hour {
		_ = os.Remove(path)
		return Acquire(path)
	}
	return nil, ErrLocked
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return os.Remove(l.path)
}
