package torrentqueue

// BT-11: the opt-in watch session. Plain `queue run` stays a finite drain; with
// Watch set the same loop keeps scanning after either pool goes idle, so work saved
// by another process (the CLI here, the TUI in production) is claimed without a
// restart. Every assertion below is driven by an explicit snapshot or channel, never
// by a sleep long enough to "probably" work.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const watchPoll = 20 * time.Millisecond

var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
	cliDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if cliDir != "" {
		_ = os.RemoveAll(cliDir)
	}
	os.Exit(code)
}

// testCLI builds the real binary once per test binary: the "separate process" the
// contract names must be able to write the queue while a runner owns it.
func testCLI(t *testing.T) string {
	t.Helper()
	cliOnce.Do(func() {
		dir, err := os.MkdirTemp("", "prov-watch-cli")
		if err != nil {
			cliErr = err
			return
		}
		bin := filepath.Join(dir, "provenance")
		// Bounded: a hung build must fail the test, not the package timeout. The
		// child is not built with -race, so only the test process is instrumented.
		buildCtx, cancelBuild := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancelBuild()
		cmd := exec.CommandContext(buildCtx, "go", "build", "-o", bin, "./cmd/provenance")
		cmd.Dir = "../.."
		if out, berr := cmd.CombinedOutput(); berr != nil {
			cliErr = fmt.Errorf("build cli: %v: %s", berr, out)
			return
		}
		cliPath, cliDir = bin, dir
	})
	if cliErr != nil {
		t.Fatalf("setup: %v", cliErr)
	}
	return cliPath
}

// runCLI performs one ordinary CLI queue operation against a root, as a separate
// process would.
func runCLI(t *testing.T, root string, args ...string) string {
	t.Helper()
	bin := testCLI(t)
	full := append([]string{"torrent", "queue"}, args...)
	full = append(full, "-o", root)
	cliCtx, cancelCLI := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelCLI()
	cmd := exec.CommandContext(cliCtx, bin, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup: cli %v: %v: %s", args, err, out)
	}
	return string(out)
}

// queueList reads the durable queue the way `queue list --json` does.
func queueList(t *testing.T, root string) []Entry {
	t.Helper()
	out := runCLI(t, root, "list", "--json")
	// `queue list --json` emits {"runner_held":bool,"entries":[...]}.
	var doc struct {
		RunnerHeld bool    `json:"runner_held"`
		Entries    []Entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("setup: parse queue list: %v: %s", err, out)
	}
	return doc.Entries
}

// matchedFixture builds a torrent from the directory the peer serves and asserts the
// two identities agree, then returns the .torrent path and the peer address.
func matchedFixture(t *testing.T, size int, upload int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	writeFixtureFile(t, dir, "fixture", "b.bin", patternedBytes(size, 197))
	info := metainfo.Info{PieceLength: 16 << 10, Name: "fixture"}
	if err := info.BuildFromFilePath(filepath.Join(dir, "fixture")); err != nil {
		t.Fatalf("setup: build info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	torrentPath := filepath.Join(t.TempDir(), "b.torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatalf("setup: create torrent: %v", err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatalf("setup: write torrent: %v", err)
	}
	_ = f.Close()
	peer := startProbeSeeder(t, dir, &mi, upload)
	loaded, err := metainfo.LoadFromFile(torrentPath)
	if err != nil {
		t.Fatalf("setup: load torrent: %v", err)
	}
	if loaded.HashInfoBytes() != mi.HashInfoBytes() {
		t.Fatalf("setup: torrent identity does not match the peer")
	}
	return torrentPath, peer
}

// addFileEntry records a download entry for a local .torrent, as `queue add` does.
func addFileEntry(t *testing.T, root, torrentPath, peer string) Entry {
	t.Helper()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	entry, _, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "file", Path: torrentPath},
		RawSource: torrentPath,
		Peers:     []string{peer},
		Discovery: "direct",
		Selection: []string{"fixture/b.bin"},
	})
	if aerr != nil {
		t.Fatalf("setup: add entry: %v", aerr)
	}
	return entry
}

// completeSeedEntry finishes one small entry through the ordinary queue path and
// enables its seed intent, so a seed slot has real, verified work to do.
func completeSeedEntry(t *testing.T, store *Store) Entry {
	t.Helper()
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(96<<10, 199)}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin")
	startEntry(t, store, entry.ID)
	if sum, rerr := NewRunner(store, realAdapter(), Options{Log: nil}).Run(context.Background()); rerr != nil || sum.Completed != 1 {
		t.Fatalf("setup: entry did not complete: %+v %v", sum, rerr)
	}
	done := mustGet(t, store, entry.ID)
	if done.Phase != PhaseCompleted || done.Capture == nil || done.Workspace == "" {
		t.Fatalf("setup: entry is not seed-eligible: %+v", done)
	}
	if _, serr := store.EnableSeed(entry.ID); serr != nil {
		t.Fatalf("setup: enable seed intent: %v", serr)
	}
	return mustGet(t, store, entry.ID)
}

// logBuffer captures runner output so a test can synchronise on a completed scan
// rather than inferring one from an empty snapshot.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), s)
}

// waitLog waits for a log line, which is the only signal that can distinguish "the
// scheduler completed an empty scan" from "nothing has been attempted yet".
func waitLog(t *testing.T, b *logBuffer, want string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if b.contains(want) {
			return
		}
		time.Sleep(watchPoll)
	}
	t.Fatalf("the runner never logged %q", want)
}

// breakEntriesDir makes the queue's entry directory unlistable, so the next store
// read fails the way a real permission or I/O error would. The returned function
// restores it and is safe to call twice.
func breakEntriesDir(t *testing.T, root string) func() {
	t.Helper()
	dir := filepath.Join(root, ".provenance", "torrent", "queue", "entries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: mkdir entries: %v", err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("setup: chmod entries: %v", err)
	}
	return func() { _ = os.Chmod(dir, 0o755) }
}

// watchSession is one running session plus the bound around its cancellation.
type watchSession struct {
	cancel  context.CancelFunc
	done    chan struct{}
	tried   bool
	stopped bool
}

func startWatch(t *testing.T, store *Store, transfer Transfer, opts Options) *watchSession {
	t.Helper()
	opts.Watch = true
	if opts.PollInterval == 0 {
		opts.PollInterval = watchPoll
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &watchSession{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		_, _ = NewRunner(store, transfer, opts).Run(ctx)
	}()
	t.Cleanup(func() {
		w.stop(t, 60*time.Second)
	})
	return w
}

// stop cancels the session and waits for it once, with a bound.
func (w *watchSession) stop(t *testing.T, limit time.Duration) bool {
	t.Helper()
	if w.tried {
		return w.stopped
	}
	w.tried = true
	w.cancel()
	select {
	case <-w.done:
		w.stopped = true
	case <-time.After(limit):
	}
	return w.stopped
}

// alive reports whether the session is still running.
func (w *watchSession) alive() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

func TestWatchClaimsLateDownloadWhileSeeding(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	entryA := completeSeedEntry(t, store)
	torrentPath, peer := matchedFixture(t, 6<<20, 512<<10)
	entryB := addFileEntry(t, root, torrentPath, peer)

	logs := &logBuffer{}
	w := startWatch(t, store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, Log: logs})

	// An empty snapshot alone is not a scan barrier: wait until the pool has reported
	// a completed scan that found nothing, then confirm the snapshot agrees.
	waitLog(t, logs, "idle: watching for eligible work", 60*time.Second)
	idle, ok := waitSnapshot(t, root, 60*time.Second, func(s LiveSnapshot) bool {
		if attemptPhase(s, entryA.ID, "seed") != "serving" || s.Seeds.Used != 1 {
			return false
		}
		for _, a := range s.Attempts {
			if a.Kind == "download" {
				return false
			}
		}
		return s.Downloads.Used == 0
	})
	if !ok {
		t.Fatalf("setup: the download pool never went idle while A served (A=%+v)", mustGet(t, store, entryA.ID))
	}
	pid := idle.PID

	// A separate process starts B through the ordinary CLI.
	runCLI(t, root, "start", entryB.ID)

	claimed, ok := waitSnapshot(t, root, 10*time.Second, func(s LiveSnapshot) bool {
		return s.PID == pid && attemptPhase(s, entryB.ID, "download") == "running"
	})
	if !ok {
		t.Fatalf("the same runner did not claim B after an external start (B=%+v)", mustGet(t, store, entryB.ID))
	}
	if claimed.PID != pid {
		t.Fatalf("a different runner claimed B: pid %d, want %d", claimed.PID, pid)
	}

	// One snapshot with both pools occupied by exactly the two expected attempts.
	both, ok := waitSnapshot(t, root, 30*time.Second, func(s LiveSnapshot) bool {
		return attemptPhase(s, entryA.ID, "seed") == "serving" &&
			attemptPhase(s, entryB.ID, "download") == "running" &&
			s.Seeds.Used == 1 && s.Seeds.Max == 1 &&
			s.Downloads.Used == 1 && s.Downloads.Max == 1 &&
			len(s.Attempts) == 2
	})
	if !ok {
		t.Fatalf("no single snapshot showed the late download beside the seed; A=%+v B=%+v",
			mustGet(t, store, entryA.ID), mustGet(t, store, entryB.ID))
	}
	t.Logf("WATCH overlap pid=%d downloads=%d/%d seeds=%d/%d attempts=%d",
		both.PID, both.Downloads.Used, both.Downloads.Max, both.Seeds.Used, both.Seeds.Max, len(both.Attempts))
	for _, a := range both.Attempts {
		t.Logf("  WATCH attempt entry=%s kind=%s phase=%s", a.EntryID, a.Kind, a.Phase)
	}

	if !w.stop(t, 60*time.Second) {
		t.Fatal("the watch session did not stop")
	}
	if _, lerr := liveProbe(t, root); lerr == nil {
		t.Fatal("the endpoint still answers after the session ended")
	}
}

func TestWatchIdlePicksUpNewWorkInBothPools(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	// Prepared before the session starts: a completed entry whose seed intent is off.
	seedEntry := completeSeedEntry(t, store)
	if _, serr := store.SetSeedIntent(seedEntry.ID, false); serr != nil {
		t.Fatalf("setup: clear seed intent: %v", serr)
	}

	w := startWatch(t, store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, Log: nil})

	// An empty queue is an idle session, not an exit condition.
	time.Sleep(10 * watchPoll)
	if !w.alive() {
		t.Fatal("the watch session exited with an empty queue")
	}
	snap, perr := liveProbe(t, root)
	if perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("the idle session is not discoverable: %v", perr)
	}
	if len(snap.Attempts) != 0 || snap.Downloads.Used != 0 || snap.Seeds.Used != 0 {
		t.Fatalf("idle snapshot = %+v", snap)
	}

	// Work saved by a separate process is claimed without a restart.
	torrentPath, peer := matchedFixture(t, 96<<10, 0)
	started := time.Now()
	runCLI(t, root, "add", torrentPath, "--select", "fixture/b.bin", "--peer", peer, "--discovery", "direct")
	var downloadID string
	for _, e := range queueList(t, root) {
		if e.Source.Kind == "file" {
			downloadID = e.ID
		}
	}
	if downloadID == "" {
		t.Fatal("setup: the CLI add produced no entry")
	}
	runCLI(t, root, "start", downloadID)
	_, ok := waitSnapshot(t, root, 10*time.Second, func(s LiveSnapshot) bool {
		return attemptPhase(s, downloadID, "download") == "running"
	})
	if !ok {
		t.Fatalf("the idle session did not claim a download added by another process (%+v)", mustGet(t, store, downloadID))
	}
	t.Logf("WATCH pickup: download claimed after %s (poll %s)", time.Since(started).Round(time.Millisecond), watchPoll)

	// An idle seed pool wakes too: enabling intent from outside starts a seed.
	runCLI(t, root, "seed", seedEntry.ID)
	serving, ok := waitSnapshot(t, root, 30*time.Second, func(s LiveSnapshot) bool {
		return attemptPhase(s, seedEntry.ID, "seed") == "serving" && s.Seeds.Used == 1
	})
	if !ok {
		t.Fatalf("the idle seed pool did not start a seed enabled by another process (%+v)", mustGet(t, store, seedEntry.ID))
	}
	t.Logf("WATCH seed pool woke: seeds=%d/%d attempts=%d", serving.Seeds.Used, serving.Seeds.Max, len(serving.Attempts))

	if !w.stop(t, 60*time.Second) {
		t.Fatal("the watch session did not stop")
	}
}

func TestDrainDoesNotClaimLateDownloadButNextPassDoes(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	entryA := completeSeedEntry(t, store)
	torrentPath, peer := matchedFixture(t, 64<<10, 0)
	entryB := addFileEntry(t, root, torrentPath, peer)

	// A plain drain: it finishes downloads, then waits on the seed pool.
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = NewRunner(store, realAdapter(), Options{MaxSeeds: 1, PollInterval: watchPoll, Log: nil}).Run(ctx)
	}()
	stopDrain := func(limit time.Duration) bool {
		cancel()
		select {
		case <-runDone:
			return true
		case <-time.After(limit):
			return false
		}
	}
	t.Cleanup(func() { stopDrain(60 * time.Second) })

	if _, ok := waitSnapshot(t, root, 60*time.Second, func(s LiveSnapshot) bool {
		return attemptPhase(s, entryA.ID, "seed") == "serving" && s.Downloads.Used == 0
	}); !ok {
		t.Fatalf("setup: the drain never reached serving with an idle download pool")
	}

	runCLI(t, root, "start", entryB.ID)
	time.Sleep(15 * watchPoll)
	if got := mustGet(t, store, entryB.ID); got.Phase != PhaseQueued {
		t.Fatalf("a drain claimed a download started after its pool drained: %+v", got)
	}
	// An exited runner must not pass this check: require the drain to still be
	// observable before asserting that it did not claim B.
	snap, perr := liveProbe(t, root)
	if perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("the drain was no longer running after B was started: %v", perr)
	}
	for _, a := range snap.Attempts {
		if a.EntryID == entryB.ID {
			t.Fatalf("the drain started B although its download pool had drained: %+v", a)
		}
	}

	if !stopDrain(60 * time.Second) {
		t.Fatal("the drain did not stop")
	}

	// The next invocation claims it.
	sum, rerr := NewRunner(store, realAdapter(), Options{PollInterval: watchPoll, Log: nil}).Run(context.Background())
	if rerr != nil || sum.Completed != 1 {
		t.Fatalf("second pass: %+v %v", sum, rerr)
	}
	if got := mustGet(t, store, entryB.ID); got.Phase != PhaseCompleted {
		t.Fatalf("second pass left B at %q", got.Phase)
	}
}

func TestWatchShutdownWhileIdle(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	w := startWatch(t, store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 1, Log: nil})
	if _, ok := waitSnapshot(t, root, 10*time.Second, func(s LiveSnapshot) bool { return len(s.Attempts) == 0 }); !ok {
		dumpEndpoint(t, root)
		t.Fatal("the idle session never became discoverable")
	}
	if !w.stop(t, 60*time.Second) {
		t.Fatal("an idle session did not stop on cancellation")
	}
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the endpoint still answers after Run returned")
	}
	if probe, lerr := OpenStore(root); lerr == nil {
		l, e := probe.LockRunner()
		if e != nil {
			t.Fatalf("ownership is still held after Run returned: %v", e)
		}
		_ = l.Release()
	}
}

func TestWatchShutdownHeldCloseKeepsSlot(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	entry := seedableEntry(t, store)
	sess := newBarrierSession("W-held", entry.ID)
	t.Cleanup(func() { sess.release(t, "cleanup") })
	opener := &barrierOpener{Adapter: realAdapter(), sess: sess}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = NewRunner(store, opener, Options{Watch: true, MaxActive: 1, MaxSeeds: 1, PollInterval: watchPoll, Log: nil}).Run(ctx)
	}()
	t.Cleanup(func() {
		sess.release(t, "cleanup")
		cancel()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Errorf("Run did not exit during cleanup")
		}
	})

	waitSeedPhase(t, store, entry.ID, SeedServing, 60*time.Second)

	// Disable the seed from another process: the session must stop serving it while
	// staying alive itself.
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-sess.closeIn:
	case <-time.After(60 * time.Second):
		t.Fatal("Close was never entered")
	}

	select {
	case <-runDone:
		t.Fatal("Run returned while Close was held")
	case <-time.After(300 * time.Millisecond):
	}
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatal("runner ownership was free while an attempt was closing")
	}
	snap, perr := liveProbe(t, root)
	if perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("live probe during Close: %v (closes=%d releases=%d)", perr, sess.closes.Load(), sess.releases.Load())
	}
	if len(snap.Attempts) != 1 || !snap.Attempts[0].Stopping || snap.Attempts[0].Phase != "stopping" {
		t.Fatalf("during Close the attempts = %+v", snap.Attempts)
	}
	if snap.Seeds.Used != 1 {
		t.Fatalf("a closing attempt released its slot: %+v", snap.Seeds)
	}
	t.Logf("WATCH held Close: stopping with slot used %d/%d, ownership refused", snap.Seeds.Used, snap.Seeds.Max)

	// Cancellation must not shortcut the teardown either: the slot and ownership stay
	// held until Close actually returns.
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned after cancellation while Close was still held")
	case <-time.After(300 * time.Millisecond):
	}
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatal("runner ownership was released by cancellation before Close returned")
	}

	sess.release(t, "explicit")
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the session did not return after Close")
	}
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the endpoint still answered after Run returned")
	}
	if probe, lerr := OpenStore(root); lerr == nil {
		l, e := probe.LockRunner()
		if e != nil {
			t.Fatalf("ownership still held after Run returned: %v", e)
		}
		_ = l.Release()
	}
}

// TestSeedPoolEndsOnStoreError drives the seed pool directly: a store it cannot read
// is a failure, not "no work", so the pool must return and hand the error to the
// session instead of logging and polling again forever.
func TestSeedPoolEndsOnStoreError(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	restore := breakEntriesDir(t, root)
	defer restore()

	fatal := &runFatal{}
	r := NewRunner(store, realAdapter(), Options{Watch: true, MaxSeeds: 1, PollInterval: watchPoll, Log: nil})
	var mu sync.Mutex
	var sum Summary
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runSeeds(context.Background(), &mu, &sum, fatal)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the seed pool kept polling after a store error")
	}
	ferr := fatal.get()
	if ferr == nil {
		t.Fatal("the seed pool returned without recording the error")
	}
	if !strings.Contains(ferr.Error(), "seed") {
		t.Fatalf("the pool reported an unrelated error: %v", ferr)
	}
	t.Logf("seed pool ended with: %v", ferr)
}

// TestWatchFatalDownloadErrorJoinsHeldSeed: a fatal download-store error must cancel
// the seed pool and join it, not return out of Run while a seed still owns a slot.
func TestWatchFatalDownloadErrorJoinsHeldSeed(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	entry := seedableEntry(t, store)
	sess := newBarrierSession("W-fatal-download", entry.ID)
	t.Cleanup(func() { sess.release(t, "cleanup") })
	opener := &barrierOpener{Adapter: realAdapter(), sess: sess}

	fatalSeen := make(chan error, 1)
	var runErr error
	runDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(runDone)
		_, runErr = NewRunner(store, opener, Options{
			Watch: true, MaxActive: 1, MaxSeeds: 1, PollInterval: watchPoll, Log: nil,
			BeforeFatal: func(err error) {
				select {
				case fatalSeen <- err:
				default:
				}
			},
		}).Run(ctx)
	}()
	t.Cleanup(func() {
		sess.release(t, "cleanup")
		cancel()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Errorf("Run did not exit during cleanup")
		}
	})

	waitSeedPhase(t, store, entry.ID, SeedServing, 60*time.Second)

	// Stop the seed and hold its Close: the seed slot stays occupied from here on.
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-sess.closeIn:
	case <-time.After(60 * time.Second):
		t.Fatal("Close was never entered")
	}

	restore := breakEntriesDir(t, root)
	t.Cleanup(restore)

	select {
	case ferr := <-fatalSeen:
		t.Logf("fatal download error recorded: %v", ferr)
	case <-runDone:
		t.Fatalf("Run returned while the seed was still inside Close (err=%v)", runErr)
	case <-time.After(15 * time.Second):
		t.Fatal("the download pool never reported the store error")
	}

	// The fatal error must not shortcut teardown: Run is joining a seed inside Close.
	select {
	case <-runDone:
		t.Fatalf("Run returned while the seed was still inside Close (err=%v)", runErr)
	case <-time.After(300 * time.Millisecond):
	}
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatal("runner ownership was released while an attempt was closing")
	}
	snap, perr := liveProbe(t, root)
	if perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("live probe after the fatal error: %v", perr)
	}
	if len(snap.Attempts) != 1 || !snap.Attempts[0].Stopping || snap.Attempts[0].Phase != "stopping" {
		t.Fatalf("attempts after the fatal error = %+v", snap.Attempts)
	}
	if snap.Seeds.Used != 1 {
		t.Fatalf("the slot was released before Close returned: %+v", snap.Seeds)
	}
	t.Logf("fatal error held: stopping seed still occupies %d/%d, ownership refused", snap.Seeds.Used, snap.Seeds.Max)

	restore()
	sess.release(t, "explicit")
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return after Close was released")
	}
	if runErr == nil {
		t.Fatal("Run returned success after a fatal store error")
	}
	if errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run returned the sibling's cancellation instead of the cause: %v", runErr)
	}
	t.Logf("Run returned the injected error: %v", runErr)

	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the endpoint still answered after Run returned")
	}
	if probe, lerr := OpenStore(root); lerr == nil {
		l, e := probe.LockRunner()
		if e != nil {
			t.Fatalf("ownership still held after Run returned: %v", e)
		}
		_ = l.Release()
	}
}

// TestWatchFatalStoreErrorEndsIdleSession: with no work at all, a store that becomes
// unreadable must end the session with an error, not leave it polling or return
// success. Either pool may be the first reporter; both are fatal by design.
func TestWatchFatalStoreErrorEndsIdleSession(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	logs := &logBuffer{}
	var runErr error
	runDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(runDone)
		_, runErr = NewRunner(store, realAdapter(), Options{
			Watch: true, MaxActive: 1, MaxSeeds: 1, PollInterval: watchPoll, Log: logs,
		}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Errorf("Run did not exit during cleanup")
		}
	})

	waitLog(t, logs, "idle: watching for eligible work", 30*time.Second)
	restore := breakEntriesDir(t, root)
	t.Cleanup(restore)

	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the idle session kept polling after a fatal store error")
	}
	if runErr == nil {
		t.Fatal("Run returned success although the store became unreadable")
	}
	if errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run returned a cancellation instead of the cause: %v", runErr)
	}
	t.Logf("idle watch ended with: %v", runErr)
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the endpoint still answered after Run returned")
	}
}
