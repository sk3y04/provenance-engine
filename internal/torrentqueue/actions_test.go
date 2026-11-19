package torrentqueue

// BT-10d: the durable operations the CLI and the TUI share.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func actionEntry(t *testing.T, store *Store, phase Phase, desired Desired) Entry {
	t.Helper()
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(16<<10, 163)}, 0)
	e := enqueue(t, store, seed, "alpha/a.bin")
	if _, uerr := store.Update(e.ID, func(x *Entry) error {
		x.Phase = phase
		x.Desired = desired
		if phase == PhaseFailed {
			x.LastError = "previous failure"
			x.Note = "note"
			x.NotBefore = time.Now().Add(time.Hour)
		}
		return nil
	}); uerr != nil {
		t.Fatalf("seed state: %v", uerr)
	}
	return mustGet(t, store, e.ID)
}

func TestQueueStartSavesIntentAndRefusesFailures(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	paused := actionEntry(t, store, PhasePaused, DesiredPaused)
	after, serr := store.Start(paused.ID)
	if serr != nil {
		t.Fatalf("start: %v", serr)
	}
	if after.Desired != DesiredRun || after.Phase != PhaseQueued {
		t.Fatalf("start = desired %q phase %q", after.Desired, after.Phase)
	}
	if RunnerHeld(store.Root()) {
		t.Fatal("start acquired runner ownership")
	}

	failed := actionEntry(t, store, PhaseFailed, DesiredRun)
	if _, serr := store.Start(failed.ID); !errors.Is(serr, ErrEntryFailed) {
		t.Fatalf("start on failed = %v, want ErrEntryFailed", serr)
	}
	if got := mustGet(t, store, failed.ID); got.Phase != PhaseFailed {
		t.Fatalf("a refused start changed the entry: %+v", got)
	}
}

func TestQueuePauseRecordsIntentOnly(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	running := actionEntry(t, store, PhaseRunning, DesiredRun)
	after, perr := store.Pause(running.ID)
	if perr != nil {
		t.Fatalf("pause: %v", perr)
	}
	if after.Desired != DesiredPaused {
		t.Fatalf("pause desired = %q", after.Desired)
	}
	// A running attempt is not rewritten: the runner reports stopping until it closes.
	if after.Phase != PhaseRunning {
		t.Fatalf("pause changed the observed phase to %q", after.Phase)
	}
	queued := actionEntry(t, store, PhaseQueued, DesiredRun)
	afterQueued, _ := store.Pause(queued.ID)
	if afterQueued.Phase != PhasePaused {
		t.Fatalf("pausing a queued entry left phase %q", afterQueued.Phase)
	}
}

func TestQueueRetryClearsFailureNotJustStarts(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	failed := actionEntry(t, store, PhaseFailed, DesiredRun)
	after, rerr := store.Retry(failed.ID)
	if rerr != nil {
		t.Fatalf("retry: %v", rerr)
	}
	if after.Desired != DesiredRun || after.Phase != PhaseQueued {
		t.Fatalf("retry = desired %q phase %q", after.Desired, after.Phase)
	}
	if after.LastError != "" || after.Note != "" || !after.NotBefore.IsZero() {
		t.Fatalf("retry left failure state: err=%q note=%q notBefore=%v", after.LastError, after.Note, after.NotBefore)
	}
	completed := actionEntry(t, store, PhaseCompleted, DesiredRun)
	if _, rerr := store.Retry(completed.ID); !errors.Is(rerr, ErrEntryCompleted) {
		t.Fatalf("retry on completed = %v, want ErrEntryCompleted", rerr)
	}
	if !strings.Contains(ErrEntryCompleted.Error(), "capture") {
		t.Fatalf("unhelpful error text: %v", ErrEntryCompleted)
	}
}
