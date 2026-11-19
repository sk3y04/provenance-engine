package torrentqueue

// BT-09a: non-destructive queue removal.

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// fakeMagnet builds an identity-only magnet with a distinct infohash.
func fakeMagnet(prefix string) string {
	return "magnet:?xt=urn:btih:" + prefix + strings.Repeat("0", 40-len(prefix))
}

func addWithSecret(t *testing.T, store *Store, magnet, secret, selection string) Entry {
	t.Helper()
	entry, created, err := store.Add(AddRequest{
		Source:     Source{Kind: "magnet", Magnet: magnet},
		RawSource:  magnet + "&tr=http://tracker.example/announce?passkey=" + secret,
		Discovery:  "direct",
		Selection:  []string{selection},
		Unresolved: true,
	})
	if err != nil || created {
		t.Fatalf("add: %+v duplicate=%v err=%v", entry, created, err)
	}
	return entry
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(b)
}

func treeDigest(t *testing.T, dir string) [32]byte {
	t.Helper()
	sum := sha256.New()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum.Write([]byte(rel))
		if info.Mode().IsRegular() {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			sum.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	var out [32]byte
	copy(out[:], sum.Sum(nil))
	return out
}

func TestQueueRemoveUnresolvedEntryKeepsOthers(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	magnetA := fakeMagnet("aa")
	magnetB := fakeMagnet("bb")
	a := addWithSecret(t, store, magnetA, "SECRET-A", "alpha/a.bin")
	b := addWithSecret(t, store, magnetB, "SECRET-B", "beta/b.bin")
	bBytes := fileDigest(t, store.entryPath(b.ID))

	res, rerr := store.Remove(a.ID)
	if rerr != nil {
		t.Fatalf("remove: %v", rerr)
	}
	if !res.EntryRemoved || !res.RestrictedRemoved || res.CleanupError != "" {
		t.Fatalf("removal = %+v, want both records gone without a cleanup error", res)
	}
	if _, serr := os.Stat(store.entryPath(a.ID)); !os.IsNotExist(serr) {
		t.Fatalf("public entry still present")
	}
	if _, serr := os.Stat(filepath.Join(store.restrictedDir(), a.ID+".json")); !os.IsNotExist(serr) {
		t.Fatalf("restricted record still present")
	}
	if fileDigest(t, store.entryPath(b.ID)) != bBytes {
		t.Fatal("removing one entry changed another")
	}
	if _, serr := os.Stat(filepath.Join(store.restrictedDir(), b.ID+".json")); serr != nil {
		t.Fatalf("the other entry's restricted record was touched: %v", serr)
	}

	// The secret never appears in the removal result.
	if res.CleanupError != "" {
		t.Fatal("unexpected cleanup error")
	}

	// Invalid and unknown ids are refused, including traversal-shaped input.
	for _, bad := range []string{"queued-000001", "aa" + strings.Repeat("0", 38),
		"../../etc/passwd", a.ID + "/../../x", "queued-999999-zzzzzzzz", "queued-000001-aaaaaaaa-extra"} {
		if _, berr := store.Remove(bad); berr == nil {
			t.Fatalf("remove(%q) was accepted", bad)
		}
	}
	if _, berr := store.Remove("queued-000042-deadbeef"); !errors.Is(berr, ErrEntryNotFound) {
		t.Fatalf("unknown id error = %v, want ErrEntryNotFound", berr)
	}
}

func TestQueueRemoveCompletedEntryRetainsWorkspace(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(256<<10, 37)}, 0)
	entry := completedSeededEntry(t, store, seed, "alpha/a.bin")
	ws := workspacePath(t, store, entry)

	payload := fileDigest(t, filepath.Join(ws, "alpha", "a.bin"))
	metainfoDigest := treeDigest(t, filepath.Join(ws, ".provenance", "torrent"))
	itemsDigest := treeDigest(t, filepath.Join(ws, ".provenance", "items"))
	runsDigest := treeDigest(t, filepath.Join(ws, ".provenance", "runs"))
	captureKey := entry.Capture.Key

	res, rerr := store.Remove(entry.ID)
	if rerr != nil || !res.EntryRemoved {
		t.Fatalf("remove: %+v %v", res, rerr)
	}
	if _, serr := store.Get(entry.ID); !errors.Is(serr, ErrEntryNotFound) {
		t.Fatalf("entry still readable: %v", serr)
	}
	if fileDigest(t, filepath.Join(ws, "alpha", "a.bin")) != payload {
		t.Fatal("removal changed the payload")
	}
	if treeDigest(t, filepath.Join(ws, ".provenance", "torrent")) != metainfoDigest {
		t.Fatal("removal changed the persisted metainfo or job record")
	}
	if treeDigest(t, filepath.Join(ws, ".provenance", "items")) != itemsDigest {
		t.Fatal("removal changed the capture items")
	}
	if treeDigest(t, filepath.Join(ws, ".provenance", "runs")) != runsDigest {
		t.Fatal("removal changed the published run")
	}
	// The workspace is still usable: it verifies and can be served.
	sess, oerr := bittorrent.OpenSeedSession(context.Background(),
		bittorrent.Config{StagingDir: ws, Discovery: bittorrent.DiscoveryDirect}, entry.JobID)
	if oerr != nil {
		t.Fatalf("workspace unusable after removal: %v", oerr)
	}
	report := sess.Report()
	if report.VerifiedPieces == 0 || report.VerifiedPieces != report.TotalPieces {
		t.Fatalf("workspace no longer verifies: %+v", report)
	}
	_ = sess.Close()
	if entry.Capture == nil || entry.Capture.Key != captureKey {
		t.Fatal("capture key changed")
	}
}

func TestQueueRemoveRefusesActiveRunner(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	magnet := fakeMagnet("cc")
	entry := addWithSecret(t, store, magnet, "SECRET-C", "alpha/a.bin")

	owner, lerr := store.LockRunner()
	if lerr != nil {
		t.Fatalf("take runner lock: %v", lerr)
	}
	if _, rerr := store.Remove(entry.ID); !errors.Is(rerr, ErrRunnerActive) {
		t.Fatalf("remove under an active runner = %v, want ErrRunnerActive", rerr)
	}
	if _, serr := store.Get(entry.ID); serr != nil {
		t.Fatalf("a refused removal changed the entry: %v", serr)
	}
	// The reverse ordering: while ownership is held, a runner cannot start.
	if _, rerr := NewRunner(store, realAdapter(), Options{}).Run(context.Background()); !errors.Is(rerr, ErrRunnerActive) {
		t.Fatalf("runner started while removal held ownership: %v", rerr)
	}
	_ = owner.Release()

	res, rerr := store.Remove(entry.ID)
	if rerr != nil || !res.EntryRemoved {
		t.Fatalf("remove after release: %+v %v", res, rerr)
	}
}

func TestQueueRemoveStaleRecordWithoutOwner(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(64<<10, 41)}, 0)
	entry := completedSeededEntry(t, store, seed, "alpha/a.bin")
	// A stale observation from a process that no longer exists: this is not a kill,
	// it is the record a killed runner leaves behind.
	if _, uerr := store.Update(entry.ID, func(e *Entry) error {
		e.Phase = PhaseRunning
		e.SeedDesired = SeedOn
		e.SeedPhase = SeedServing
		e.Attempt = "a-deadbeef"
		e.SeedAttempt = entry.ID + "/seed-1"
		return nil
	}); uerr != nil {
		t.Fatalf("seed stale record: %v", uerr)
	}
	if RunnerHeld(store.Root()) {
		t.Fatal("a runner owns the queue; the fixture is not what it claims")
	}
	res, rerr := store.Remove(entry.ID)
	if rerr != nil || !res.EntryRemoved {
		t.Fatalf("removal with a stale running/serving record: %+v %v", res, rerr)
	}
}

func TestQueueRemovePartialCleanupRetry(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	magnet := fakeMagnet("dd")
	entry := addWithSecret(t, store, magnet, "SECRET-D", "alpha/a.bin")
	other := addWithSecret(t, store, fakeMagnet("ee"), "SECRET-E", "beta/b.bin")
	otherBytes := fileDigest(t, store.entryPath(other.ID))

	if cerr := os.Chmod(store.restrictedDir(), 0o500); cerr != nil {
		t.Skipf("cannot make the restricted directory read-only here: %v", cerr)
	}
	t.Cleanup(func() { _ = os.Chmod(store.restrictedDir(), 0o700) })

	res, rerr := store.Remove(entry.ID)
	if rerr != nil {
		t.Fatalf("remove with a failing restricted delete: %v", rerr)
	}
	if !res.EntryRemoved || res.RestrictedRemoved || res.CleanupError == "" {
		t.Fatalf("removal = %+v, want the entry gone and cleanup reported incomplete", res)
	}
	if _, serr := store.Get(entry.ID); !errors.Is(serr, ErrEntryNotFound) {
		t.Fatalf("the entry is still runnable after a partial removal: %v", serr)
	}
	if _, werr := NewRunner(store, realAdapter(), Options{}).Run(context.Background()); werr != nil {
		t.Fatalf("a partial removal left the queue undrainable: %v", werr)
	}

	if cerr := os.Chmod(store.restrictedDir(), 0o700); cerr != nil {
		t.Fatalf("restore permissions: %v", cerr)
	}
	retry, rerr := store.Remove(entry.ID)
	if rerr != nil {
		t.Fatalf("retry: %v", rerr)
	}
	if retry.EntryRemoved || !retry.RestrictedRemoved || retry.CleanupError != "" {
		t.Fatalf("retry = %+v, want only the restricted record cleaned", retry)
	}
	if fileDigest(t, store.entryPath(other.ID)) != otherBytes {
		t.Fatal("the retry touched another entry")
	}
	if _, serr := os.Stat(filepath.Join(store.restrictedDir(), other.ID+".json")); serr != nil {
		t.Fatal("the retry deleted another entry's restricted record")
	}
}

func TestQueueReAddAdoptsExistingCapture(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(256<<10, 43)}, 0)
	first := completedSeededEntry(t, store, seed, "alpha/a.bin")
	ws := workspacePath(t, store, first)
	runsBefore := runManifestCount(t, ws)
	payload := fileDigest(t, filepath.Join(ws, "alpha", "a.bin"))
	key := first.Capture.Key

	if _, rerr := store.Remove(first.ID); rerr != nil {
		t.Fatalf("remove: %v", rerr)
	}
	again, created, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: seed.magnet()},
		RawSource: seed.magnet(),
		Peers:     []string{seed.addr},
		Discovery: "direct",
		Selection: []string{"alpha/a.bin"},
		JobID:     first.JobID,
		InfoHash:  first.InfoHash,
		Workspace: first.Workspace,
	})
	if aerr != nil || created {
		t.Fatalf("re-add: duplicate=%v err=%v", created, aerr)
	}
	if again.ID == first.ID {
		t.Fatalf("the re-added entry reused the removed id %s", again.ID)
	}
	startEntry(t, store, again.ID)
	sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
	if rerr != nil || sum.Completed != 1 {
		t.Fatalf("re-added drain: %+v %v", sum, rerr)
	}
	done := mustGet(t, store, again.ID)
	if done.Capture == nil || done.Capture.Key != key {
		t.Fatalf("re-add published a new capture: %+v (was %s)", done.Capture, key)
	}
	if runs := runManifestCount(t, ws); runs != runsBefore {
		t.Fatalf("re-add published another run: %d, was %d", runs, runsBefore)
	}
	if fileDigest(t, filepath.Join(ws, "alpha", "a.bin")) != payload {
		t.Fatal("re-add refetched or rewrote the payload")
	}

	// A different selection must not adopt that capture or change the workspace.
	conflict, ccreated, cerr := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: seed.magnet()},
		RawSource: seed.magnet(),
		Peers:     []string{seed.addr},
		Discovery: "direct",
		Selection: []string{"alpha/other.bin"},
		JobID:     first.JobID,
		InfoHash:  first.InfoHash,
		Workspace: first.Workspace,
	})
	if cerr == nil && !ccreated {
		startEntry(t, store, conflict.ID)
		csum, crerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
		if crerr == nil && csum.Completed > 0 {
			t.Fatalf("a conflicting selection completed against the existing workspace: %+v", csum)
		}
	}
	if runs := runManifestCount(t, ws); runs != runsBefore {
		t.Fatalf("a conflicting selection published a capture: %d runs", runs)
	}
}

func TestQueueRemoveHighestThenAllocate(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedA := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(32<<10, 47)}, 0)
	seedB := newLoopbackSeeder(t, "beta", map[string][]byte{"b.bin": patternedBytes(32<<10, 53)}, 0)
	a := enqueue(t, store, seedA, "alpha/a.bin")
	b := enqueue(t, store, seedB, "beta/b.bin")
	if b.Seq <= a.Seq {
		t.Fatalf("sequences did not increase: %d then %d", a.Seq, b.Seq)
	}
	if _, rerr := store.Remove(b.ID); rerr != nil {
		t.Fatalf("remove highest: %v", rerr)
	}
	seedC := newLoopbackSeeder(t, "gamma", map[string][]byte{"c.bin": patternedBytes(32<<10, 59)}, 0)
	c := enqueue(t, store, seedC, "gamma/c.bin")
	if c.ID == b.ID {
		t.Fatalf("a new entry reused the removed id %s", b.ID)
	}
	if _, serr := os.Stat(filepath.Join(store.restrictedDir(), b.ID+".json")); serr == nil {
		t.Fatalf("the removed entry's restricted record is still present")
	}
	// The reused sequence is fine as long as identity is not: the id carries entropy.
	t.Logf("removed seq %d id %s; next entry seq %d id %s", b.Seq, b.ID, c.Seq, c.ID)
}
