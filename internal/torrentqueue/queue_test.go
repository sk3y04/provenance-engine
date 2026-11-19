package torrentqueue

// Queue semantics with a fake backend: ordering, intent versus observation,
// pause, retry, shutdown, restart reconciliation, and one-runner-at-a-time.
// Every wait is a channel or a bounded poll of recorded state; nothing sleeps and
// hopes.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testJobID(n byte) string {
	return "bt-" + strings.Repeat(string(rune('a'+n%6)), 40)
}

// fakeTransfer is a controllable backend.
type fakeTransfer struct {
	mu      sync.Mutex
	runs    []Request
	active  int32
	maxSeen int32
	// entered receives each run's request as it starts.
	entered chan Request
	// behavior decides how a run ends. Default: succeed immediately.
	behavior func(ctx context.Context, req Request) (Outcome, error)
	adopted  map[string]CaptureRef
}

func newFakeTransfer() *fakeTransfer {
	return &fakeTransfer{entered: make(chan Request, 16), adopted: map[string]CaptureRef{}}
}

func (f *fakeTransfer) Run(ctx context.Context, req Request) (Outcome, error) {
	cur := atomic.AddInt32(&f.active, 1)
	for {
		max := atomic.LoadInt32(&f.maxSeen)
		if cur <= max || atomic.CompareAndSwapInt32(&f.maxSeen, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.active, -1)

	f.mu.Lock()
	f.runs = append(f.runs, req)
	f.mu.Unlock()
	f.entered <- req

	if f.behavior != nil {
		return f.behavior(ctx, req)
	}
	return successOutcome(req), nil
}

func (f *fakeTransfer) Adopt(root, jobID string, selection []string) (CaptureRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, ok := f.adopted[jobID]
	return ref, ok
}

func (f *fakeTransfer) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func successOutcome(req Request) Outcome {
	job := req.KnownJobID
	if job == "" {
		job = testJobID(0)
	}
	return Outcome{
		JobID:        job,
		InfoHash:     strings.TrimPrefix(job, "bt-"),
		Workspace:    ".provenance/torrent/jobs/" + job,
		CaptureKey:   "cap-" + job,
		ManifestPath: ".provenance/runs/2026-11-19T010000Z.json",
	}
}

// waitPhase polls a recorded phase, which is what a user sees, with a deadline.
func waitPhase(t *testing.T, store *Store, id string, want Phase, timeout time.Duration) Entry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		entry, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if entry.Phase == want {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed at phase %q, want %q (desired %q, note %q)", id, entry.Phase, want, entry.Desired, entry.Note)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitForRun(t *testing.T, f *fakeTransfer, timeout time.Duration) Request {
	t.Helper()
	select {
	case req := <-f.entered:
		return req
	case <-time.After(timeout):
		t.Fatal("backend was never asked to run")
		return Request{}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

func addEntry(t *testing.T, store *Store, jobID string, sel ...string) Entry {
	t.Helper()
	if len(sel) == 0 {
		sel = []string{"fixture/a.bin"}
	}
	entry, dup, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(jobID, "bt-")},
		RawSource: "magnet:?xt=urn:btih:" + strings.TrimPrefix(jobID, "bt-"),
		Discovery: "direct",
		Selection: sel,
		JobID:     jobID,
		InfoHash:  strings.TrimPrefix(jobID, "bt-"),
		Workspace: ".provenance/torrent/jobs/" + jobID,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if dup {
		t.Fatalf("Add reported a duplicate for a fresh entry")
	}
	return entry
}

// TestQueueAddPausedThenStart is acceptance 1's spine: a new entry does nothing
// until an explicit start, and a drain runs one job at a time in queue order.
func TestQueueAddPausedThenStart(t *testing.T) {
	store := newTestStore(t)
	first := addEntry(t, store, testJobID(0))
	second := addEntry(t, store, testJobID(1))
	for _, e := range []Entry{first, second} {
		if e.Desired != DesiredPaused || e.Phase != PhaseQueued {
			t.Fatalf("new entry %s = desired %q phase %q, want paused/queued", e.ID, e.Desired, e.Phase)
		}
	}

	fake := newFakeTransfer()
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	sum, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain with everything paused: %v", err)
	}
	if fake.runCount() != 0 || sum.Completed != 0 {
		t.Fatalf("paused entries ran: runs=%d summary=%+v", fake.runCount(), sum)
	}

	for _, id := range []string{first.ID, second.ID} {
		if _, err := store.Update(id, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	sum, err = runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Completed != 2 || len(sum.Ran) != 2 {
		t.Fatalf("summary = %+v, want 2 completed", sum)
	}
	if max := atomic.LoadInt32(&fake.maxSeen); max != 1 {
		t.Fatalf("observed %d concurrent runs, want 1 worker", max)
	}
	// Queue order: sequence, not submission race.
	if got := fake.runs[0].KnownJobID; got != first.JobID {
		t.Errorf("first run was %s, want %s", got, first.JobID)
	}
	if got := fake.runs[1].KnownJobID; got != second.JobID {
		t.Errorf("second run was %s, want %s", got, second.JobID)
	}
	for _, id := range []string{first.ID, second.ID} {
		entry, _ := store.Get(id)
		if entry.Phase != PhaseCompleted || entry.Capture == nil {
			t.Errorf("%s = phase %q capture %+v, want completed with a capture", id, entry.Phase, entry.Capture)
		}
		if entry.Attempts != 1 {
			t.Errorf("%s attempts = %d, want 1", id, entry.Attempts)
		}
	}
}

// TestQueuePauseDuringWork: pause is recorded before acknowledgement, the entry
// reports stopping while the worker closes, and only then becomes paused.
func TestQueuePauseDuringWork(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(2))
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()
	waitForRun(t, fake, 5*time.Second)

	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	// stopping is observable while the worker is still closing.
	stopping := waitPhase(t, store, entry.ID, PhaseStopping, 5*time.Second)
	if stopping.Desired != DesiredPaused {
		t.Fatalf("stopping entry desired %q", stopping.Desired)
	}
	sum := <-done
	final := waitPhase(t, store, entry.ID, PhasePaused, 5*time.Second)
	if final.Capture != nil {
		t.Errorf("a paused entry recorded a capture: %+v", final.Capture)
	}
	if sum.Paused != 1 {
		t.Errorf("summary = %+v, want 1 paused", sum)
	}
	// Nothing is left runnable, so a second drain does nothing.
	before := fake.runCount()
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.runCount() != before {
		t.Error("a paused entry ran again")
	}
}

// TestQueuePauseRacingCompletion is acceptance 2: the pause is respected as
// intent, and a capture that was legitimately published is not discarded.
func TestQueuePauseRacingCompletion(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(3))
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		<-release
		return successOutcome(req), nil
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()
	waitForRun(t, fake, 5*time.Second)

	// Pause lands while the transfer is still in flight, then the transfer
	// completes anyway: the capture is real and must be kept.
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	final, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseCompleted {
		t.Fatalf("phase = %q, want completed (a published capture must not be undone)", final.Phase)
	}
	if final.Desired != DesiredPaused {
		t.Errorf("desired = %q, want paused (intent must survive completion)", final.Desired)
	}
	if final.Capture == nil || final.Capture.Key == "" {
		t.Fatalf("capture not recorded: %+v", final.Capture)
	}
	if !strings.Contains(final.Note, "pause") {
		t.Errorf("note = %q, want an explanation that a pause arrived during commit", final.Note)
	}
}

// TestQueueFailureDoesNotBlockLaterJobs: one failed entry is recorded and skipped,
// later entries still run, and retry is explicit.
func TestQueueFailureDoesNotBlockLaterJobs(t *testing.T) {
	store := newTestStore(t)
	bad := addEntry(t, store, testJobID(4))
	good := addEntry(t, store, testJobID(5))
	for _, id := range []string{bad.ID, good.ID} {
		if _, err := store.Update(id, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	var failOnce sync.Once
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		if req.KnownJobID == bad.JobID {
			var err error
			failOnce.Do(func() { err = errors.New("storage: no space left on device") })
			if err != nil {
				return Outcome{}, err
			}
			return successOutcome(req), nil
		}
		return successOutcome(req), nil
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond, Backoff: time.Minute})
	sum, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Failed != 1 || sum.Completed != 1 {
		t.Fatalf("summary = %+v, want 1 failed and 1 completed", sum)
	}
	badEntry, _ := store.Get(bad.ID)
	if badEntry.Phase != PhaseFailed {
		t.Fatalf("failed entry phase = %q", badEntry.Phase)
	}
	if !strings.Contains(badEntry.LastError, "no space left") {
		t.Errorf("last error = %q", badEntry.LastError)
	}
	goodEntry, _ := store.Get(good.ID)
	if goodEntry.Phase != PhaseCompleted {
		t.Fatalf("later entry phase = %q, want completed", goodEntry.Phase)
	}

	// A failed entry is not re-claimed by another drain.
	before := fake.runCount()
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.runCount() != before {
		t.Error("a failed entry was retried without an explicit retry")
	}

	// The explicit retry makes it runnable again, and it then succeeds.
	if _, err := store.Update(bad.ID, func(e *Entry) error {
		e.Desired = DesiredRun
		e.Phase = PhaseQueued
		e.LastError = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sum, err = runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain after retry: %v", err)
	}
	if sum.Completed != 1 {
		t.Fatalf("retry summary = %+v, want 1 completed", sum)
	}
	retried, _ := store.Get(bad.ID)
	if retried.Phase != PhaseCompleted || retried.Attempts != 2 {
		t.Errorf("retried entry = phase %q attempts %d", retried.Phase, retried.Attempts)
	}
}

// TestQueueShutdownPreservesIntent is acceptance 1's restart half: SIGINT is an
// interruption, not a pause, and a paused entry is still paused after restart.
func TestQueueShutdownPreservesIntent(t *testing.T) {
	store := newTestStore(t)
	runnable := addEntry(t, store, testJobID(6))
	paused := addEntry(t, store, testJobID(7))
	if _, err := store.Update(runnable.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	fake := newFakeTransfer()
	fake.behavior = func(runCtx context.Context, req Request) (Outcome, error) {
		<-runCtx.Done()
		return Outcome{}, runCtx.Err()
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(ctx)
		done <- sum
	}()
	waitForRun(t, fake, 5*time.Second)
	cancel()
	<-done

	interrupted, err := store.Get(runnable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Desired != DesiredRun {
		t.Fatalf("desired = %q after shutdown, want run", interrupted.Desired)
	}
	if interrupted.Phase != PhaseQueued {
		t.Fatalf("phase = %q after shutdown, want queued (interruption is not a pause)", interrupted.Phase)
	}
	if !strings.Contains(interrupted.Note, "shutdown") {
		t.Errorf("note = %q, want an interruption explanation", interrupted.Note)
	}
	stillPaused, _ := store.Get(paused.ID)
	if stillPaused.Desired != DesiredPaused || stillPaused.Phase != PhaseQueued {
		t.Errorf("paused entry became %q/%q across the shutdown", stillPaused.Desired, stillPaused.Phase)
	}

	// Restart with a healthy backend: the interrupted entry resumes, the paused
	// one stays put.
	healthy := newFakeTransfer()
	sum, err := NewRunner(store, healthy, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("restart drain: %v", err)
	}
	if sum.Completed != 1 {
		t.Fatalf("restart summary = %+v, want 1 completed", sum)
	}
	if healthy.runCount() != 1 || healthy.runs[0].KnownJobID != runnable.JobID {
		t.Fatalf("restart ran %+v, want only the interrupted entry", healthy.runs)
	}
	final, _ := store.Get(paused.ID)
	if final.Phase == PhaseCompleted {
		t.Error("an explicitly paused entry ran after restart")
	}
}

// TestQueueReconcileInterruptedObservations: a record left "running" by a dead
// process is reconciled from intent and from an existing published capture.
func TestQueueReconcileInterruptedObservations(t *testing.T) {
	store := newTestStore(t)
	adopted := addEntry(t, store, testJobID(8))
	resume := addEntry(t, store, testJobID(9))
	for _, id := range []string{adopted.ID, resume.ID} {
		if _, err := store.Update(id, func(e *Entry) error {
			e.Desired = DesiredRun
			e.Phase = PhaseRunning
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFakeTransfer()
	fake.adopted[adopted.JobID] = CaptureRef{Key: "adopted-key", Manifest: ".provenance/runs/2026-11-19T010000Z.json"}
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) { return successOutcome(req), nil }

	sum, err := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	got, _ := store.Get(adopted.ID)
	if got.Phase != PhaseCompleted || got.Capture == nil || got.Capture.Key != "adopted-key" {
		t.Fatalf("interrupted-with-capture entry = phase %q capture %+v", got.Phase, got.Capture)
	}
	if !strings.Contains(got.Note, "adopted") {
		t.Errorf("note = %q, want an adoption explanation", got.Note)
	}
	// The adopted entry was never transferred.
	for _, req := range fake.runs {
		if req.KnownJobID == adopted.JobID {
			t.Fatal("an entry with an existing capture was refetched")
		}
	}
	resumed, _ := store.Get(resume.ID)
	if resumed.Phase != PhaseCompleted || resumed.Attempts != 1 {
		t.Fatalf("interrupted entry = phase %q attempts %d", resumed.Phase, resumed.Attempts)
	}
	if sum.Completed != 2 {
		t.Errorf("summary = %+v, want 2 completed", sum)
	}
}

// TestQueueBlockedWorkspaceBacksOff: a busy workspace is retryable, not a hot loop.
func TestQueueBlockedWorkspaceBacksOff(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(10))
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		return Outcome{}, errors.New("job workspace is busy: owned by another process")
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond, Backoff: time.Minute})
	sum, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	got, _ := store.Get(entry.ID)
	if got.Phase != PhaseFailed {
		// A plain error is a failure; only a typed busy error blocks.
		t.Fatalf("phase = %q, want failed for an untyped error", got.Phase)
	}
	if sum.Failed != 1 {
		t.Errorf("summary = %+v", sum)
	}

	// Now the typed busy error: blocked, with a backoff that keeps it out of the
	// next drain and does not spin.
	typed := newTestStore(t)
	blocked := addEntry(t, typed, testJobID(11))
	if _, err := typed.Update(blocked.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	busy := newFakeTransfer()
	busy.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		return Outcome{}, ErrWorkspaceBusy
	}
	r := NewRunner(typed, busy, Options{PollInterval: 2 * time.Millisecond, Backoff: time.Minute})
	sum, err = r.Run(context.Background())
	if err != nil {
		t.Fatalf("busy drain: %v", err)
	}
	got, _ = typed.Get(blocked.ID)
	if got.Phase != PhaseBlocked || got.NotBefore.IsZero() {
		t.Fatalf("busy entry = phase %q not_before %v, want blocked with a backoff", got.Phase, got.NotBefore)
	}
	if sum.Blocked != 1 {
		t.Errorf("summary = %+v, want 1 blocked", sum)
	}
	before := busy.runCount()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if busy.runCount() != before {
		t.Error("a blocked entry was retried before its backoff elapsed")
	}
}

// TestQueueOneRunnerPerQueue: a second runner is refused by the runner lock.
func TestQueueOneRunnerPerQueue(t *testing.T) {
	store := newTestStore(t)
	entry := addEntry(t, store, testJobID(12))
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		select {
		case <-release:
			return successOutcome(req), nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		_, _ = runner.Run(context.Background())
		close(done)
	}()
	waitForRun(t, fake, 5*time.Second)

	if !RunnerHeld(store.Root()) {
		t.Error("runner lock not held while draining")
	}
	if _, err := NewRunner(store, fake, Options{}).Run(context.Background()); !errors.Is(err, ErrRunnerActive) {
		t.Fatalf("second runner error = %v, want ErrRunnerActive", err)
	}
	close(release)
	<-done
	if RunnerHeld(store.Root()) {
		t.Error("runner lock still held after the drain returned")
	}
}

// TestQueueOrderAndIds: ids are independent of torrent identity and ordering is
// the recorded sequence, not directory order.
func TestQueueOrderAndIds(t *testing.T) {
	store := newTestStore(t)
	var ids []string
	for i := 0; i < 5; i++ {
		entry := addEntry(t, store, testJobID(byte(i)), "fixture/"+string(rune('a'+i))+".bin")
		ids = append(ids, entry.ID)
		if strings.Contains(entry.ID, strings.TrimPrefix(entry.JobID, "bt-")) {
			t.Fatalf("entry id %q embeds the infohash", entry.ID)
		}
		if !strings.HasPrefix(entry.ID, "queued-") {
			t.Fatalf("entry id %q is not an entry id", entry.ID)
		}
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(ids) {
		t.Fatalf("%d entries listed, want %d", len(entries), len(ids))
	}
	for i, e := range entries {
		if e.ID != ids[i] {
			t.Fatalf("position %d is %s, want %s (queue order must be the recorded sequence)", i, e.ID, ids[i])
		}
		if e.Seq != uint64(i+1) {
			t.Errorf("%s seq = %d, want %d", e.ID, e.Seq, i+1)
		}
	}
}

// TestQueueDuplicateAndConflict: the same capture intent converges on one entry,
// and a different selection on the same torrent is a second, distinct entry.
func TestQueueDuplicateAndConflict(t *testing.T) {
	store := newTestStore(t)
	first := addEntry(t, store, testJobID(13), "fixture/a.bin")
	again, duplicate, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(first.JobID, "bt-")},
		Discovery: "direct",
		Selection: []string{"fixture/a.bin"},
		JobID:     first.JobID,
	})
	if err != nil {
		t.Fatalf("duplicate Add: %v", err)
	}
	if !duplicate || again.ID != first.ID {
		t.Fatalf("duplicate submission created %s instead of returning %s", again.ID, first.ID)
	}
	entries, _ := store.List()
	if len(entries) != 1 {
		t.Fatalf("%d entries after a duplicate submission, want 1", len(entries))
	}

	other := addEntry(t, store, testJobID(13), "fixture/b.bin")
	if other.ID == first.ID {
		t.Fatal("a differing selection reused the existing entry")
	}
	if other.JobID != first.JobID {
		t.Fatalf("conflicting entry job id %s, want the same torrent %s", other.JobID, first.JobID)
	}
}

// TestQueueRecordsNoSecrets: the public record and list output never carry a
// tracker URL or a peer, while the restricted record does (0600).
func TestQueueRecordsNoSecrets(t *testing.T) {
	store := newTestStore(t)
	const passkey = "SECRETPASSKEY123"
	raw := "magnet:?xt=urn:btih:" + strings.TrimPrefix(testJobID(14), "bt-") +
		"&tr=http://tracker.example/announce?passkey=" + passkey + "&x.pe=10.0.0.1:6881"
	entry, _, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(testJobID(14), "bt-")},
		RawSource: raw,
		Peers:     []string{"10.0.0.1:6881"},
		Discovery: "direct",
		Selection: []string{"fixture/a.bin"},
		JobID:     testJobID(14),
	})
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(store.entriesDir(), entry.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{passkey, "tracker.example", "10.0.0.1", "passkey"} {
		if strings.Contains(string(public), secret) {
			t.Errorf("public record leaks %q", secret)
		}
	}
	restrictedPath := filepath.Join(store.restrictedDir(), entry.ID+".json")
	restricted, err := os.ReadFile(restrictedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restricted), passkey) {
		t.Error("restricted record lost the original source")
	}
	fi, err := os.Stat(restrictedPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("restricted record mode = %o, want 600", perm)
	}
	raw2, peers, err := store.RestrictedSource(entry.ID)
	if err != nil || raw2 != raw || len(peers) != 1 {
		t.Fatalf("RestrictedSource = %q %v %v", raw2, peers, err)
	}
	// The runner hands the restricted source to the backend, not the sanitized one.
	fake := newFakeTransfer()
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.runCount() != 1 || fake.runs[0].Source != raw {
		t.Fatalf("backend got source %q, want the restricted original", fake.runs[0].Source)
	}
	if fake.runs[0].Discovery != "direct" {
		t.Errorf("backend got discovery %q, want the recorded direct profile", fake.runs[0].Discovery)
	}
}

// TestQueueUnresolvedEntrySurvivesRestart: an unresolved entry keeps its source and
// discovery policy across a restart and is only attached after resolution.
func TestQueueUnresolvedEntrySurvivesRestart(t *testing.T) {
	store := newTestStore(t)
	entry, _, err := store.Add(AddRequest{
		Source:     Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32)},
		RawSource:  "magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32) + "&tr=http://private.example/announce?passkey=SECRET",
		Discovery:  "tracker",
		Selection:  []string{"fixture/a.bin"},
		Unresolved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Unresolved || entry.JobID != "" || entry.Workspace != "" {
		t.Fatalf("unresolved entry = %+v, want no job id and no workspace", entry)
	}
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}
	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		// The backend resolves: only now does a canonical identity exist.
		return Outcome{
			JobID:        testJobID(15),
			InfoHash:     strings.TrimPrefix(testJobID(15), "bt-"),
			Workspace:    ".provenance/torrent/jobs/" + testJobID(15),
			CaptureKey:   "cap-resolved",
			ManifestPath: ".provenance/runs/2026-11-19T010000Z.json",
		}, nil
	}
	if _, err := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(entry.ID)
	if got.JobID != testJobID(15) || got.Unresolved || got.Workspace == "" {
		t.Fatalf("resolved entry = %+v", got)
	}
	if got.Discovery != "tracker" {
		t.Errorf("discovery = %q, want the recorded profile", got.Discovery)
	}
	// The persisted record carries the resolved identity, the recorded discovery
	// profile, and no tracker URL: the resume path needs the id and the profile,
	// never the private announce list.
	public, rerr := os.ReadFile(filepath.Join(store.entriesDir(), entry.ID+".json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(public), testJobID(15)) {
		t.Error("resolved job id was not persisted")
	}
	for _, leak := range []string{"private.example", "SECRET", "tr="} {
		if strings.Contains(string(public), leak) {
			t.Errorf("public record leaks %q", leak)
		}
	}
	if got.Capture == nil || got.Capture.Key != "cap-resolved" {
		t.Errorf("capture = %+v, want the resolved capture", got.Capture)
	}
}
