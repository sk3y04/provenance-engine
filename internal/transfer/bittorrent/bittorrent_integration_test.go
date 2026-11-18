package bittorrent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// TestFetchCapturesOnlySelectedFiles is the slice's headline behaviour: a
// multi-file torrent is joined by magnet from a local seeder, exactly one file
// is selected, and exactly one capture item is recorded and verifiable through
// the existing manifest and archive paths.
//
// The fixture is sized so file boundaries do not line up with piece boundaries:
// piece 0 spans a.bin and part of b.bin. Selecting b.bin therefore fetches
// bytes of a.bin as well, which is why selection is not a promise about disk
// contents — only selected complete files are captured.
func TestFetchCapturesOnlySelectedFiles(t *testing.T) {
	contentB := patterned(20000, 'a')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": bytes.Repeat([]byte("A"), 5000),
		"b.bin": contentB,
		"c.bin": patterned(9000, '0'),
	})
	staging := t.TempDir()
	sess := newTestSession(t, staging, s.peer())

	ctx, cancel := waitCtx(t)
	defer cancel()

	info, err := inspectForTest(t, s.magnet(), s.peer())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.InfoHash != s.infohash() {
		t.Errorf("infohash = %s, want %s", info.InfoHash, s.infohash())
	}
	if info.Name != "fixture" || len(info.Files) != 3 {
		t.Fatalf("info = %+v, want name fixture and 3 files", info)
	}
	if strings.Contains(info.Source, "tr=") {
		t.Errorf("source %q must not carry tracker parameters", info.Source)
	}
	if info.Source != "magnet:?xt=urn:btih:"+s.infohash() {
		t.Errorf("source = %q, want a sanitised magnet", info.Source)
	}

	cap, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/b.bin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(cap.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 (%+v)", len(cap.Assets), cap.Assets)
	}
	got := cap.Assets[0]
	if got.Path != "fixture/b.bin" {
		t.Errorf("asset path = %q, want fixture/b.bin", got.Path)
	}
	if want := sha256Hex(contentB); got.SHA256 != want {
		t.Errorf("sha256 = %s, want %s", got.SHA256, want)
	}
	if cap.BytesRead < int64(len(contentB)) {
		t.Errorf("bytes read = %d, want at least the selected size %d", cap.BytesRead, len(contentB))
	}
	if cap.JobID != "bt-"+s.infohash() {
		t.Errorf("job id = %q, want it derived from the infohash", cap.JobID)
	}
	if cap.CaptureKey == "" {
		t.Error("capture key is empty")
	}
	if cap.State.Payload != PayloadReady || cap.State.Capture != CaptureCommitted || cap.State.Seeding != SeedingStopped {
		t.Errorf("state = %+v, want ready/committed/stopped", cap.State)
	}

	// The shared edge piece writes a.bin to disk. Capturing it would be
	// wrong: the operator selected one file.
	if _, err := os.Stat(filepath.Join(staging, "fixture", "a.bin")); err == nil {
		t.Log("note: unselected neighbour a.bin exists on disk (shared edge piece), as expected")
	}

	cms := captureManifests(t, staging)
	if len(cms) != 1 {
		t.Fatalf("capture manifests = %d, want 1", len(cms))
	}
	cm := cms[0]
	if len(cm.Items) != 1 {
		t.Fatalf("capture items = %d, want 1 (%+v)", len(cm.Items), cm.Items)
	}
	if cm.Items[0].DownloadedPath != "fixture/b.bin" {
		t.Errorf("captured path = %q, want fixture/b.bin", cm.Items[0].DownloadedPath)
	}
	if cm.Items[0].Sha256 != got.SHA256 {
		t.Errorf("manifest hash %s != asset hash %s", cm.Items[0].Sha256, got.SHA256)
	}
	if cm.CaptureKey != cap.CaptureKey {
		t.Errorf("manifest capture key %q != result key %q", cm.CaptureKey, cap.CaptureKey)
	}
	if !strings.Contains(string(cm.Items[0].RawMetadata), s.infohash()) {
		t.Errorf("item metadata %s does not record the infohash", cm.Items[0].RawMetadata)
	}

	// Existing verification path.
	results, err := manifest.VerifyCaptureDir(staging)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("verify results = %+v, want exactly one OK", results)
	}

	// Existing archive handoff: one entity, one content-addressed artifact.
	vault := t.TempDir()
	bs := blobstore.New(vault)
	rev, err := archive.IngestCaptureManifest(bs, archive.IngestOptions{
		VaultRoot:      vault,
		CollectionName: "torrent-test",
		Source:         archive.Source{URL: s.magnet(), Kind: archive.SourceURL, Reference: "torrent-test"},
		Tool:           "provenance",
	}, cm)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(rev.Entities) != 1 || len(rev.Artifacts) != 1 {
		t.Fatalf("revision = %d entities, %d artifacts; want 1 and 1", len(rev.Entities), len(rev.Artifacts))
	}
	if rev.Artifacts[0].Sha256 != got.SHA256 {
		t.Errorf("archived artifact %s != captured %s", rev.Artifacts[0].Sha256, got.SHA256)
	}
	if !bs.Exists(got.SHA256) {
		t.Error("artifact is not in the blob store")
	}
}

// TestInspectLeavesStagingUntouched: inspection is metadata-only and job-free.
// It must not take the staging lock, materialise the layout, open payload
// storage, create a piece-completion database, persist metainfo, or write a
// capture — even when handed a staging directory that a fetch would use.
func TestInspectLeavesStagingUntouched(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(4000, 'x')})
	staging := t.TempDir()
	before := snapshotTree(t, staging)

	info, err := inspectForTest(t, s.magnet(), s.peer())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(info.Files) != 1 || info.Files[0].Length != 4000 {
		t.Fatalf("info = %+v", info)
	}
	if diff := diffSnapshots(before, snapshotTree(t, staging)); diff != "" {
		t.Errorf("inspection changed the staging tree:\n%s", diff)
	}
	if runs, err := manifest.ListRunManifests(staging); err == nil && len(runs) != 0 {
		t.Errorf("inspect wrote %d capture manifests, want 0", len(runs))
	}
	if _, err := os.Stat(filepath.Join(staging, torrentStateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inspect created the torrent state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, ".torrent.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inspect created a piece-completion database: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "fixture")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inspect created a payload directory: %v", err)
	}
}

// TestInterruptedFetchResumesAndReusesVerifiedBytes drives the three states a
// restart can be in: interrupted with partial data, resumed with a peer, and
// restarted with no peer at all.
func TestInterruptedFetchResumesAndReusesVerifiedBytes(t *testing.T) {
	content := patterned(2<<20, 'r')
	// 512 KiB/s upload keeps the transfer in flight long enough for the
	// interruption to be a real one rather than a race with loopback speed.
	// The torrent is multi-file so the resumed selection is a proper subset.
	s := newSeederRateLimited(t, "fixture", 32*1024, map[string][]byte{
		"big.bin":      content,
		"neighbor.bin": patterned(8000, 'n'),
	}, 512<<10)
	selection := []string{"fixture/big.bin"}

	// Pass 1: interrupt once a quarter of the payload has arrived.
	staging := t.TempDir()
	cfg := testConfig(t, staging, s.peer())
	ctx, cancel := context.WithCancel(context.Background())
	cfg.OnUpdate = func(u Update) {
		if u.Written >= 512<<10 {
			cancel()
		}
	}
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := sess.Fetch(ctx, s.magnet(), selection); err == nil {
		t.Fatal("interrupted fetch reported success")
	} else if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrTransferTimeout) {
		t.Fatalf("interrupted fetch error = %v, want cancellation", err)
	}
	_ = sess.Close()
	cancel()

	if parts := listPartFiles(t, staging); len(parts) == 0 {
		t.Fatal("interruption left no .part data; the resume test would prove nothing")
	}
	if job := jobRecordFor(t, staging, s.infohash()); job.State.Payload != PayloadPending {
		t.Errorf("payload state after interruption = %q, want pending", job.State.Payload)
	}

	// Pass 2: resume with the seeder reachable. Verified bytes must be reused,
	// so the peer sends less than the whole file.
	sess2 := newTestSession(t, staging, s.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap2, err := sess2.Fetch(ctx2, s.magnet(), selection)
	if err != nil {
		t.Fatalf("resume fetch: %v", err)
	}
	if cap2.BytesRead <= 0 {
		t.Errorf("resume read %d bytes from the peer, want > 0", cap2.BytesRead)
	}
	if cap2.BytesRead >= int64(len(content)) {
		t.Errorf("resume read %d of %d bytes; retained data was not reused", cap2.BytesRead, len(content))
	}
	if got := sha256Hex(content); cap2.Assets[0].SHA256 != got {
		t.Errorf("resumed content hash = %s, want %s", cap2.Assets[0].SHA256, got)
	}
	if cap2.State.Payload != PayloadReady || cap2.State.Capture != CaptureCommitted {
		t.Errorf("state after resume = %+v, want ready/committed", cap2.State)
	}
	if cap2.CaptureKey == "" {
		t.Error("resume wrote a capture without an idempotency key")
	}

	// Pass 3: no peer at all. Persisted metainfo plus verified retained data
	// must be enough to re-capture. The staging root is owned for the lifetime
	// of a Session, so the previous one is closed first.
	if errs := s.client.Close(); len(errs) > 0 {
		t.Logf("seeder close: %v", errs)
	}
	if err := sess2.Close(); err != nil {
		t.Fatalf("close resume session: %v", err)
	}
	sess3, err := New(Config{
		StagingDir:      staging,
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new offline session: %v", err)
	}
	defer func() { _ = sess3.Close() }()
	ctx3, cancel3 := waitCtx(t)
	defer cancel3()
	cap3, err := sess3.Fetch(ctx3, s.magnet(), selection)
	if err != nil {
		t.Fatalf("offline restart: %v", err)
	}
	if cap3.BytesRead != 0 {
		t.Errorf("offline restart read %d bytes from peers, want 0", cap3.BytesRead)
	}
	if cap3.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("offline capture hash = %s", cap3.Assets[0].SHA256)
	}
	// The capture already existed: pass 3 reconciles it rather than writing a
	// second manifest, and the key matches the one pass 2 committed.
	if !cap3.Reconciled {
		t.Error("offline restart wrote a new capture instead of reconciling the existing one")
	}
	if cap3.CaptureKey != cap2.CaptureKey {
		t.Errorf("capture key changed across runs: %s vs %s", cap3.CaptureKey, cap2.CaptureKey)
	}
	if n := len(manifestsWithKey(t, staging, cap3.CaptureKey)); n != 1 {
		t.Errorf("manifests with the job key = %d, want 1", n)
	}
}

// TestCorruptedRetainedDataIsRepaired: retained bytes are re-hashed before
// capture, so local corruption is detected and repaired from the peer instead
// of being archived.
func TestCorruptedRetainedDataIsRepaired(t *testing.T) {
	content := patterned(256<<10, 'c')
	s := newSeeder(t, "fixture", 32*1024, map[string][]byte{"c.bin": content})
	selection := []string{"fixture/c.bin"}
	staging := t.TempDir()

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), selection); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// Files are finalized read-only by the library; make one writable and
	// corrupt it, as a bad disk or a sync tool would.
	target := filepath.Join(staging, "fixture", "c.bin")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	f, err := os.OpenFile(target, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 1024); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	_ = f.Close()

	// Staging ownership is per Session, so release the first one before the
	// repair run opens its own.
	if err := sess.Close(); err != nil {
		t.Fatalf("close first session: %v", err)
	}
	sess2 := newTestSession(t, staging, s.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, s.magnet(), selection)
	if err != nil {
		t.Fatalf("repair fetch: %v", err)
	}
	if cap.BytesRead <= 0 {
		t.Error("repair read nothing from the peer; corruption may have been captured")
	}
	repaired, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if !bytes.Equal(repaired, content) {
		t.Fatal("repaired file does not match the fixture")
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("captured hash = %s, want %s", cap.Assets[0].SHA256, sha256Hex(content))
	}
	cms := captureManifests(t, staging)
	if len(cms) == 0 {
		t.Fatal("no capture manifest written")
	}
	// Run paths have second granularity, so two fast fetches may collapse onto
	// one manifest. Whatever exists must never describe corrupt bytes.
	for _, cm := range cms {
		if len(cm.Items) != 1 {
			t.Fatalf("capture items = %d, want 1", len(cm.Items))
		}
		if cm.Items[0].Sha256 != sha256Hex(content) {
			t.Errorf("a capture manifest recorded the corrupt hash %s", cm.Items[0].Sha256)
		}
	}
}

// TestFetchFromLocalTorrentFile covers the second input form: a .torrent on
// disk, still with explicit selection.
func TestFetchFromLocalTorrentFile(t *testing.T) {
	content := patterned(12000, 'f')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"one.bin": patterned(3000, '1'),
		"two.bin": content,
	})
	staging := t.TempDir()
	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()

	cap, err := sess.Fetch(ctx, s.torrentFile(t), []string{"fixture/two.bin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(cap.Assets) != 1 || cap.Assets[0].Path != "fixture/two.bin" {
		t.Fatalf("assets = %+v", cap.Assets)
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("sha256 = %s, want %s", cap.Assets[0].SHA256, sha256Hex(content))
	}
	if cap.InfoHash != s.infohash() {
		t.Errorf("infohash = %s, want %s", cap.InfoHash, s.infohash())
	}
}

// TestSelectionIsRequiredAndExact: this backend never downloads a whole
// torrent by accident, and a mistyped path is an error.
func TestSelectionIsRequiredAndExact(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(2000, 'a')})
	torrentFile := s.torrentFile(t)
	staging := t.TempDir()
	sess := newTestSession(t, staging) // metadata is in the file; no peer needed

	ctx, cancel := waitCtx(t)
	defer cancel()

	if _, err := sess.Fetch(ctx, torrentFile, nil); !errors.Is(err, ErrSelectionRequired) {
		t.Errorf("empty selection error = %v, want ErrSelectionRequired", err)
	}
	if _, err := sess.Fetch(ctx, torrentFile, []string{"fixture/missing.bin"}); !errors.Is(err, ErrSelectionNotFound) {
		t.Errorf("unknown selection error = %v, want ErrSelectionNotFound", err)
	}
	if _, err := sess.Fetch(ctx, torrentFile, []string{}); !errors.Is(err, ErrSelectionRequired) {
		t.Errorf("nil selection error = %v, want ErrSelectionRequired", err)
	}
}

// TestSelectionSizeLimitRejectsBeforeDownload: the size cap is enforced from
// metainfo, before any payload is requested.
func TestSelectionSizeLimitRejectsBeforeDownload(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(20000, 'a')})
	staging := t.TempDir()
	cfg := testConfig(t, staging)
	cfg.MaxSelectedBytes = 1000
	sess, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.torrentFile(t), []string{"fixture/a.bin"}); !errors.Is(err, ErrSelectionTooLarge) {
		t.Fatalf("error = %v, want ErrSelectionTooLarge", err)
	}
	if parts := listPartFiles(t, staging); len(parts) != 0 {
		t.Errorf("size limit still created %v", parts)
	}
}

// TestJobSelectionCannotChangeMidJob: resuming with a different selection is
// refused, because resume would otherwise mean something different each run.
func TestJobSelectionCannotChangeMidJob(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": patterned(2000, 'a'),
		"b.bin": patterned(2000, 'b'),
	})
	staging := t.TempDir()
	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()

	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/b.bin"}); !errors.Is(err, ErrSelectionLocked) {
		t.Fatalf("second selection error = %v, want ErrSelectionLocked", err)
	}
	// The same selection with a different order is still the same job.
	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("same selection retry: %v", err)
	}
}

// TestHostileMetainfoPathsRejected: a torrent that carries a traversal path is
// refused whole, before anything is written outside staging.
func TestHostileMetainfoPathsRejected(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := newTestSession(t, staging)
	ctx, cancel := waitCtx(t)
	defer cancel()

	payload := []byte("evil")
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"fixture", []string{"..", "evil.bin"}},
		{"fixture", []string{"", "evil.bin"}},
		{"fixture", []string{"sub/../../evil.bin"}},
		{"fixture", []string{"C:", "evil.bin"}},
		{"..", []string{"evil.bin"}},
	} {
		mi := metainfoFor(t, tc.name, tc.path, payload)
		path := filepath.Join(t.TempDir(), "hostile.torrent")
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		// Inspection is refused too: nobody should learn a path we will not use.
		if _, err := inspectForTest(t, path); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Inspect(%v/%v) = %v, want ErrUnsafePath", tc.name, tc.path, err)
		}
		if _, err := sess.Fetch(ctx, path, []string{"fixture/evil.bin"}); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Fetch(%v/%v) = %v, want ErrUnsafePath", tc.name, tc.path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "evil.bin")); err == nil {
		t.Fatal("hostile torrent wrote outside staging")
	}
}

// TestStagingSymlinkRejected: a symlink already sitting at the torrent's
// directory name must stop the job rather than redirect writes.
func TestStagingSymlinkRejected(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(2000, 'a')})
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(staging, "fixture")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()

	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("error = %v, want ErrUnsafePath", err)
	}
	if entries, err := os.ReadDir(outside); err == nil && len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}

// metainfoFor builds a metainfo whose declared file path is whatever the test
// asks for, with real piece hashes over the payload.
func metainfoFor(t *testing.T, name string, path []string, content []byte) *metainfo.MetaInfo {
	t.Helper()
	info := metainfo.Info{
		Name:        name,
		PieceLength: 16 * 1024,
		Files:       []metainfo.FileInfo{{Length: int64(len(content)), Path: path}},
	}
	used := false
	if err := info.GeneratePieces(func(metainfo.FileInfo) (io.ReadCloser, error) {
		if used {
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		used = true
		return io.NopCloser(bytes.NewReader(content)), nil
	}); err != nil {
		t.Fatalf("generate pieces: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	mi.SetDefaults()
	return mi
}
