//go:build windows

package lfsgc

// repoLock is a no-op on Windows, which does not provide flock.
type repoLock struct{}

// acquireLock returns without taking a lock on Windows. Callers that need
// mutual exclusion there must serialize runs externally.
func acquireLock(_ string) (func() error, error) {
	return (&repoLock{}).release, nil
}

func (*repoLock) release() error { return nil }
