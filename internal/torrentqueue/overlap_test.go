package torrentqueue

// BT-10h: one real live snapshot showing a seed serving and a download running at the
// same time, against their own durable rows.
//
// Setup is a separate, verified step: entry A reaches a completed capture through the
// ordinary queue path against a controlled seeder, and only then is its seed intent
// enabled. Entry B uses BT-10g's matched construction (the .torrent is built from the
// directory the peer serves and the identities are asserted equal). No store record is
// fabricated and no ad-hoc seeder bootstraps the prerequisite.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// attemptPhase returns the live phase of one entry's attempt of the given kind.
func attemptPhase(s LiveSnapshot, entryID, kind string) string {
	for _, a := range s.Attempts {
		if a.EntryID == entryID && a.Kind == kind {
			return a.Phase
		}
	}
	return ""
}

// waitSnapshot polls the real live endpoint until cond holds, returning the single
// snapshot that satisfied it.
func waitSnapshot(t *testing.T, root string, limit time.Duration, cond func(LiveSnapshot) bool) (LiveSnapshot, bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		snap, err := LiveSnapshotFor(ctx, root)
		cancel()
		if err == nil {
			if cond(snap) {
				return snap, true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return LiveSnapshot{}, false
}

func TestLiveOverlapDownloadAndSeed(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}

	// --- Entry A: completed capture through the queue, then seed intent -----------
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(96<<10, 167)}, 0)
	entryA := enqueue(t, store, seedA, "alpha/a.bin")
	startEntry(t, store, entryA.ID)
	if sum, rerr := NewRunner(store, realAdapter(), Options{Log: nil}).Run(context.Background()); rerr != nil || sum.Completed != 1 {
		t.Fatalf("setup: entry A did not complete: %+v %v", sum, rerr)
	}
	doneA := mustGet(t, store, entryA.ID)
	if doneA.Phase != PhaseCompleted || doneA.Capture == nil || doneA.Workspace == "" {
		t.Fatalf("setup: entry A is not seed-eligible: %+v", doneA)
	}
	if _, serr := store.EnableSeed(entryA.ID); serr != nil {
		t.Fatalf("setup: enable seed intent: %v", serr)
	}

	// --- Entry B: matched fixture, throttled peer, not started yet ----------------
	payload := patternedBytes(6<<20, 173)
	dir := t.TempDir()
	writeFixtureFile(t, dir, "fixture", "b.bin", payload)
	info := metainfo.Info{PieceLength: 16 << 10, Name: "fixture"}
	if berr := info.BuildFromFilePath(dir + "/fixture"); berr != nil {
		t.Fatalf("setup: build info: %v", berr)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	torrentPath := t.TempDir() + "/b.torrent"
	tf, cerr := os.Create(torrentPath)
	if cerr != nil {
		t.Fatalf("setup: create torrent: %v", cerr)
	}
	if werr := mi.Write(tf); werr != nil {
		t.Fatalf("setup: write torrent: %v", werr)
	}
	_ = tf.Close()
	peerB := startProbeSeeder(t, dir, &mi, 768<<10)
	loaded, lerr := metainfo.LoadFromFile(torrentPath)
	if lerr != nil {
		t.Fatalf("setup: load torrent: %v", lerr)
	}
	if loaded.HashInfoBytes() != mi.HashInfoBytes() {
		t.Fatalf("setup: torrent identity does not match the peer")
	}
	entryB, _, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "file", Path: torrentPath},
		RawSource: torrentPath,
		Peers:     []string{peerB},
		Discovery: "direct",
		Selection: []string{"fixture/b.bin"},
	})
	if aerr != nil {
		t.Fatalf("setup: add entry B: %v", aerr)
	}

	// Both intents are recorded before the run starts: the runner is a drain, so an
	// entry started after it has drained downloads would never be claimed in this pass.
	startEntry(t, store, entryB.ID)

	// --- Runner: one download slot, one seed slot --------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = NewRunner(store, realAdapter(), Options{
			MaxActive: 1, MaxSeeds: 1, PollInterval: 20 * time.Millisecond, Log: nil,
		}).Run(ctx)
	}()

	// stopRunner cancels the runner and waits for it once, with a bound: a runner that
	// never exits fails the test instead of hanging until the package timeout. The
	// deferred call is the safety net for assertion failures, and it never waits a
	// second time - stopTried is set before runDone is read.
	stopTried := false
	stopRunner := func(limit time.Duration) bool {
		stopTried = true
		cancel()
		select {
		case <-runDone:
			return true
		case <-time.After(limit):
			return false
		}
	}
	defer func() {
		if stopTried {
			return
		}
		if !stopRunner(60 * time.Second) {
			t.Errorf("the runner did not exit within 60s of cancellation")
		}
	}()

	// --- ONE snapshot with both attempts ----------------------------------------
	snap, ok := waitSnapshot(t, root, 90*time.Second, func(s LiveSnapshot) bool {
		return attemptPhase(s, entryA.ID, "seed") == "serving" &&
			attemptPhase(s, entryB.ID, "download") == "running" &&
			s.Seeds.Used == 1 && s.Seeds.Max == 1 &&
			s.Downloads.Used == 1 && s.Downloads.Max == 1
	})
	if !ok {
		t.Fatalf("no single snapshot showed both attempts; A=%+v B=%+v",
			mustGet(t, store, entryA.ID), mustGet(t, store, entryB.ID))
	}
	// The count is asserted here rather than in the polling predicate: a third attempt
	// must fail explicitly instead of timing the poll out.
	if got := len(snap.Attempts); got != 2 {
		for _, a := range snap.Attempts {
			t.Logf("  unexpected attempt entry=%s kind=%s phase=%s stopping=%v",
				a.EntryID, a.Kind, a.Phase, a.Stopping)
		}
		t.Fatalf("the snapshot carried %d attempts, want exactly 2", got)
	}
	t.Logf("OVERLAP snapshot pid=%d downloads=%d/%d seeds=%d/%d attempts=%d",
		snap.PID, snap.Downloads.Used, snap.Downloads.Max, snap.Seeds.Used, snap.Seeds.Max, len(snap.Attempts))
	for _, a := range snap.Attempts {
		up := int64(-1)
		if a.UploadedBytes != nil {
			up = *a.UploadedBytes
		}
		t.Logf("  OVERLAP attempt entry=%s kind=%s phase=%s stopping=%v uploaded=%d",
			a.EntryID, a.Kind, a.Phase, a.Stopping, up)
	}
	// The seed's uploaded counter is absent in this snapshot (logged as -1), not a
	// measured zero: nothing in this fixture leeches from A, so "serving" means the
	// session passed verification and holds its slot with its own listener, not that
	// bytes have been pushed. Uploaded bytes were measured separately in BT-08d.1
	// (aggregate budget) and BT-08d.3 (release gate).

	// --- Clean shutdown ----------------------------------------------------------
	if !stopRunner(60 * time.Second) {
		t.Fatalf("the runner did not shut down after cancellation")
	}
	if a := mustGet(t, store, entryA.ID); !a.SeedWanted() {
		t.Fatalf("interruption cleared seed intent: %+v", a)
	}
	if _, lerr := LiveSnapshotFor(context.Background(), root); lerr == nil {
		t.Fatal("the live endpoint still answers after the runner exited")
	}
}

func writeFixtureFile(t *testing.T, root, dir, name string, content []byte) {
	t.Helper()
	full := root + "/" + dir + "/" + name
	if err := os.MkdirAll(root+"/"+dir, 0o755); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("setup: write fixture: %v", err)
	}
}
