package torrentqueue

// Concurrency (BT-08, first slice) with a fake backend and deterministic barriers:
// bounded slots, FIFO claims, one pause without collateral cancellation, slots that
// are not freed early, stale results, shutdown with several active attempts, and
// invalid limits.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedTransfer blocks each attempt on its own channel until the test opens it, so
// overlap is observed rather than timed.
type gatedTransfer struct {
	mu      sync.Mutex
	runs    []Request
	active  int32
	maxSeen int32
	entered chan string
	release map[string]chan struct{}
	// ignoreCancel makes an attempt ignore context cancellation, which is how a
	// closing client looks from the scheduler's side: the slot is still held.
	ignoreCancel bool
	adopted      map[string]CaptureRef
}

func newGatedTransfer() *gatedTransfer {
	return &gatedTransfer{
		entered: make(chan string, 32),
		release: map[string]chan struct{}{},
		adopted: map[string]CaptureRef{},
	}
}

func (g *gatedTransfer) gate(id string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.release[id]
	if !ok {
		ch = make(chan struct{})
		g.release[id] = ch
	}
	return ch
}

func (g *gatedTransfer) Run(ctx context.Context, req Request) (Outcome, error) {
	cur := atomic.AddInt32(&g.active, 1)
	for {
		max := atomic.LoadInt32(&g.maxSeen)
		if cur <= max || atomic.CompareAndSwapInt32(&g.maxSeen, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&g.active, -1)
	id := req.KnownJobID
	if id == "" {
		id = req.Source
	}
	g.mu.Lock()
	g.runs = append(g.runs, req)
	g.mu.Unlock()
	g.entered <- id

	gate := g.gate(id)
	if g.ignoreCancel {
		// Stands in for a client that is still closing: it only notices the
		// cancellation once its own close completes.
		<-gate
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		return successOutcome(req), nil
	}
	select {
	case <-gate:
		return successOutcome(req), nil
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

func (g *gatedTransfer) Adopt(root, jobID string, selection []string) (CaptureRef, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ref, ok := g.adopted[jobID]
	return ref, ok
}

func (g *gatedTransfer) runCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.runs)
}

func waitEntered(t *testing.T, g *gatedTransfer, want int, timeout time.Duration) []string {
	t.Helper()
	var got []string
	deadline := time.Now().Add(timeout)
	for len(got) < want {
		select {
		case id := <-g.entered:
			got = append(got, id)
		case <-time.After(time.Until(deadline)):
			t.Fatalf("only %d of %d attempts started: %v", len(got), want, got)
		}
	}
	return got
}

// assertNotEntered fails if the transfer is asked to start another attempt within
// the settle window. It is a negative assertion, so the window is explicit.
func assertNotEntered(t *testing.T, g *gatedTransfer, why string) {
	t.Helper()
	select {
	case id := <-g.entered:
		t.Fatalf("%s: attempt %s started", why, id)
	case <-time.After(150 * time.Millisecond):
	}
}

func prepared(t *testing.T, store *Store, jobID string, sel ...string) Entry {
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
		t.Fatalf("add %s: %v", jobID, err)
	}
	if dup {
		t.Fatalf("add %s reported a duplicate", jobID)
	}
	return entry
}

func startAll(t *testing.T, store *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.Update(id, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
			t.Fatal(err)
		}
	}
}

// TestQueueMaxActiveOverlapAndFIFO is acceptance 1: with max-active=2 exactly two
// attempts overlap, a third waits for a slot, and claims follow queue order.
func TestQueueMaxActiveOverlapAndFIFO(t *testing.T) {
	store := newTestStore(t)
	var ids []string
	for i := 0; i < 3; i++ {
		e := prepared(t, store, testJobID(byte(i)))
		ids = append(ids, e.ID)
	}
	startAll(t, store, ids...)

	g := newGatedTransfer()
	runner := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2, Log: testWriter{t}})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()

	first := waitEntered(t, g, 2, 5*time.Second)
	if first[0] != testJobID(0) || first[1] != testJobID(1) {
		t.Fatalf("claims = %v, want the two lowest sequence entries in order", first)
	}
	assertNotEntered(t, g, "with both slots full")
	if max := atomic.LoadInt32(&g.maxSeen); max != 2 {
		t.Fatalf("observed %d concurrent attempts, want exactly 2", max)
	}

	close(g.gate(testJobID(0)))
	third := waitEntered(t, g, 1, 5*time.Second)
	if third[0] != testJobID(2) {
		t.Fatalf("third claim = %v, want %s", third, testJobID(2))
	}
	close(g.gate(testJobID(1)))
	close(g.gate(testJobID(2)))
	sum := <-done
	if sum.Completed != 3 || sum.Stale != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if max := atomic.LoadInt32(&g.maxSeen); max != 2 {
		t.Fatalf("peak concurrency %d, want 2 (the cap must hold)", max)
	}
	for _, id := range ids {
		entry, _ := store.Get(id)
		if entry.Phase != PhaseCompleted {
			t.Errorf("%s phase = %q", id, entry.Phase)
		}
	}
}

// TestQueueDefaultMaxActiveIsOne: the default stays a single worker.
func TestQueueDefaultMaxActiveIsOne(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	startAll(t, store, a.ID, b.ID)

	g := newGatedTransfer()
	done := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background())
		done <- sum
	}()
	waitEntered(t, g, 1, 5*time.Second)
	assertNotEntered(t, g, "with the default max-active")
	close(g.gate(testJobID(0)))
	waitEntered(t, g, 1, 5*time.Second)
	close(g.gate(testJobID(1)))
	<-done
	if max := atomic.LoadInt32(&g.maxSeen); max != 1 {
		t.Fatalf("default max concurrency = %d, want 1", max)
	}
}

// TestQueueInvalidMaxActive: zero, negative and excessive values are refused before
// anything is claimed.
func TestQueueInvalidMaxActive(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"negative", Options{MaxActive: -1}},
		{"too-high", Options{MaxActive: MaxActiveLimit + 1}},
		{"absurd", Options{MaxActive: 1000}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			entry := prepared(t, store, testJobID(0))
			startAll(t, store, entry.ID)
			g := newGatedTransfer()
			if _, err := NewRunner(store, g, tc.opts).Run(context.Background()); err == nil {
				t.Fatal("invalid max-active was accepted")
			}
			if g.runCount() != 0 {
				t.Error("an attempt started despite the invalid limit")
			}
			if got, _ := store.Get(entry.ID); got.Phase == PhaseRunning {
				t.Error("the entry was claimed despite the invalid limit")
			}
		})
	}
	// Zero means "default", which is validated as one.
	if err := (Options{MaxActive: 0}).Validate(); err != nil {
		t.Errorf("zero max-active should mean the default: %v", err)
	}
	if err := (Options{MaxActive: MaxActiveLimit}).Validate(); err != nil {
		t.Errorf("the documented limit should be accepted: %v", err)
	}
}

// TestQueuePauseOneKeepsSibling is acceptance 2: pausing one entry cancels only its
// attempt, the sibling keeps running, and the paused attempt's slot stays occupied
// until its client has actually closed.
func TestQueuePauseOneKeepsSibling(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	c := prepared(t, store, testJobID(2))
	startAll(t, store, a.ID, b.ID, c.ID)

	// A ignores cancellation: it stands in for a client that is still closing.
	g := newGatedTransfer()
	g.ignoreCancel = true
	runner := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()
	waitEntered(t, g, 2, 5*time.Second)

	// Pause A. Its intent is recorded immediately, but it cannot report paused while
	// its attempt is still open - and it must not release the slot.
	if _, err := store.Update(a.ID, func(e *Entry) error { e.Desired = DesiredPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	stopping := waitPhase(t, store, a.ID, PhaseStopping, 5*time.Second)
	if stopping.Desired != DesiredPaused {
		t.Fatalf("desired = %q", stopping.Desired)
	}
	// The sibling is untouched: no cancellation, still running.
	sibling, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Phase != PhaseRunning {
		t.Fatalf("sibling phase = %q, want running (a pause on A must not touch B)", sibling.Phase)
	}
	// And the third entry cannot start: A still holds its slot.
	assertNotEntered(t, g, "while the paused attempt is still closing")

	// A's client finally closes: now it becomes paused and its slot is freed.
	close(g.gate(testJobID(0)))
	waitPhase(t, store, a.ID, PhasePaused, 5*time.Second)
	third := waitEntered(t, g, 1, 5*time.Second)
	if third[0] != testJobID(2) {
		t.Fatalf("third claim = %v", third)
	}
	close(g.gate(testJobID(1)))
	close(g.gate(testJobID(2)))
	sum := <-done
	if sum.Paused != 1 || sum.Completed != 2 {
		t.Fatalf("summary = %+v, want one paused and two completed", sum)
	}
	if max := atomic.LoadInt32(&g.maxSeen); max > 2 {
		t.Fatalf("peak concurrency %d exceeded the cap", max)
	}
}

// TestQueueStaleResultIsIgnored: a result from a superseded attempt must not
// overwrite the newer command that replaced it.
func TestQueueStaleResultIsIgnored(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	startAll(t, store, a.ID)

	g := newGatedTransfer()
	g.ignoreCancel = true // the old attempt stays open while the user retries
	done := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background())
		done <- sum
	}()
	waitEntered(t, g, 1, 5*time.Second)

	// A newer command: the operator retries the entry while the old attempt is still
	// closing. That mints a new attempt token.
	if _, err := store.Update(a.ID, func(e *Entry) error {
		e.Desired = DesiredPaused
		e.Phase = PhasePaused
		e.Attempt = "newer-command"
		e.LastError = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(g.gate(testJobID(0)))
	sum := <-done

	final, err := store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase == PhaseCompleted || final.Phase == PhaseRunning {
		t.Fatalf("a stale attempt result overwrote the newer command: phase %q", final.Phase)
	}
	if final.Desired != DesiredPaused {
		t.Errorf("desired = %q, want the newer command's pause", final.Desired)
	}
	if final.Capture != nil {
		t.Errorf("stale result recorded a capture: %+v", final.Capture)
	}
	if sum.Stale != 1 {
		t.Errorf("summary = %+v, want one stale result", sum)
	}
}

// TestQueueShutdownWithMultipleActive is acceptance 3's shutdown half: every active
// attempt is cancelled and waited for, and intent survives.
func TestQueueShutdownWithMultipleActive(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	paused := prepared(t, store, testJobID(2))
	startAll(t, store, a.ID, b.ID)

	g := newGatedTransfer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2}).Run(ctx)
		done <- sum
	}()
	waitEntered(t, g, 2, 5*time.Second)
	cancel()
	sum := <-done
	if sum.Completed != 0 {
		t.Fatalf("summary = %+v, want nothing completed", sum)
	}
	for _, id := range []string{a.ID, b.ID} {
		entry, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Desired != DesiredRun || entry.Phase != PhaseQueued {
			t.Errorf("%s = %q/%q, want run/queued after shutdown", id, entry.Desired, entry.Phase)
		}
		if !strings.Contains(entry.Note, "shutdown") {
			t.Errorf("%s note = %q", id, entry.Note)
		}
	}
	if p, _ := store.Get(paused.ID); p.Desired != DesiredPaused || p.Phase != PhaseQueued {
		t.Errorf("paused entry = %q/%q, want untouched", p.Desired, p.Phase)
	}
	if atomic.LoadInt32(&g.active) != 0 {
		t.Fatal("Run returned while an attempt was still active")
	}
	if RunnerHeld(store.Root()) {
		t.Error("runner lock still held after shutdown")
	}
}

// TestQueueBusyDoesNotBlockSibling is acceptance 3's failure half: a busy entry backs
// off without hot-looping while healthy work proceeds.
func TestQueueBusyDoesNotBlockSibling(t *testing.T) {
	store := newTestStore(t)
	busy := prepared(t, store, testJobID(0))
	healthy := prepared(t, store, testJobID(1))
	startAll(t, store, busy.ID, healthy.ID)

	var busyRuns int32
	inner := newGatedTransfer()
	fake := &selectiveTransfer{
		gatedTransfer: inner,
		behavior: func(ctx context.Context, req Request) (Outcome, error) {
			if req.KnownJobID == testJobID(0) {
				atomic.AddInt32(&busyRuns, 1)
				return Outcome{}, ErrWorkspaceBusy
			}
			return successOutcome(req), nil
		},
	}
	sum, err := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2, Backoff: time.Minute}).Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Blocked != 1 || sum.Completed != 1 {
		t.Fatalf("summary = %+v, want one blocked and one completed", sum)
	}
	if got := atomic.LoadInt32(&busyRuns); got != 1 {
		t.Errorf("busy entry was attempted %d times in one drain, want 1 (no hot loop)", got)
	}
	blocked, _ := store.Get(busy.ID)
	if blocked.Phase != PhaseBlocked || blocked.NotBefore.IsZero() {
		t.Errorf("busy entry = %q not_before=%v", blocked.Phase, blocked.NotBefore)
	}
	if done, _ := store.Get(healthy.ID); done.Phase != PhaseCompleted {
		t.Errorf("healthy entry = %q, want completed", done.Phase)
	}
}

// testWriter routes runner transitions into the test log while debugging.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("runner: %s", string(p))
	return len(p), nil
}

// selectiveTransfer lets one test decide per request.
type selectiveTransfer struct {
	*gatedTransfer
	behavior func(ctx context.Context, req Request) (Outcome, error)
}

func (s *selectiveTransfer) Run(ctx context.Context, req Request) (Outcome, error) {
	if s.behavior != nil {
		return s.behavior(ctx, req)
	}
	return s.gatedTransfer.Run(ctx, req)
}

var _ = errors.Is
