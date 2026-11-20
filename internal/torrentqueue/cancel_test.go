package torrentqueue

// PR-2: cancel is a distinct, terminal stop. It is not a pause (a restart does
// not resume it), it is not remove (payload and workspace stay), and only an
// explicit retry makes the entry runnable again.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueCancelIsTerminalAndRetryReopens(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(1))
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}

	cancelled, err := store.Cancel(entry.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Desired != DesiredCancelled || cancelled.Phase != PhaseCancelled {
		t.Fatalf("cancel = desired %q phase %q", cancelled.Desired, cancelled.Phase)
	}
	if _, err := store.Start(entry.ID); !errors.Is(err, ErrEntryCancelled) {
		t.Fatalf("start on cancelled = %v, want ErrEntryCancelled", err)
	}
	still, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Desired != DesiredCancelled || still.Phase != PhaseCancelled {
		t.Fatalf("a refused start changed the entry: %+v", still)
	}

	retried, err := store.Retry(entry.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.Desired != DesiredRun || retried.Phase != PhaseQueued {
		t.Fatalf("retry = desired %q phase %q", retried.Desired, retried.Phase)
	}

	completed := writeCommittedCapture(t, store, testJobID(2), map[string][]byte{"fixture/a.bin": []byte("done")})
	if _, err := store.Cancel(completed.ID); !errors.Is(err, ErrEntryCompleted) {
		t.Fatalf("cancel on completed = %v, want ErrEntryCompleted", err)
	}
}

func TestQueueCancelRunningStopsOnlyThatAttemptAndKeepsPayload(t *testing.T) {
	store := newTestStore(t)
	sibling := addEntry(t, store, testJobID(3), "fixture/b.bin")
	entry := addEntry(t, store, testJobID(4))
	for _, id := range []string{sibling.ID, entry.ID} {
		if _, err := store.Update(id, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
			t.Fatal(err)
		}
	}

	cancelled := make(chan struct{})
	fake := newFakeTransfer()
	var blockedOnce atomic.Bool
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		if req.KnownJobID == entry.JobID && blockedOnce.CompareAndSwap(false, true) {
			<-ctx.Done()
			close(cancelled)
			return Outcome{}, ctx.Err()
		}
		return successOutcome(req), nil
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()
	started := make(chan struct{})
	go func() {
		for req := range fake.entered {
			if req.KnownJobID == entry.JobID {
				close(started)
				return
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never started the entry to cancel")
	}

	if _, err := store.Cancel(entry.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker was never cancelled by the cancel request")
	}
	sum := <-done

	final, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseCancelled || final.Desired != DesiredCancelled {
		t.Fatalf("cancelled entry = desired %q phase %q (%s)", final.Desired, final.Phase, final.Note)
	}
	if final.Capture != nil {
		t.Fatalf("a cancelled entry recorded a capture: %+v", final.Capture)
	}
	if sum.Cancelled != 1 {
		t.Fatalf("summary = %+v, want one cancelled", sum)
	}
	// The sibling is untouched: cancel is per attempt, not a queue-wide stop.
	sib, err := store.Get(sibling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sib.Desired != DesiredRun || sib.Phase == PhaseCancelled {
		t.Fatalf("the sibling was affected by cancel: %+v", sib)
	}

	// A cancelled entry is not claimed by a restart.
	before := fake.runCount()
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.runCount() != before {
		t.Fatal("a cancelled entry was claimed without an explicit retry")
	}

	// Retry makes it runnable again and it completes from a normal attempt.
	if _, err := store.Retry(entry.ID); err != nil {
		t.Fatal(err)
	}
	sum, err = runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain after retry: %v", err)
	}
	if sum.Completed < 1 {
		t.Fatalf("retry summary = %+v, want a completed entry", sum)
	}
	retried, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Phase != PhaseCompleted || retried.Attempts != 2 {
		t.Fatalf("retried entry = phase %q attempts %d", retried.Phase, retried.Attempts)
	}
}

func TestQueueReconcileCancelledObservation(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(5))
	if _, err := store.Update(entry.ID, func(e *Entry) error {
		e.Desired = DesiredCancelled
		e.Phase = PhaseRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeTransfer()
	if _, err := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fake.runCount() != 0 {
		t.Fatalf("a cancelled interrupted entry ran %d transfers", fake.runCount())
	}
	got, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseCancelled || got.Desired != DesiredCancelled {
		t.Fatalf("reconciled entry = desired %q phase %q", got.Desired, got.Phase)
	}
}
