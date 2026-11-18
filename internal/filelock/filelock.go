// Package filelock provides advisory cross-process exclusive locks keyed by
// a stable sidecar file (path + ".lock"). It serializes read-modify-write
// cycles on stores whose data files are replaced by atomic rename: locking
// the data file itself would not work, because rename swaps the inode.
//
// The sidecar is never deleted on release — removing it would race with
// other processes about to lock the same path.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked reports that the lock is already held and TryAcquire did not wait
// for it. It is not a failure of the locking machinery: the caller can retry
// later.
var ErrLocked = errors.New("lock is held by another owner")

// Lock is a held exclusive lock. Release must be called (typically deferred).
type Lock struct {
	f *os.File
}

// openLockFile creates/opens the sidecar. The sidecar is stable across
// acquisitions and is never removed, so two processes always contend on the
// same inode regardless of how they spelled the path.
func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return f, nil
}

// TryAcquire takes the exclusive lock without waiting. It returns ErrLocked
// when another owner holds it, which lets a caller fail fast with a retryable
// "busy" error instead of blocking.
func TryAcquire(path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if isWouldBlock(err) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Acquire blocks until the exclusive lock for path is held or ctx is done.
// Contention is polled with a short non-blocking retry so cancellation stays
// responsive without leaking goroutines.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	for {
		err := tryLock(f)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Release unlocks and closes the sidecar handle.
func (l *Lock) Release() error {
	err := unlock(l.f)
	cerr := l.f.Close()
	if err != nil {
		return err
	}
	return cerr
}
