package torrentqueue

// Acceptance 3 and 4 against the real isolated backend and a loopback seeder:
// interrupt and resume, adoption of a capture published before the queue heard
// about it, a conflicting selection, and a genuinely busy workspace. DHT, PEX,
// trackers, web seeds, uTP and IPv6 are off; the peer is explicit.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/sk3y04/provenance-engine/internal/filelock"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// loopbackSeeder serves one fixture over loopback at a chosen rate.
type loopbackSeeder struct {
	client *at.Client
	mi     *metainfo.MetaInfo
	addr   string
}

func newLoopbackSeeder(t *testing.T, name string, files map[string][]byte, upload int) *loopbackSeeder {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10, Name: name}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}

	cfg := at.NewDefaultClientConfig()
	cfg.DataDir = root
	cfg.Seed = true
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisablePEX = true, true, true
	cfg.DisableUTP, cfg.DisableIPv6, cfg.DisableWebtorrent = true, true, true
	cfg.DisableWebseeds, cfg.NoDefaultPortForwarding = true, true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	if upload > 0 {
		// The burst must exceed one piece: the library reserves tokens per piece
		// request, and a burst below the 16 KiB piece size deadlocks the upload
		// (metadata still flows, payload never starts) instead of slowing it down.
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(upload), 64<<10)
	}
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("start seeder: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("seeder add: %v", err)
	}
	<-tor.GotInfo()
	tor.VerifyData()
	deadline := time.Now().Add(30 * time.Second)
	for !tor.Complete().Bool() {
		if time.Now().After(deadline) {
			t.Fatal("seeder never completed verification")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return &loopbackSeeder{client: client, mi: mi, addr: "127.0.0.1:" + itoa(client.LocalPort())}
}

func (s *loopbackSeeder) magnet() string {
	return "magnet:?xt=urn:btih:" + s.mi.HashInfoBytes().HexString()
}

func (s *loopbackSeeder) jobID() string { return "bt-" + s.mi.HashInfoBytes().HexString() }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// enqueueHelper adds a resolved magnet entry for a seeder fixture.
func enqueue(t *testing.T, store *Store, seed *loopbackSeeder, selection ...string) Entry {
	t.Helper()
	entry, _, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: seed.magnet()},
		RawSource: seed.magnet(),
		Peers:     []string{seed.addr},
		Discovery: "direct",
		Selection: selection,
		JobID:     seed.jobID(),
		InfoHash:  strings.TrimPrefix(seed.jobID(), "bt-"),
		Workspace: ".provenance/torrent/jobs/" + seed.jobID(),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return entry
}

func startEntry(t *testing.T, store *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.Update(id, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func realAdapter() Adapter {
	return Adapter{MetadataTimeout: 30 * time.Second, Timeout: 120 * time.Second}
}

func runManifestCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".provenance", "runs"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestQueueRealInterruptAndResume is acceptance 3's first half: a selective
// transfer interrupted mid-flight resumes in the same workspace and publishes
// exactly one run.
func TestQueueRealInterruptAndResume(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(400<<10, 7), "b.bin": patternedBytes(60<<10, 11)}
	seed := newLoopbackSeeder(t, "fixture", files, 48<<10)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, entry.ID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(ctx)
		done <- sum
	}()

	// Interrupt once real bytes have landed: a .part in the workspace is the
	// evidence that the transfer started, not a timer.
	ws, werr := filepath.Join(root, ".provenance", "torrent", "jobs", seed.jobID()), error(nil)
	_ = werr
	deadline := time.Now().Add(60 * time.Second)
	for {
		if findPart(t, ws) {
			break
		}
		select {
		case sum := <-done:
			entry, _ := store.Get(entry.ID)
			cancel()
			t.Fatalf("drain ended before any byte landed: %+v phase=%q error=%q", sum, entry.Phase, entry.LastError)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			entry, _ := store.Get(entry.ID)
			t.Fatalf("transfer never started writing: phase=%q error=%q tree=\n%s", entry.Phase, entry.LastError, dumpTree(ws))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	interrupted, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Phase != PhaseQueued {
		t.Fatalf("phase after interrupt = %q, want queued (shutdown is not a pause)", interrupted.Phase)
	}
	if interrupted.Desired != DesiredRun {
		t.Fatalf("desired after interrupt = %q", interrupted.Desired)
	}
	if interrupted.Capture != nil {
		t.Fatalf("interrupted entry recorded a capture: %+v", interrupted.Capture)
	}

	// Restart, same workspace, finish.
	sum, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("restart drain: %v", err)
	}
	final, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseCompleted || final.Capture == nil {
		t.Fatalf("final = phase %q capture %+v", final.Phase, final.Capture)
	}
	if final.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", final.Attempts)
	}
	if final.Workspace != ".provenance/torrent/jobs/"+seed.jobID() {
		t.Errorf("workspace = %q, want the canonical one", final.Workspace)
	}
	deployed := filepath.Join(root, filepath.FromSlash(final.Workspace))
	if got := runManifestCount(t, deployed); got != 1 {
		t.Errorf("%d run manifests, want exactly 1", got)
	}
	if sum.Completed != 1 {
		t.Errorf("restart summary = %+v", sum)
	}
	payload, rerr := os.ReadFile(filepath.Join(deployed, "fixture", "a.bin"))
	if rerr != nil {
		t.Fatalf("payload: %v", rerr)
	}
	if string(payload) != string(files["a.bin"]) {
		t.Error("resumed payload does not match the fixture")
	}
	// The unselected neighbour was never captured.
	if countCaptureItems(t, deployed) != 1 {
		t.Errorf("%d capture items, want 1 (only the selected file)", countCaptureItems(t, deployed))
	}
}

// findPart reports that bytes have started landing: the library keeps partial
// data either in a .part file or in the final name while the piece set is
// incomplete, so the workspace simply gaining a payload file is the signal.
func findPart(t *testing.T, ws string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(ws, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(ws, p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(filepath.ToSlash(rel), ".provenance/") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > 0 {
			found = true
		}
		return nil
	})
	return found
}

func dumpTree(root string) string {
	var b strings.Builder
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		info, _ := d.Info()
		size := int64(-1)
		if info != nil && !d.IsDir() {
			size = info.Size()
		}
		b.WriteString("  " + rel + " (" + itoa(int(size)) + ")\n")
		return nil
	})
	return b.String()
}

func countCaptureItems(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".provenance", "items"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// failingQueueUpdate runs the real backend and then loses the result, which is the
// crash-between-commit-and-queue-update shape. It is an injected failure, not
// process death: the workspace keeps its published capture either way.
type failingQueueUpdate struct{ Adapter }

func (f failingQueueUpdate) Run(ctx context.Context, req Request) (Outcome, error) {
	if _, err := f.Adapter.Run(ctx, req); err != nil {
		return Outcome{}, err
	}
	return Outcome{}, errors.New("queue update lost after the capture was published")
}

// TestQueueAdoptsCapturePublishedBeforeQueueUpdate is acceptance 3's second half.
func TestQueueAdoptsCapturePublishedBeforeQueueUpdate(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(80<<10, 13)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, entry.ID)

	sum, err := NewRunner(store, failingQueueUpdate{realAdapter()}, Options{PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Completed != 1 {
		t.Fatalf("summary = %+v, want the injected failure resolved by adoption", sum)
	}
	got, _ := store.Get(entry.ID)
	if got.Phase != PhaseCompleted {
		t.Fatalf("phase = %q, want completed: the capture was published, so the queue must adopt it", got.Phase)
	}
	if got.Capture == nil || got.Capture.Key == "" {
		t.Fatalf("capture = %+v, want the published capture adopted", got.Capture)
	}
	if !strings.Contains(got.Note, "adopted") {
		t.Errorf("note = %q, want an adoption explanation", got.Note)
	}
	ws := filepath.Join(root, filepath.FromSlash(got.Workspace))
	if n := runManifestCount(t, ws); n != 1 {
		t.Fatalf("%d run manifests after adoption, want 1", n)
	}
	adoptedKey := got.Capture.Key

	// A restart must not refetch or duplicate: the entry is complete, so the drain
	// claims nothing at all.
	after, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("restart drain: %v", err)
	}
	if len(after.Ran) != 0 {
		t.Errorf("restart claimed %v, want nothing: a published capture is done", after.Ran)
	}
	final, _ := store.Get(entry.ID)
	if final.Capture.Key != adoptedKey {
		t.Errorf("capture key changed across restart: %s -> %s", adoptedKey, final.Capture.Key)
	}
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests after restart, want 1", n)
	}
}

// TestQueueConflictingSelectionIsRejected: a second selection for the same torrent
// is refused by the job record rather than silently changing the published job.
func TestQueueConflictingSelectionIsRejected(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(60<<10, 17), "b.bin": patternedBytes(40<<10, 19)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	first := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, first.ID)
	if _, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	done, _ := store.Get(first.ID)
	if done.Phase != PhaseCompleted {
		t.Fatalf("first entry phase = %q", done.Phase)
	}
	firstKey := done.Capture.Key

	// A different selection on the same torrent is a distinct capture intent, and
	// the job record's immutable selection refuses it.
	second := enqueue(t, store, seed, "fixture/b.bin")
	if second.ID == first.ID {
		t.Fatal("a differing selection was deduplicated onto the first entry")
	}
	startEntry(t, store, second.ID)
	if _, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	conflicted, _ := store.Get(second.ID)
	if conflicted.Phase != PhaseFailed {
		t.Fatalf("conflicting entry phase = %q, want failed", conflicted.Phase)
	}
	if !strings.Contains(strings.ToLower(conflicted.LastError), "selection") {
		t.Errorf("error = %q, want a selection explanation", conflicted.LastError)
	}
	// The published capture and its job are untouched.
	again, _ := store.Get(first.ID)
	if again.Capture == nil || again.Capture.Key != firstKey {
		t.Errorf("the first entry's capture changed: %+v", again.Capture)
	}
	ws := filepath.Join(root, filepath.FromSlash(again.Workspace))
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests after a refused selection, want 1", n)
	}
	if countCaptureItems(t, ws) != 1 {
		t.Errorf("capture items changed after a refused selection")
	}
}

// TestQueueBusyWorkspaceBlocksThenRetries: a live workspace owner defers the entry
// with a backoff instead of failing it or hot-looping.
func TestQueueBusyWorkspaceBlocksThenRetries(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(50<<10, 23)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, entry.ID)

	ws, err := bittorrent.WorkspaceDir(root, entry.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".provenance", "torrent"), 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := filelock.TryAcquire(filepath.Join(ws, ".provenance", "torrent", "staging"))
	if err != nil {
		t.Fatalf("take workspace lock: %v", err)
	}

	if _, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond, Backoff: time.Minute}).Run(context.Background()); err != nil {
		t.Fatalf("busy drain: %v", err)
	}
	blocked, _ := store.Get(entry.ID)
	if blocked.Phase != PhaseBlocked {
		t.Fatalf("phase = %q (%s), want blocked on a busy workspace", blocked.Phase, blocked.LastError)
	}
	if blocked.NotBefore.IsZero() {
		t.Error("blocked entry has no backoff")
	}
	// The capture was not touched: a busy workspace is not a failed transfer.
	if blocked.Capture != nil {
		t.Errorf("blocked entry recorded a capture: %+v", blocked.Capture)
	}

	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(entry.ID, func(e *Entry) error {
		e.NotBefore = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatalf("drain after release: %v", err)
	}
	final, _ := store.Get(entry.ID)
	if final.Phase != PhaseCompleted || final.Capture == nil {
		t.Fatalf("after release = phase %q capture %+v", final.Phase, final.Capture)
	}
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests, want 1", n)
	}
}

// patternedBytes makes deterministic, incompressible-enough content.
func patternedBytes(n int, seed byte) []byte {
	out := make([]byte, n)
	x := uint32(seed) | 1
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 24)
	}
	return out
}

// countingAdapter wraps the real backend and records peak concurrency, so overlap
// is observed rather than inferred from timing.
type countingAdapter struct {
	inner   Adapter
	active  int32
	maxSeen int32
}

func (c *countingAdapter) Run(ctx context.Context, req Request) (Outcome, error) {
	cur := atomic.AddInt32(&c.active, 1)
	for {
		max := atomic.LoadInt32(&c.maxSeen)
		if cur <= max || atomic.CompareAndSwapInt32(&c.maxSeen, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&c.active, -1)
	return c.inner.Run(ctx, req)
}

func (c *countingAdapter) Adopt(root, jobID string, selection []string) (CaptureRef, bool) {
	return c.inner.Adopt(root, jobID, selection)
}

// TestQueueRealConcurrentSameName is acceptance 4's first half: two torrents that
// share a display name and a relative path but not their bytes transfer with
// overlapping attempts and stay entirely separate.
func TestQueueRealConcurrentSameName(t *testing.T) {
	filesA := map[string][]byte{"a.bin": patternedBytes(200<<10, 43)}
	filesB := map[string][]byte{"a.bin": patternedBytes(200<<10, 47)}
	seedA := newLoopbackSeeder(t, "fixture", filesA, 0)
	seedB := newLoopbackSeeder(t, "fixture", filesB, 0)
	if seedA.jobID() == seedB.jobID() {
		t.Fatal("the two fixtures share an infohash; the test proves nothing")
	}
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	a := enqueue(t, store, seedA, "fixture/a.bin")
	b := enqueue(t, store, seedB, "fixture/a.bin")
	if a.Workspace == b.Workspace {
		t.Fatalf("two identities share a workspace: %s", a.Workspace)
	}
	startEntry(t, store, a.ID, b.ID)

	counter := &countingAdapter{inner: realAdapter()}
	sum, err := NewRunner(store, counter, Options{MaxActive: 2, PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Completed != 2 || sum.Failed != 0 || sum.Blocked != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if max := atomic.LoadInt32(&counter.maxSeen); max != 2 {
		t.Errorf("peak concurrent attempts = %d, want 2", max)
	}

	for _, tc := range []struct {
		entry Entry
		want  []byte
	}{
		{a, filesA["a.bin"]},
		{b, filesB["a.bin"]},
	} {
		got, gerr := store.Get(tc.entry.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Phase != PhaseCompleted || got.Capture == nil {
			t.Fatalf("%s = %q capture %+v (%s)", tc.entry.ID, got.Phase, got.Capture, got.LastError)
		}
		ws := filepath.Join(root, filepath.FromSlash(got.Workspace))
		payload, rerr := os.ReadFile(filepath.Join(ws, "fixture", "a.bin"))
		if rerr != nil {
			t.Fatalf("%s payload: %v", tc.entry.ID, rerr)
		}
		if string(payload) != string(tc.want) {
			t.Errorf("%s payload does not match its fixture", tc.entry.ID)
		}
		db := filepath.Join(ws, ".provenance", "completion", ".torrent.db")
		if _, serr := os.Stat(db); serr != nil {
			t.Errorf("%s has no completion database at %s", tc.entry.ID, db)
		}
		if n := runManifestCount(t, ws); n != 1 {
			t.Errorf("%s: %d run manifests, want 1", tc.entry.ID, n)
		}
	}
	gotA, _ := store.Get(a.ID)
	gotB, _ := store.Get(b.ID)
	if gotA.Capture.Key == gotB.Capture.Key {
		t.Error("two distinct payloads produced one capture key")
	}
	if gotA.Workspace == gotB.Workspace {
		t.Error("both jobs recorded the same workspace")
	}
}

// TestQueueRealSameWorkspaceRace is acceptance 4's second half: two entries land on
// one canonical workspace. The queue id is not a workspace lock, so the loser backs
// off; a later drain adopts the accepted capture instead of writing a second one or
// silently taking over a different selection.
func TestQueueRealSameWorkspaceRace(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(150<<10, 53), "b.bin": patternedBytes(70<<10, 59)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}

	// Same torrent, two source forms: a magnet and a local .torrent. Both resolve to
	// bt-<v1>, so both want the same workspace.
	torrentPath := filepath.Join(t.TempDir(), "fixture.torrent")
	f, cerr := os.Create(torrentPath)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if werr := seed.mi.Write(f); werr != nil {
		t.Fatal(werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	first, _, err := store.Add(AddRequest{
		Source: Source{Kind: "magnet", Magnet: seed.magnet()}, RawSource: seed.magnet(),
		Peers: []string{seed.addr}, Discovery: "direct",
		Selection: []string{"fixture/a.bin"}, JobID: seed.jobID(),
		InfoHash:  strings.TrimPrefix(seed.jobID(), "bt-"),
		Workspace: ".provenance/torrent/jobs/" + seed.jobID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, duplicate, err := store.Add(AddRequest{
		Source: Source{Kind: "file", Path: torrentPath}, RawSource: torrentPath,
		Peers: []string{seed.addr}, Discovery: "direct",
		Selection: []string{"fixture/a.bin"}, JobID: seed.jobID(),
		InfoHash:  strings.TrimPrefix(seed.jobID(), "bt-"),
		Workspace: ".provenance/torrent/jobs/" + seed.jobID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same job and same selection is the same capture intent: the store converges
	// them instead of queueing a second writer.
	if !duplicate || second.ID != first.ID {
		t.Fatalf("duplicate add created %s instead of returning %s", second.ID, first.ID)
	}
	startEntry(t, store, first.ID)

	// A conflicting selection for the same torrent is a different intent and does
	// create a second entry.
	conflict := seed.magnet()
	clash, dup2, err := store.Add(AddRequest{
		Source: Source{Kind: "magnet", Magnet: conflict}, RawSource: conflict,
		Peers: []string{seed.addr}, Discovery: "direct",
		Selection: []string{"fixture/b.bin"}, JobID: seed.jobID(),
		InfoHash:  strings.TrimPrefix(seed.jobID(), "bt-"),
		Workspace: ".provenance/torrent/jobs/" + seed.jobID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup2 || clash.ID == first.ID {
		t.Fatalf("a differing selection was deduplicated onto %s", first.ID)
	}
	startEntry(t, store, clash.ID)

	sum, err := NewRunner(store, realAdapter(), Options{MaxActive: 2, PollInterval: 10 * time.Millisecond, Backoff: time.Minute}).Run(context.Background())
	if err != nil {
		t.Fatalf("first drain: %v", err)
	}
	winner, _ := store.Get(first.ID)
	if winner.Phase != PhaseCompleted || winner.Capture == nil {
		t.Fatalf("the accepted intent did not complete: %q %+v (%s)", winner.Phase, winner.Capture, winner.LastError)
	}
	loser, _ := store.Get(clash.ID)
	switch loser.Phase {
	case PhaseBlocked, PhaseFailed:
	default:
		t.Fatalf("the conflicting entry ended %q, want blocked (busy) or failed (selection)", loser.Phase)
	}

	// Second drain: whatever the loser's path, it must not produce a second capture
	// for this workspace, and a blocked loser is adopted rather than refetched.
	if _, uerr := store.Update(clash.ID, func(e *Entry) error {
		e.NotBefore = time.Time{}
		e.Phase = PhaseQueued
		e.Desired = DesiredRun
		e.Attempt = ""
		return nil
	}); uerr != nil {
		t.Fatal(uerr)
	}
	sum, err = NewRunner(store, realAdapter(), Options{MaxActive: 1, PollInterval: 10 * time.Millisecond, Backoff: time.Minute}).Run(context.Background())
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	final, _ := store.Get(clash.ID)
	if final.Phase == PhaseCompleted {
		// Completing is only acceptable if it adopted the same capture.
		if final.Capture == nil || final.Capture.Key != winner.Capture.Key {
			t.Fatalf("a conflicting selection completed with a different capture: %+v", final.Capture)
		}
	}
	ws := filepath.Join(root, filepath.FromSlash(winner.Workspace))
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests, want exactly 1 for this workspace", n)
	}
	if n := countCaptureItems(t, ws); n != 1 {
		t.Errorf("%d capture items, want 1", n)
	}
	if len(sum.Ran) > 1 {
		t.Errorf("second drain claimed %v", sum.Ran)
	}
	// The conflicting selection never overwrote the job's recorded selection.
	job, jerr := os.ReadFile(filepath.Join(ws, filepath.FromSlash(".provenance/torrent/"+seed.jobID()+".job.json")))
	if jerr != nil {
		t.Fatal(jerr)
	}
	if !strings.Contains(string(job), "fixture/a.bin") || strings.Contains(string(job), "fixture/b.bin") {
		t.Errorf("job record selection changed: %s", job)
	}
}
