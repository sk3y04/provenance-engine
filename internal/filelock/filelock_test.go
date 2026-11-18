package filelock

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseAndSidecarStability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	l, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// The sidecar must remain: deleting it would race with other lockers.
	if _, err := Acquire(context.Background(), path); err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
}

func TestSecondAcquireWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	first, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan *Lock, 1)
	errs := make(chan error, 1)
	go func() {
		l, err := Acquire(context.Background(), path)
		if err != nil {
			errs <- err
			return
		}
		got <- l
	}()

	select {
	case <-got:
		t.Fatal("second Acquire succeeded while the lock was held")
	case err := <-errs:
		t.Fatalf("second Acquire errored: %v", err)
	case <-time.After(100 * time.Millisecond):
		// expected: still blocked
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-got:
		_ = l.Release()
	case err := <-errs:
		t.Fatalf("second Acquire after release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never succeeded after release")
	}
}

func TestAcquireRespectsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	held, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); err == nil {
		t.Fatal("Acquire succeeded despite contention and cancelled context")
	}
}
