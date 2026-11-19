package torrentqueue

// BT-08d.3: managed seeding inside the foreground queue.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// watchedSeed publishes the serving address and keeps the live counter readable, so a
// test can prove bytes moved rather than that a phase was recorded.
type watchedSeed struct {
	SeedSession
	addr     chan<- string
	uploaded *atomic.Int64
	closed   *atomic.Bool
}

func (w *watchedSeed) Serve(ctx context.Context) error {
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if addr := w.SeedSession.Address(); addr != "" {
				select {
				case w.addr <- addr:
				default:
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		for ctx.Err() == nil {
			w.uploaded.Store(w.SeedSession.UploadedBytes())
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return w.SeedSession.Serve(ctx)
}

func (w *watchedSeed) Close() error {
	defer w.closed.Store(true)
	return w.SeedSession.Close()
}

// seedWatchingAdapter wraps the real adapter and hands back watched sessions.
type seedWatchingAdapter struct {
	Adapter
	addr     chan string
	uploaded *atomic.Int64
	closed   *atomic.Bool
	opened   *atomic.Int32
}

func (a *seedWatchingAdapter) OpenSeed(entry Entry, req Request) (SeedSession, error) {
	sess, err := a.Adapter.OpenSeed(entry, req)
	if err != nil {
		return nil, err
	}
	a.opened.Add(1)
	return &watchedSeed{SeedSession: sess, addr: a.addr, uploaded: a.uploaded, closed: a.closed}, nil
}

// peerAddr is a torrent.PeerRemoteAddr for a loopback seed.
type peerAddr struct {
	host string
	port int
}

func (p peerAddr) Network() string { return "tcp" }
func (p peerAddr) String() string  { return net.JoinHostPort(p.host, strconv.Itoa(p.port)) }

// leechFrom pulls the whole torrent from one seed and reports its own verdict.
func leechFrom(t *testing.T, addr string, mi *metainfo.MetaInfo, dir string) (*at.Torrent, *at.Client) {
	t.Helper()
	cfg := at.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisablePEX = true, true, true
	cfg.DisableUTP, cfg.DisableIPv6, cfg.DisableWebtorrent = true, true, true
	cfg.DisableWebseeds, cfg.NoDefaultPortForwarding = true, true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("leecher client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("leecher add: %v", err)
	}
	_, portStr, perr := net.SplitHostPort(addr)
	if perr != nil {
		t.Fatalf("leecher: bad address %q: %v", addr, perr)
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		t.Fatalf("leecher: bad port in %q", addr)
	}
	tor.AddPeers([]at.PeerInfo{{Addr: peerAddr{host: "127.0.0.1", port: port}}})
	tor.DownloadAll()
	return tor, client
}

// TestQueueSeedServesWhileDownloadCompletes is the release gate: one download slot and
// one seed slot. Entry A has completed and is serving a controlled leecher; entry B must
// finish downloading while A is still serving, and A's payload counter must advance.
func TestQueueSeedServesWhileDownloadCompletes(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(512<<10, 7)}, 0)
	entryA := enqueue(t, store, seedA, "alpha/a.bin")
	startEntry(t, store, entryA.ID)

	if sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background()); rerr != nil || sum.Completed != 1 {
		t.Fatalf("first drain: %+v, %v", sum, rerr)
	}
	done, err := store.Get(entryA.ID)
	if err != nil || done.Phase != PhaseCompleted || done.Capture == nil {
		t.Fatalf("A did not complete: %+v, %v", done, err)
	}
	if _, err := store.SetSeedIntent(entryA.ID, true); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// B is deliberately slow, so its download overlaps A's serving window.
	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(2<<20, 11)}, 192<<10)
	entryB := enqueue(t, store, seedB, "beta/b.bin")
	startEntry(t, store, entryB.ID)

	var uploaded atomic.Int64
	var closedFlag atomic.Bool
	var opened atomic.Int32
	adapter := &seedWatchingAdapter{
		Adapter:  realAdapter(),
		addr:     make(chan string, 1),
		uploaded: &uploaded,
		closed:   &closedFlag,
		opened:   &opened,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, adapter, Options{MaxActive: 1, MaxSeeds: 1, Log: os.Stderr}).Run(ctx)
		runDone <- sum
	}()

	var addr string
	select {
	case addr = <-adapter.addr:
	case <-time.After(90 * time.Second):
		cancel()
		t.Fatalf("the seed never started serving; entry=%+v", mustGet(t, store, entryA.ID))
	}
	leech, _ := leechFrom(t, addr, seedMetainfo(t, seedA), t.TempDir())

	// A must actually serve bytes while B is still downloading.
	deadline := time.Now().Add(90 * time.Second)
	for uploaded.Load() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no payload served; seed entry=%+v leecher=%d bytes", mustGet(t, store, entryA.ID), torBytes(leech))
		}
		time.Sleep(20 * time.Millisecond)
	}
	servedWhileDownloading := uploaded.Load()
	bDuring := mustGet(t, store, entryB.ID)
	if bDuring.Phase == PhaseCompleted {
		cancel()
		t.Fatalf("B completed before A served anything; the overlap was not exercised")
	}

	// B must publish before A stops.
	for {
		b := mustGet(t, store, entryB.ID)
		if b.Phase == PhaseCompleted {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("B did not complete while A served: B=%+v A=%+v", b, mustGet(t, store, entryA.ID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	aAtBPublish := mustGet(t, store, entryA.ID)
	if closedFlag.Load() {
		t.Fatalf("A stopped serving before B published")
	}
	if aAtBPublish.SeedPhase != SeedServing {
		t.Fatalf("A's seed phase at B's completion = %q, want serving", aAtBPublish.SeedPhase)
	}
	// The serving window must overlap B's download. A may already have finished its
	// whole fixture before this check - 512 KiB is small - so an unchanged counter is
	// not a failure once the leecher has verified every piece; what matters is that the
	// bytes were served while B was still downloading, asserted above.
	if uploaded.Load() <= servedWhileDownloading && !torComplete(leech) {
		t.Fatalf("A's payload counter did not advance while B downloaded (%d -> %d) and the leecher has not finished",
			servedWhileDownloading, uploaded.Load())
	}
	t.Logf("A served %d bytes while B downloaded; A phase=%q at B's completion; leecher read %d",
		uploaded.Load(), aAtBPublish.SeedPhase, torBytes(leech))

	cancel()
	select {
	case sum := <-runDone:
		t.Logf("runner summary: completed=%d seeded=%v", sum.Completed, sum.Seeded)
	case <-time.After(120 * time.Second):
		t.Fatal("runner did not return after cancellation")
	}
	// Intent survives an interruption, and the stop was recorded as an observation.
	final := mustGet(t, store, entryA.ID)
	if final.SeedDesired != SeedOn {
		t.Fatalf("interruption cleared intent: %+v", final)
	}
	if final.SeedPhase != SeedIdle {
		t.Fatalf("phase after an interrupted seed = %q, want idle (intent kept)", final.SeedPhase)
	}
	if !closedFlag.Load() {
		t.Fatal("the seed session was never closed")
	}
	assertLockFree(t, store, entryA)
}

// TestQueueSeedCommandRules: intent is off by default, cannot be requested for an
// incomplete entry, and unseed does not touch download intent.
func TestQueueSeedCommandRules(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "gamma", map[string][]byte{"g.bin": patternedBytes(64<<10, 3)}, 0)
	entry := enqueue(t, store, seed, "gamma/g.bin")
	if entry.SeedDesired != "" && entry.SeedDesired != SeedOff {
		t.Fatalf("a new entry defaults to seeding %q, want off", entry.SeedDesired)
	}
	if _, serr := store.SetSeedIntent(entry.ID, true); serr == nil {
		t.Fatal("seeding an incomplete entry was accepted")
	}
	startEntry(t, store, entry.ID)
	if _, rerr := NewRunner(store, realAdapter(), Options{}).Run(context.Background()); rerr != nil {
		t.Fatalf("drain: %v", rerr)
	}
	before := mustGet(t, store, entry.ID)
	if _, serr := store.SetSeedIntent(entry.ID, true); serr != nil {
		t.Fatalf("seed intent: %v", serr)
	}
	on := mustGet(t, store, entry.ID)
	if on.SeedDesired != SeedOn || on.Desired != before.Desired {
		t.Fatalf("seed intent changed download intent or did not persist: %+v", on)
	}
	off := func() Entry {
		if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
			t.Fatalf("unseed: %v", serr)
		}
		return mustGet(t, store, entry.ID)
	}()
	if off.SeedDesired != SeedOff || off.Desired != before.Desired {
		t.Fatalf("unseed changed download intent: %+v", off)
	}
	if off.SeedPhase != SeedStopped {
		t.Fatalf("unseeded phase = %q, want stopped", off.SeedPhase)
	}
}

func mustGet(t *testing.T, store *Store, id string) Entry {
	t.Helper()
	entry, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return entry
}

// seedMetainfo is the seeder's metainfo, normalised for another client.
func seedMetainfo(t *testing.T, s *loopbackSeeder) *metainfo.MetaInfo {
	t.Helper()
	cp := *s.mi
	if len(cp.PieceLayers) == 0 {
		cp.PieceLayers = nil
	}
	return &cp
}

// torBytes is what a leecher has read as payload.
// torComplete reports whether every piece of a torrent is present.
func torComplete(tor *at.Torrent) bool {
	for i := 0; i < tor.NumPieces(); i++ {
		if !tor.Piece(i).State().Complete {
			return false
		}
	}
	return true
}

func torBytes(tor *at.Torrent) int64 {
	stats := tor.Stats()
	return stats.BytesReadUsefulData.Int64()
}

// assertLockFree proves the workspace is acquirable again after a seed stopped.
func assertLockFree(t *testing.T, store *Store, entry Entry) {
	t.Helper()
	ws, err := bittorrent.WorkspacePathForJob(store.Root(), entry.JobID)
	if err != nil {
		t.Fatalf("workspace path: %v", err)
	}
	// The lock file must not be held: a probe acquire must not report ErrLocked.
	probe, perr := os.OpenFile(filepath.Join(ws, ".provenance", "torrent", "staging.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if perr != nil {
		return // no lock file at all is also free
	}
	_ = probe.Close()
	sess, serr := bittorrent.OpenSeedSession(context.Background(),
		bittorrent.Config{StagingDir: ws, Discovery: bittorrent.DiscoveryDirect}, entry.JobID)
	if serr != nil {
		t.Fatalf("workspace still owned after the seed stopped: %v", serr)
	}
	_ = sess.Close()
}
