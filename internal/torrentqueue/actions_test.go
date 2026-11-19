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

// TestQueueSeedIntentParity: enable/disable are the CLI's operations, with the same
// eligibility and the same effect on a serving entry.
func TestQueueSeedIntentParity(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	incomplete := actionEntry(t, store, PhaseQueued, DesiredRun)
	if _, serr := store.EnableSeed(incomplete.ID); !errors.Is(serr, ErrUsage) {
		t.Fatalf("enable on an incomplete entry = %v, want ErrUsage", serr)
	}
	if got := mustGet(t, store, incomplete.ID); got.SeedWanted() {
		t.Fatalf("a refused enable changed the entry: %+v", got)
	}

	done := actionEntry(t, store, PhaseCompleted, DesiredRun)
	if _, uerr := store.Update(done.ID, func(e *Entry) error {
		e.Capture = &CaptureRef{Key: "capture-fixture"}
		e.Workspace = ".provenance/torrent/jobs/bt-fixture"
		return nil
	}); uerr != nil {
		t.Fatalf("seed capture: %v", uerr)
	}
	on, eerr := store.EnableSeed(done.ID)
	if eerr != nil {
		t.Fatalf("enable: %v", eerr)
	}
	if on.SeedDesired != SeedOn || on.SeedPhase != SeedWaiting {
		t.Fatalf("enable = %+v", on)
	}
	// Repeated enable is idempotent and touches nothing else.
	again, eerr := store.EnableSeed(done.ID)
	if eerr != nil || again.SeedDesired != SeedOn || again.Desired != on.Desired || again.Phase != on.Phase {
		t.Fatalf("repeated enable = %+v err=%v", again, eerr)
	}
	// A serving entry reports stopping, not stopped: the runner has not closed yet.
	if _, uerr := store.Update(done.ID, func(e *Entry) error {
		e.SeedPhase = SeedServing
		return nil
	}); uerr != nil {
		t.Fatalf("mark serving: %v", uerr)
	}
	off, derr := store.DisableSeed(done.ID)
	if derr != nil {
		t.Fatalf("disable: %v", derr)
	}
	if off.SeedDesired != SeedOff || off.SeedPhase != SeedStopping {
		t.Fatalf("disable of a serving seed = desired %q phase %q", off.SeedDesired, off.SeedPhase)
	}
	// Download intent and the capture are untouched by either direction.
	if off.Desired != on.Desired || off.Capture == nil || off.Capture.Key != "capture-fixture" {
		t.Fatalf("seed intent changed download state: %+v", off)
	}
	// The other entry is untouched.
	if got := mustGet(t, store, incomplete.ID); got.SeedDesired == SeedOn {
		t.Fatalf("an unrelated entry was changed: %+v", got)
	}
}

// TestQueueSeedIntentDoesNotStartRunner: enabling intent is durable and inert.
func TestQueueSeedIntentDoesNotStartRunner(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	done := actionEntry(t, store, PhaseCompleted, DesiredRun)
	if _, uerr := store.Update(done.ID, func(e *Entry) error {
		e.Capture = &CaptureRef{Key: "k"}
		e.Workspace = ".provenance/torrent/jobs/bt-fixture"
		return nil
	}); uerr != nil {
		t.Fatalf("capture: %v", uerr)
	}
	if _, eerr := store.EnableSeed(done.ID); eerr != nil {
		t.Fatalf("enable: %v", eerr)
	}
	if RunnerHeld(store.Root()) {
		t.Fatal("enabling seed intent took runner ownership")
	}
	if got := mustGet(t, store, done.ID); got.SeedDesired != SeedOn {
		t.Fatalf("intent not durable: %+v", got)
	}
}
