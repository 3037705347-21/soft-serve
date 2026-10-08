//go:build !windows

package lfsgc

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// repoLock prevents concurrent garbage collection runs against the same
// repository, including runs started from separate processes while the server
// is running.
type repoLock struct {
	f *os.File
}

// acquireLock takes a non-blocking exclusive lock on <lfsRoot>/.gc-lock.
// It returns ErrAlreadyLocked if another run already holds it.
func acquireLock(lfsRoot string) (func() error, error) {
	if err := os.MkdirAll(lfsRoot, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(lfsRoot, ".gc-lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, ErrAlreadyLocked
		}
		return nil, err
	}
	l := &repoLock{f: f}
	return l.release, nil
}

func (l *repoLock) release() error {
	defer func() { _ = l.f.Close() }()
	return unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
}
