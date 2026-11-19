package torrentqueue

// BT-10a: live snapshots for the foreground runner.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// liveQuery retries only the transient "not published yet" window. A runner that has
// exited never publishes again, so the retry cannot mask a dead endpoint: the tests
// that care assert separately that the runner is still running.
func liveQuery(t *testing.T, root string) LiveSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		snap, err := LiveSnapshotFor(ctx, root)
		cancel()
		if err == nil {
			return snap
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("live snapshot: %v", lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLiveSnapshotOverlappingAttempts: one download slot and one seed slot in use, and
// the durable list still reports recorded state independently.
func TestLiveSnapshotOverlappingAttempts(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(128<<10, 109)}, 0)
	done := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(done.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	// A slow download keeps the download slot busy while the seed serves.
	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(2<<20, 113)}, 128<<10)
	slow := enqueue(t, store, seedB, "beta/b.bin")
	startEntry(t, store, slow.ID)

	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()

	deadline := time.Now().Add(90 * time.Second)
	var snap LiveSnapshot
	for {
		snap, err = LiveSnapshotFor(context.Background(), store.Root())
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if len(snap.Attempts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no overlapping attempts observed: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
	kinds := map[string]string{}
	for _, a := range snap.Attempts {
		kinds[a.Kind] = a.Phase
	}
	if kinds["download"] != "running" {
		t.Fatalf("download attempt = %+v, want running", snap.Attempts)
	}
	if _, ok := kinds["seed"]; !ok {
		t.Fatalf("no seed attempt in %+v", snap.Attempts)
	}
	if snap.Downloads.Used != 1 || snap.Downloads.Max != 1 || snap.Seeds.Used != 1 || snap.Seeds.Max != 1 {
		t.Fatalf("slots = %+v/%+v", snap.Downloads, snap.Seeds)
	}
	// The durable view is independent: it still reports recorded phases, and the live
	// snapshot never rewrote them.
	list, lerr := store.List()
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	for _, e := range list {
		if e.Phase != PhaseCompleted && e.Phase != PhaseRunning {
			t.Fatalf("durable phase changed by live reporting: %+v", e)
		}
	}
}

// TestLiveStoppingHeldAtClose: a seed whose client is still closing is reported as
// stopping, keeps its slot, and frees it only after Close returns. The barrier is the
// session's own Close, not a hook, so the invariant is about the runner's slot
// accounting rather than about a test seam.
func TestLiveStoppingHeldAtClose(t *testing.T) {
	// The barrier here is the session's own Close, and it is the session handed to the
	// runner by the opener - not a hook. The old failure was not reproducible once the
	// barrier was proven to belong to that exact session (see the TRACE lines in
	// docs/BT-CHECKPOINTS.md and TestLiveCloseBarrierFreshRoot/ReusedRoot).
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(128<<10, 127)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}

	sess := newBarrierSession("live", entry.ID)
	t.Cleanup(func() { sess.release(t, "cleanup") })
	opener := &barrierOpener{Adapter: realAdapter(), sess: sess}
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, opener, Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-runnerDone })

	waitSeedPhase(t, store, entry.ID, SeedServing, 90*time.Second)
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-sess.closeIn:
	case <-time.After(60 * time.Second):
		t.Fatal("the session never reached Close")
	}

	// Close is held: Run is still running, ownership is held, and the live client sees
	// a stopping seed that still occupies its slot.
	select {
	case <-runnerDone:
		t.Fatal("the runner exited while a seed client was still closing")
	default:
	}
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatal("runner ownership was free while an attempt was closing")
	}
	snap := liveQuery(t, store.Root())
	if len(snap.Attempts) != 1 || !snap.Attempts[0].Stopping || snap.Attempts[0].Phase != "stopping" {
		t.Fatalf("during Close the attempt = %+v, want one stopping seed", snap.Attempts)
	}
	if snap.Attempts[0].EntryID != entry.ID {
		t.Fatalf("the live attempt is %s, want %s", snap.Attempts[0].EntryID, entry.ID)
	}
	if snap.Seeds.Used != 1 {
		t.Fatalf("a closing attempt released its slot early: %+v", snap.Seeds)
	}

	// Explicit release: Close returns, the runner joins, ownership frees and the
	// endpoint stops answering.
	sess.release(t, "explicit")
	select {
	case <-runnerDone:
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return after Close finished")
	}
	if _, perr := liveProbe(t, store.Root()); perr == nil {
		t.Fatal("the endpoint still answered after Run returned")
	}
	probe, lerr := store.LockRunner()
	if lerr != nil {
		t.Fatalf("runner ownership was not released after Run returned: %v", lerr)
	}
	_ = probe.Release()
	// The workspace is reusable.
	ws := workspacePath(t, store, entry)
	if s2, oerr := bittorrent.OpenSeedSession(context.Background(),
		bittorrent.Config{StagingDir: ws, Discovery: bittorrent.DiscoveryDirect}, entry.JobID); oerr != nil {
		t.Fatalf("workspace not reusable after shutdown: %v", oerr)
	} else {
		_ = s2.Close()
	}
}

type fakeSeedOpener struct {
	Adapter
	next SeedSession
}

func (f *fakeSeedOpener) OpenSeed(Entry, Request) (SeedSession, error) { return f.next, nil }

// TestLiveNoRunner is the truth requirement: a stale endpoint or a leftover index file
// must not be reported as a live runner.
func TestLiveNoRunner(t *testing.T) {
	root := t.TempDir()
	if _, err := OpenStore(root); err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := LiveSnapshotFor(context.Background(), root); !errors.Is(err, ErrNoLiveRunner) {
		t.Fatalf("no runner = %v, want ErrNoLiveRunner", err)
	}
	// A stale index naming a socket that does not exist is still "no runner".
	idx := filepath.Join(root, ".provenance", "torrent", "queue", "live.json")
	if werr := os.WriteFile(idx, []byte(`{"format":"`+LiveFormat+`","pid":999999,"socket":"/tmp/does-not-exist.sock"}`), 0o600); werr != nil {
		t.Fatalf("write stale index: %v", werr)
	}
	if _, err := LiveSnapshotFor(context.Background(), root); !errors.Is(err, ErrNoLiveRunner) {
		t.Fatalf("stale endpoint = %v, want ErrNoLiveRunner", err)
	}
}

// TestLiveSlowReaderCannotBlockShutdown: a reader that never finishes must not delay
// cancellation or the runner's exit.
func TestLiveSlowReaderCannotBlockShutdown(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(64<<10, 131)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	waitSeedPhase(t, store, entry.ID, SeedServing, 90*time.Second)

	raw, rerr := os.ReadFile(filepath.Join(store.Root(), ".provenance", "torrent", "queue", "live.json"))
	if rerr != nil {
		t.Fatalf("read index: %v", rerr)
	}
	sock := ""
	for _, field := range strings.Split(string(raw), `"`) {
		if strings.HasPrefix(field, "/") && strings.HasSuffix(field, "q.sock") {
			sock = field
		}
	}
	if sock == "" {
		t.Fatalf("no socket in index: %s", raw)
	}
	conn, derr := net.Dial("unix", sock)
	if derr != nil {
		t.Fatalf("dial: %v", derr)
	}
	defer func() { _ = conn.Close() }()
	// Send nothing: this reader is stuck before its request even starts.

	// Malformed requests are refused without touching anything.
	liveQuery(t, store.Root()) // still answers other readers
	bad, berr := net.Dial("unix", sock)
	if berr != nil {
		t.Fatalf("dial 2: %v", berr)
	}
	_, _ = bad.Write([]byte("not json\n"))
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 128)
	n, _ := bad.Read(buf)
	_ = bad.Close()
	if !strings.Contains(string(buf[:n]), "unsupported request") {
		t.Fatalf("malformed request response = %q", string(buf[:n]))
	}
	before := mustGet(t, store, entry.ID)

	start := time.Now()
	cancel()
	select {
	case <-runnerDone:
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("shutdown took %s with a stuck reader attached", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a stuck reader blocked runner shutdown")
	}
	after := mustGet(t, store, entry.ID)
	if after.Desired != before.Desired || after.Capture == nil && before.Capture != nil {
		t.Fatalf("live traffic mutated the record: %+v", after)
	}
	if _, err := LiveSnapshotFor(context.Background(), store.Root()); !errors.Is(err, ErrNoLiveRunner) {
		t.Fatalf("after shutdown the endpoint still answers: %v", err)
	}
}

// TestLiveAfterRunnerDeath: killed with the endpoint present, a query reports no
// runner; a second runner publishes its own endpoint safely.
func TestLiveAfterRunnerDeath(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(128<<10, 137)}, 0)
	entry := completedSeededEntry(t, store, seedA, "alpha/a.bin")
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	marker := filepath.Join(t.TempDir(), "seed-addr")
	helper := startHelper(t, root, "seed-serving", marker)
	waitMarkerAddr(t, marker, 90*time.Second)
	first := liveQuery(t, root)
	if first.PID == os.Getpid() {
		t.Fatalf("snapshot reported the test process instead of the child")
	}
	killHelper(t, helper)

	deadline := time.Now().Add(30 * time.Second)
	for {
		_, lerr := LiveSnapshotFor(context.Background(), root)
		if errors.Is(lerr, ErrNoLiveRunner) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a killed runner still answered: %v", lerr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_, _ = NewRunner(store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	defer func() { cancel(); <-runnerDone }()
	deadline = time.Now().Add(90 * time.Second)
	for {
		snap, lerr := LiveSnapshotFor(context.Background(), root)
		if lerr == nil {
			if snap.PID == os.Getpid() {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the recovered runner never published an endpoint: %v", lerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
