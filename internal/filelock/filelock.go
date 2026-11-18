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
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Lock is a held exclusive lock. Release must be called (typically deferred).
type Lock struct {
	f *os.File
}

// Acquire blocks until the exclusive lock for path is held or ctx is done.
// Contention is polled with a short non-blocking retry so cancellation stays
// responsive without leaking goroutines.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
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
