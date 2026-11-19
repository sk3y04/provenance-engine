package torrentqueue

// BT-08d.4: regression coverage for the gaps BT-08d.3 declared.

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
	"golang.org/x/time/rate"
)

func completedSeededEntry(t *testing.T, store *Store, seed *loopbackSeeder, rel string) Entry {
	t.Helper()
	entry := enqueue(t, store, seed, rel)
	startEntry(t, store, entry.ID)
	if sum, err := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background()); err != nil || sum.Completed != 1 {
		t.Fatalf("prepare completed entry: %+v, %v", sum, err)
	}
	done := mustGet(t, store, entry.ID)
	if done.Phase != PhaseCompleted || done.Capture == nil {
		t.Fatalf("entry did not complete: %+v", done)
	}
	return done
}

func waitSeedPhase(t *testing.T, store *Store, id string, want SeedPhase, limit time.Duration) Entry {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		e := mustGet(t, store, id)
		if e.SeedPhase == want {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry %s stayed at seed phase %q, want %q (%+v)", id, e.SeedPhase, want, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQueueSeedStopStartDuringClose is the first gap: a token only protects the latest
// intent if the old attempt cannot publish over it. Close is held at a barrier, unseed
// and seed are issued while it is held, and the old attempt is then allowed to finish.
func TestQueueSeedStopStartDuringClose(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(256<<10, 5)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	before := mustGet(t, store, entry.ID)
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}

	hold := make(chan struct{})
	entered := make(chan string, 1)
	var opened atomic.Int32
	adapter := &seedCountingAdapter{Adapter: realAdapter(), opened: &opened}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = NewRunner(store, adapter, Options{
			MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr,
			BeforeSeedClose: func(string) {
				select {
				case entered <- "closing":
				default:
				}
				<-hold
			},
		}).Run(ctx)
	}()

	waitSeedPhase(t, store, entry.ID, SeedServing, 90*time.Second)

	// Stop it: the attempt is cancelled and enters Close, which the barrier holds.
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatalf("the seed never reached Close; entry=%+v", mustGet(t, store, entry.ID))
	}

	// Close is held: the workspace must still be owned and no replacement may start.
	ws, werr := bittorrent.WorkspacePathForJob(store.Root(), entry.JobID)
	if werr != nil {
		t.Fatalf("workspace path: %v", werr)
	}
	probe, perr := bittorrent.OpenSeedSession(context.Background(),
		bittorrent.Config{StagingDir: ws, Discovery: bittorrent.DiscoveryDirect}, entry.JobID)
	if perr == nil {
		_ = probe.Close()
		t.Fatal("the workspace was acquirable while Close was still running")
	}
	if got := opened.Load(); got != 1 {
		t.Fatalf("a replacement seed started while the old one was closing: %d sessions", got)
	}

	// Re-seed while the old attempt is still closing.
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("re-seed: %v", serr)
	}
	if got := mustGet(t, store, entry.ID); !got.SeedWanted() {
		t.Fatalf("re-seed did not persist: %+v", got)
	}
	close(hold)

	// The stale close must not publish "stopped" over the new intent.
	deadline := time.Now().Add(90 * time.Second)
	for {
		e := mustGet(t, store, entry.ID)
		if e.SeedPhase == SeedStopped && e.SeedWanted() {
			cancel()
			t.Fatalf("a stale close marked the re-seeded entry stopped: %+v", e)
		}
		if opened.Load() >= 2 && e.SeedPhase == SeedServing {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no replacement seed started; entry=%+v opened=%d", e, opened.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := mustGet(t, store, entry.ID)
	if after.SeedDesired != SeedOn || after.SeedPhase != SeedServing {
		t.Fatalf("final state = desired %q phase %q, want on/serving", after.SeedDesired, after.SeedPhase)
	}
	if after.Capture == nil || after.Capture.Key != before.Capture.Key {
		t.Fatalf("re-seeding changed the capture: %+v -> %+v", before.Capture, after.Capture)
	}
	t.Logf("old attempt closed after re-seed; replacement serving; capture key unchanged")

	cancel()
	<-done
	assertLockFree(t, store, entry)
}

type seedCountingAdapter struct {
	Adapter
	opened *atomic.Int32
}

func (a *seedCountingAdapter) OpenSeed(entry Entry, req Request) (SeedSession, error) {
	sess, err := a.Adapter.OpenSeed(entry, req)
	if err != nil {
		return nil, err
	}
	a.opened.Add(1)
	return sess, nil
}

// TestQueueSeedDuplicateWorkspaceSingleOwner: two records naming one canonical
// workspace. Admission normally prevents this, so the state is written directly - a
// converged race between two resolutions of one hybrid.
func TestQueueSeedDuplicateWorkspaceSingleOwner(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(256<<10, 9)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}

	dup := mustGet(t, store, entry.ID)
	dup.ID = "queued-900002-dup"
	dup.Seq = 900002
	dup.SeedDesired = SeedOn
	dup.SeedPhase = SeedWaiting
	if werr := store.writeEntry(dup); werr != nil {
		t.Fatalf("write duplicate record: %v", werr)
	}
	runsBefore := runManifestCount(t, workspacePath(t, store, entry))

	var opened atomic.Int32
	adapter := &seedCountingAdapter{Adapter: realAdapter(), opened: &opened}
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, adapter, Options{MaxActive: 1, MaxSeeds: 2, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()

	waitSeedPhase(t, store, entry.ID, SeedServing, 90*time.Second)
	// Hold off long enough that a hot loop would show up as repeated dispatches.
	time.Sleep(2 * time.Second)
	if got := opened.Load(); got != 1 {
		cancel()
		t.Fatalf("%d seed sessions were opened for one workspace, want 1", got)
	}
	other := mustGet(t, store, dup.ID)
	if other.SeedPhase == SeedServing {
		cancel()
		t.Fatalf("the duplicate entry is also serving: %+v", other)
	}
	cancel()
	<-runnerDone
	if runs := runManifestCount(t, workspacePath(t, store, entry)); runs != runsBefore {
		t.Fatalf("a duplicate capture was published: %d runs, was %d", runs, runsBefore)
	}
}

func workspacePath(t *testing.T, store *Store, entry Entry) string {
	t.Helper()
	ws, err := bittorrent.WorkspacePathForJob(store.Root(), entry.JobID)
	if err != nil {
		t.Fatalf("workspace path: %v", err)
	}
	return ws
}

// TestQueueSeedBlockedWorkspaceAndUnrelatedDownload: a damaged workspace is refused
// through the real queue with an actionable reason, while an unrelated download
// completes. Nothing is repaired, fetched or republished.
func TestQueueSeedBlockedWorkspaceAndUnrelatedDownload(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(256<<10, 13)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	ws := workspacePath(t, store, entry)
	runsBefore := runManifestCount(t, ws)

	payload := filepath.Join(ws, "alpha", "a.bin")
	original, rerr := os.ReadFile(payload)
	if rerr != nil {
		t.Fatalf("read payload: %v", rerr)
	}
	if cerr := os.Chmod(payload, 0o644); cerr != nil {
		t.Fatalf("make payload writable: %v", cerr)
	}
	damaged := append([]byte(nil), original...)
	damaged[len(damaged)/2] ^= 0xff
	if werr := os.WriteFile(payload, damaged, 0o644); werr != nil {
		t.Fatalf("damage payload: %v", werr)
	}
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}

	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(128<<10, 17)}, 0)
	other := enqueue(t, store, seedB, "beta/b.bin")
	startEntry(t, store, other.ID)

	sum, rerr := NewRunner(store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).
		Run(context.Background())
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	blocked := waitSeedPhase(t, store, entry.ID, SeedBlocked, 30*time.Second)
	if blocked.SeedError == "" || blocked.Note == "" {
		t.Fatalf("blocked seed carries no actionable reason: %+v", blocked)
	}
	if sum.Completed < 1 {
		t.Fatalf("the unrelated download did not complete: %+v", sum)
	}
	if got := mustGet(t, store, other.ID); got.Phase != PhaseCompleted {
		t.Fatalf("unrelated entry = %+v, want completed", got)
	}
	if runs := runManifestCount(t, ws); runs != runsBefore {
		t.Fatalf("the rejected workspace gained a run manifest: %d, was %d", runs, runsBefore)
	}
	after, rerr2 := os.ReadFile(payload)
	if rerr2 != nil || sha256.Sum256(after) != sha256.Sum256(damaged) {
		t.Logf("entry after run: %+v", mustGet(t, store, entry.ID))
		t.Logf("before: len=%d sha=%x", len(damaged), sha256.Sum256(damaged))
		t.Logf("after:  len=%d sha=%x err=%v", len(after), sha256.Sum256(after), rerr2)
		t.Fatalf("the damaged payload was rewritten by the refusal")
	}
	if blocked.Capture == nil || blocked.Capture.Key != mustGet(t, store, entry.ID).Capture.Key {
		t.Fatal("the historical capture changed")
	}
	t.Logf("blocked: %q / %q", blocked.SeedError, blocked.Note)
}

// limiterRecorder captures the upload limiter each attempt receives.
type limiterRecorder struct {
	Adapter
	mu     sync.Mutex
	upload []*rate.Limiter
}

func (l *limiterRecorder) record(req Request) {
	l.mu.Lock()
	l.upload = append(l.upload, req.UploadLimiter)
	l.mu.Unlock()
}

func (l *limiterRecorder) Run(ctx context.Context, req Request) (Outcome, error) {
	l.record(req)
	return l.Adapter.Run(ctx, req)
}

func (l *limiterRecorder) OpenSeed(entry Entry, req Request) (SeedSession, error) {
	l.record(req)
	return l.Adapter.OpenSeed(entry, req)
}

func (l *limiterRecorder) seen() []*rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*rate.Limiter(nil), l.upload...)
}

// TestQueueSeedSharesRunnerUploadLimiter: downloads and seeds receive the very same
// runner-owned upload limiter. This is pointer wiring, not a traffic measurement.
func TestQueueSeedSharesRunnerUploadLimiter(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(128<<10, 19)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(128<<10, 23)}, 0)
	other := enqueue(t, store, seedB, "beta/b.bin")
	startEntry(t, store, other.ID)

	rec := &limiterRecorder{Adapter: realAdapter()}
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, rec, Options{
			MaxActive: 1, MaxSeeds: 1, UploadLimit: 64 << 10, PollInterval: 20 * time.Millisecond, Log: os.Stderr,
		}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()
	waitSeedPhase(t, store, entry.ID, SeedServing, 90*time.Second)
	deadline := time.Now().Add(60 * time.Second)
	for len(rec.seen()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d attempts recorded; want a download and a seed", len(rec.seen()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	seen := rec.seen()
	first := seen[0]
	if first == nil {
		t.Fatal("an attempt received no upload limiter despite --upload-rate")
	}
	for i, l := range seen {
		if l != first {
			t.Fatalf("attempt %d received a different upload limiter (%p vs %p)", i, l, first)
		}
	}
	if got, want := first.Limit(), rate.Limit(64<<10); got != want {
		t.Fatalf("limiter rate = %v, want %v", got, want)
	}
	t.Logf("%d attempts (downloads and seeds) shared one upload limiter at %v B/s", len(seen), first.Limit())
}

func waitMarkerAddr(t *testing.T, path string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no serving address was announced within %s", within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestQueueSeedResumesAfterRunnerDeath kills a real runner while it is serving payload
// to a controlled leecher, then brings up a second runner on the same root.
func TestQueueSeedResumesAfterRunnerDeath(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(512<<10, 29)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	runsBefore := runManifestCount(t, workspacePath(t, store, entry))
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	// A second completed entry that was explicitly stopped: it must stay stopped.
	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(64<<10, 31)}, 0)
	stopped := completedSeededEntry(t, store, seedB, "beta/b.bin")
	if _, serr := store.SetSeedIntent(stopped.ID, true); serr != nil {
		t.Fatalf("seed intent (stopped): %v", serr)
	}
	if _, serr := store.SetSeedIntent(stopped.ID, false); serr != nil {
		t.Fatalf("unseed (stopped): %v", serr)
	}

	marker := filepath.Join(t.TempDir(), "seed-addr")
	cmd := startHelper(t, root, "seed-serving", marker)
	addr := waitMarkerAddr(t, marker, 90*time.Second)
	leech, _ := leechFrom(t, addr, seedMetainfo(t, seedA), t.TempDir())
	// Real payload before the kill: a phase or a listener is not enough.
	deadline := time.Now().Add(90 * time.Second)
	for torBytes(leech) == 0 {
		if time.Now().After(deadline) {
			killHelper(t, cmd)
			t.Fatalf("the child never served payload to a leecher")
		}
		time.Sleep(20 * time.Millisecond)
	}
	servedBeforeKill := torBytes(leech)
	killHelper(t, cmd)

	// A second runner on the same root must reacquire ownership and serve again.
	var closed atomic.Bool
	adapter := &seedWatchingAdapter{Adapter: realAdapter(), addr: make(chan string, 1),
		uploaded: new(atomic.Int64), closed: &closed, opened: new(atomic.Int32)}
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, adapter, Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()

	var addr2 string
	select {
	case addr2 = <-adapter.addr:
	case <-time.After(90 * time.Second):
		t.Fatalf("the restarted runner never served; entry=%+v", mustGet(t, store, entry.ID))
	}
	if addr2 == addr {
		t.Fatalf("the restarted seed reused the dead session's address %s", addr2)
	}
	leech2, _ := leechFrom(t, addr2, seedMetainfo(t, seedA), t.TempDir())
	deadline = time.Now().Add(90 * time.Second)
	for torBytes(leech2) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted seed served no bytes")
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := mustGet(t, store, entry.ID)
	if !after.SeedWanted() {
		t.Fatalf("restart lost seed intent: %+v", after)
	}
	stillStopped := mustGet(t, store, stopped.ID)
	if stillStopped.SeedWanted() || stillStopped.SeedPhase == SeedServing {
		t.Fatalf("an explicitly stopped entry resumed: %+v", stillStopped)
	}
	if runs := runManifestCount(t, workspacePath(t, store, entry)); runs != runsBefore {
		t.Fatalf("restart duplicated a capture: %d runs, was %d", runs, runsBefore)
	}
	t.Logf("killed with %d bytes served; restarted runner served %d bytes on a new address; stopped entry stayed stopped",
		servedBeforeKill, torBytes(leech2))
}
