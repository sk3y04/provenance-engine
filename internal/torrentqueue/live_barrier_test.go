package torrentqueue

// BT-10a.2: isolate the boundary at which the live Close snapshot fails, by comparing
// a fresh root with a root where a prior Run already published and closed an endpoint.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// barrierSession is a seed whose Close signals entry and then waits for an explicit
// release. Only the test releases it; runner cancellation never does.
type barrierSession struct {
	id         string
	entry      string
	closeIn    chan struct{}
	allowRet   chan struct{}
	closeInOne sync.Once
	closes     atomic.Int32
	releases   atomic.Int32
}

func newBarrierSession(id, entry string) *barrierSession {
	return &barrierSession{id: id, entry: entry, closeIn: make(chan struct{}), allowRet: make(chan struct{})}
}

func (b *barrierSession) trace(event string) {
	fmt.Fprintf(os.Stderr, "TRACE session=%s entry=%s event=%s closes=%d releases=%d\n",
		b.id, b.entry, event, b.closes.Load(), b.releases.Load())
}

func (b *barrierSession) Serve(ctx context.Context) error {
	b.trace("serve-enter")
	<-ctx.Done()
	b.trace("serve-return")
	return ctx.Err()
}

func (b *barrierSession) Address() string      { return "127.0.0.1:1" }
func (b *barrierSession) UploadedBytes() int64 { return 0 }

func (b *barrierSession) Close() error {
	b.closes.Add(1)
	b.closeInOne.Do(func() { close(b.closeIn) })
	b.trace("close-enter")
	<-b.allowRet
	b.releases.Add(1)
	b.trace("close-return")
	return nil
}

func (b *barrierSession) release(t *testing.T, why string) {
	t.Helper()
	select {
	case <-b.allowRet:
		return
	default:
	}
	b.releases.Add(1)
	close(b.allowRet)
	b.trace("released:" + why)
}

type barrierOpener struct {
	Adapter
	sess *barrierSession
	n    atomic.Int32
}

func (o *barrierOpener) OpenSeed(entry Entry, req Request) (SeedSession, error) {
	o.n.Add(1)
	fmt.Fprintf(os.Stderr, "TRACE session=%s entry=%s event=open call=%d\n", o.sess.id, entry.ID, o.n.Load())
	return o.sess, nil
}

func seedableEntry(t *testing.T, store *Store) Entry {
	t.Helper()
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(64<<10, 151)}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin")
	if _, uerr := store.Update(entry.ID, func(e *Entry) error {
		e.Phase = PhaseCompleted
		e.Capture = &CaptureRef{Key: "capture-fixture"}
		e.SeedDesired = SeedOn
		e.SeedPhase = SeedWaiting
		return nil
	}); uerr != nil {
		t.Fatalf("make completed: %v", uerr)
	}
	return mustGet(t, store, entry.ID)
}

// liveText reports the snapshot and, on failure, everything needed to see which
// endpoint the query reached.
func liveProbe(t *testing.T, root string) (LiveSnapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return LiveSnapshotFor(ctx, root)
}

func dumpEndpoint(t *testing.T, root string) {
	t.Helper()
	idx := filepath.Join(root, ".provenance", "torrent", "queue", "live.json")
	raw, err := os.ReadFile(idx)
	if err != nil {
		t.Logf("DUMP index: %v", err)
		return
	}
	t.Logf("DUMP index: %s", raw)
	var parsed struct {
		Socket string `json:"socket"`
		PID    int    `json:"pid"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.Socket == "" {
		return
	}
	if _, serr := os.Stat(parsed.Socket); serr != nil {
		t.Logf("DUMP socket stat: %v", serr)
	}
	conn, derr := net.Dial("unix", parsed.Socket)
	if derr != nil {
		t.Logf("DUMP dial: %v", derr)
		return
	}
	_, _ = conn.Write([]byte("{\"op\":\"snapshot\"}\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, rerr := conn.Read(buf)
	_ = conn.Close()
	t.Logf("DUMP direct read n=%d err=%v body=%s", n, rerr, string(buf[:n]))
}

// runBarrierCase starts one Run over a prepared entry and holds its Close.
func runBarrierCase(t *testing.T, root string, tag string) {
	t.Helper()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("%s: open store: %v", tag, err)
	}
	entry := seedableEntry(t, store)
	sess := newBarrierSession("S-"+tag, entry.ID)
	t.Cleanup(func() { sess.release(t, "cleanup") })
	opener := &barrierOpener{Adapter: realAdapter(), sess: sess}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = NewRunner(store, opener, Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: os.Stderr}).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-runDone })

	waitSeedPhase(t, store, entry.ID, SeedServing, 60*time.Second)

	// The endpoint must answer while the seed is serving, before any stop.
	if snap, perr := liveProbe(t, root); perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("%s: live probe while serving: %v", tag, perr)
	} else if len(snap.Attempts) != 1 || snap.Attempts[0].Kind != "seed" {
		t.Fatalf("%s: serving snapshot = %+v", tag, snap.Attempts)
	}

	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("%s: unseed: %v", tag, serr)
	}
	select {
	case <-sess.closeIn:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: Close never entered", tag)
	}

	// Close is held: Run must not return, ownership must not be free, and the endpoint
	// must show a stopping seed still holding its slot.
	select {
	case <-runDone:
		t.Fatalf("%s: Run returned while Close was held", tag)
	case <-time.After(300 * time.Millisecond):
	}
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatalf("%s: runner ownership was free while an attempt was closing", tag)
	}
	snap, perr := liveProbe(t, root)
	if perr != nil {
		dumpEndpoint(t, root)
		t.Fatalf("%s: live probe during Close: %v (closes=%d releases=%d)", tag, perr, sess.closes.Load(), sess.releases.Load())
	}
	if len(snap.Attempts) != 1 || !snap.Attempts[0].Stopping || snap.Attempts[0].Phase != "stopping" {
		t.Fatalf("%s: during Close the attempts = %+v", tag, snap.Attempts)
	}
	if snap.Seeds.Used != 1 {
		t.Fatalf("%s: closing attempt released its slot: %+v", tag, snap.Seeds)
	}
	t.Logf("%s: held Close observed as stopping with slot used 1/%d", tag, snap.Seeds.Max)

	sess.release(t, "explicit")
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: Run did not return after Close", tag)
	}
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatalf("%s: the endpoint still answered after Run returned", tag)
	}
	if probe, lerr := OpenStore(root); lerr == nil {
		if l, e := probe.LockRunner(); e == nil {
			_ = l.Release()
		} else {
			t.Fatalf("%s: ownership still held after Run returned: %v", tag, e)
		}
	}
}

func TestLiveCloseBarrierFreshRoot(t *testing.T) {
	runBarrierCase(t, t.TempDir(), "fresh")
}

func TestLiveCloseBarrierReusedRoot(t *testing.T) {
	root := t.TempDir()
	// One prior Run that publishes and cleanly closes an endpoint on this root.
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background()); rerr != nil {
		t.Fatalf("prior run: %v", rerr)
	}
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the prior run left a reachable endpoint")
	}
	runBarrierCase(t, root, "reused")
}
