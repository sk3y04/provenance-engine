package bittorrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// This file covers recovery and failure paths that only a deterministic
// fixture can show: retained bytes that are corrupt without changing length, a
// storage layer that refuses writes or never promotes a part file, and
// cancellation at a known boundary.
//
// The distinction that matters: TestSharedPieceCorruptionIsRepairedNotCaptured
// corrupts real persisted state through the production path (a stopped session,
// bytes changed on disk). The fault-storage tests below inject failures through
// a storage double and are labelled as such; they are not OS error tests.

var errInjectedWrite = errors.New("injected storage write failure")
var errInjectedMark = errors.New("injected mark-complete failure")

// faultStorage wraps a real storage implementation and injects failures. It is
// a deterministic stand-in for a full disk or a read-only mount, not an
// emulation of one: no OS call fails in these tests.
type faultStorage struct {
	inner storage.ClientImpl

	failWrite atomic.Bool
	failMark  atomic.Bool

	// onRead runs before each piece read (verification hashing reads pieces).
	onRead func()
	// enterMark receives once when MarkComplete is entered.
	enterMark chan struct{}
	// markGate blocks MarkComplete until closed.
	markGate chan struct{}
}

func newFaultStorage(inner storage.ClientImpl) *faultStorage {
	return &faultStorage{inner: inner}
}

func (f *faultStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	inner, err := f.inner.OpenTorrent(ctx, info, ih)
	if err != nil {
		return storage.TorrentImpl{}, err
	}
	out := inner
	wrap := func(p storage.PieceImpl) storage.PieceImpl {
		if p == nil {
			return nil
		}
		return &faultPiece{inner: p, f: f}
	}
	if inner.Piece != nil {
		out.Piece = func(p metainfo.Piece) storage.PieceImpl { return wrap(inner.Piece(p)) }
	}
	if inner.PieceWithHash != nil {
		out.PieceWithHash = func(p metainfo.Piece, h g.Option[[]byte]) storage.PieceImpl {
			return wrap(inner.PieceWithHash(p, h))
		}
	}
	return out, nil
}

type faultPiece struct {
	inner storage.PieceImpl
	f     *faultStorage
}

func (p *faultPiece) ReadAt(b []byte, off int64) (int, error) {
	if p.f.onRead != nil {
		p.f.onRead()
	}
	return p.inner.ReadAt(b, off)
}

func (p *faultPiece) WriteAt(b []byte, off int64) (int, error) {
	if p.f.failWrite.Load() {
		return 0, errInjectedWrite
	}
	return p.inner.WriteAt(b, off)
}

func (p *faultPiece) MarkComplete() error {
	if p.f.enterMark != nil {
		select {
		case p.f.enterMark <- struct{}{}:
		default:
		}
	}
	if p.f.markGate != nil {
		<-p.f.markGate
	}
	if p.f.failMark.Load() {
		// The real storage never promotes the part file, which is exactly what
		// a failed rename looks like from the adapter's side.
		return errInjectedMark
	}
	return p.inner.MarkComplete()
}

func (p *faultPiece) MarkNotComplete() error { return p.inner.MarkNotComplete() }

func (p *faultPiece) Completion() storage.Completion { return p.inner.Completion() }

// newFaultSession opens a Session whose payload storage is the given double.
func newFaultSession(t *testing.T, staging string, faults *faultStorage, peers ...string) *Session {
	t.Helper()
	return newTestSessionWithStorage(t, staging, faults, peers...)
}

// runManifestDigests returns a path->content-hash map of the published capture
// manifests. Publication is the run manifest, not the per-item files, so this
// is what "nothing was published" is asserted against.
func runManifestDigests(t *testing.T, staging string) map[string]string {
	t.Helper()
	out := map[string]string{}
	paths, err := manifest.ListRunManifests(staging)
	if err != nil {
		t.Fatalf("list run manifests: %v", err)
	}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		out[p] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	return out
}

// corruptBytes rewrites a byte range of a finalized file without changing its
// length, as a bad disk or an interfering sync tool would.
func corruptBytes(t *testing.T, path string, offset int64, replacement []byte) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := f.WriteAt(replacement, offset); err != nil {
		t.Fatalf("corrupt %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("corruption changed the length of %s (%d -> %d)", path, before.Size(), after.Size())
	}
}

// sharedPieceFixture is the layout that makes reuse measurable:
//
//	piece 0: a.bin[0:32KiB]                 (selected only)
//	piece 1: a.bin[32KiB:48KiB] + b.bin     (shared with the unselected neighbour)
//
// Corrupting a.bin at 40KiB damages the shared piece, leaves the length alone,
// and leaves the piece-completion database claiming the piece is done.
func sharedPieceFixture(t *testing.T) (sel *seeder, a, b []byte, selected string) {
	t.Helper()
	a = patterned(48<<10, 'a')
	b = patterned(16<<10, 'b')
	sel = newSeeder(t, "fixture", 32*1024, map[string][]byte{"a.bin": a, "b.bin": b})
	return sel, a, b, "fixture/a.bin"
}

// TestSharedPieceCorruptionIsRepairedNotCaptured is the persisted-corruption
// acceptance case: the selected file shares a piece with an unselected
// neighbour, the owner is stopped, and retained bytes are damaged in place
// (same length, completion database untouched).
func TestSharedPieceCorruptionIsRepairedNotCaptured(t *testing.T) {
	sel, a, b, selected := sharedPieceFixture(t)
	staging := t.TempDir()

	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	first, err := sess.Fetch(ctx, sel.magnet(), []string{selected})
	cancel()
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if first.Assets[0].SHA256 != sha256Hex(a) {
		t.Fatalf("first capture hash = %s", first.Assets[0].SHA256)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close first session: %v", err)
	}
	manifestsBefore := runManifestDigests(t, staging)
	if len(manifestsBefore) != 1 {
		t.Fatalf("run manifests after first fetch = %d, want 1", len(manifestsBefore))
	}

	// Damage the selected file inside the shared piece.
	target := filepath.Join(staging, "fixture", "a.bin")
	corruptBytes(t, target, 40<<10, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if got, _ := os.ReadFile(target); bytes.Equal(got, a) {
		t.Fatal("fixture corruption did not change the file")
	}

	sess2 := newTestSession(t, staging, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	second, err := sess2.Fetch(ctx2, sel.magnet(), []string{selected})
	cancel2()
	if err != nil {
		t.Fatalf("repair fetch: %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, a) {
		t.Fatal("repaired file does not match the fixture")
	}
	if second.Assets[0].SHA256 != sha256Hex(a) {
		t.Fatalf("captured hash = %s, want %s (damaged bytes must never be captured)",
			second.Assets[0].SHA256, sha256Hex(a))
	}

	// Only the selected file is captured, and only under its fixture hash.
	for _, cm := range captureManifests(t, staging) {
		if len(cm.Items) != 1 || cm.Items[0].DownloadedPath != selected {
			t.Fatalf("capture items = %+v, want only %s", cm.Items, selected)
		}
		if cm.Items[0].Sha256 != sha256Hex(a) {
			t.Fatalf("a capture manifest recorded hash %s, want %s", cm.Items[0].Sha256, sha256Hex(a))
		}
	}

	// Reuse is asserted from measured payload bytes, not from timing: only the
	// damaged piece (32 KiB) may be fetched again, and never the whole payload.
	total := int64(len(a) + len(b))
	if second.BytesRead <= 0 {
		t.Fatal("repair read nothing from the peer; corrupt bytes may have been captured")
	}
	if second.BytesRead >= total {
		t.Errorf("repair read %d bytes of %d; intact pieces were not reused", second.BytesRead, total)
	}
	if second.BytesRead > 2*32768 {
		t.Errorf("repair read %d bytes, want at most two pieces", second.BytesRead)
	}

	// The historical capture is the same one: a repaired payload that matches
	// the recorded identity must reconcile, not duplicate.
	manifestsAfter := runManifestDigests(t, staging)
	if len(manifestsAfter) != 1 {
		t.Fatalf("run manifests after repair = %d, want 1", len(manifestsAfter))
	}
	for p, hash := range manifestsBefore {
		if manifestsAfter[p] != hash {
			t.Errorf("run manifest %s was rewritten", p)
		}
	}
	if !second.Reconciled {
		t.Error("repaired payload did not reconcile to the existing capture")
	}
}

// TestUnrepairableCorruptionPublishesNothing is the same damage with no peer:
// the attempt must be bounded by the caller's context, must not publish a run
// manifest, must not mark the payload ready, and must leave the historical
// capture untouched.
func TestUnrepairableCorruptionPublishesNothing(t *testing.T) {
	sel, a, _, selected := sharedPieceFixture(t)
	staging := t.TempDir()

	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	if _, err := sess.Fetch(ctx, sel.magnet(), []string{selected}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	cancel()
	if err := sess.Close(); err != nil {
		t.Fatalf("close first session: %v", err)
	}
	before := runManifestDigests(t, staging)
	if len(before) != 1 {
		t.Fatalf("run manifests = %d, want 1", len(before))
	}
	jobBefore := jobRecordFor(t, staging, sel.infohash())
	if jobBefore.State.Payload != PayloadReady || jobBefore.State.Capture != CaptureCommitted {
		t.Fatalf("state before corruption = %+v", jobBefore.State)
	}

	target := filepath.Join(staging, "fixture", "a.bin")
	corruptBytes(t, target, 40<<10, []byte{0x01, 0x02, 0x03, 0x04})

	// No peers are passed: the swarm cannot repair anything.
	sess2 := newTestSession(t, staging)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	start := time.Now()
	_, err := sess2.Fetch(ctx2, sel.magnet(), []string{selected})
	waited := time.Since(start)
	if err == nil {
		t.Fatal("fetch over unrepairable corruption reported success")
	}
	if !errors.Is(err, ErrTransferTimeout) {
		t.Errorf("error = %v, want a bounded transfer timeout", err)
	}
	if waited > 60*time.Second {
		t.Errorf("bounded attempt took %s", waited)
	}

	// Nothing was published by the failed attempt.
	after := runManifestDigests(t, staging)
	if len(after) != 1 {
		t.Fatalf("run manifests after failed repair = %d, want the original 1", len(after))
	}
	for p, hash := range before {
		if after[p] != hash {
			t.Errorf("run manifest %s changed during a failed repair", p)
		}
	}
	job := jobRecordFor(t, staging, sel.infohash())
	if job.State.Payload == PayloadReady {
		t.Errorf("damaged bytes were marked ready (state %+v)", job.State)
	}
	if job.State.Capture != CaptureCommitted {
		t.Errorf("historical capture state changed to %q", job.State.Capture)
	}
	if got, _ := os.ReadFile(target); bytes.Equal(got, a) {
		t.Error("the staged file cannot have been repaired without a peer")
	}
	if err := sess2.Close(); err != nil {
		t.Errorf("close after failed repair: %v", err)
	}
}

// TestStorageWriteFailureIsDiagnosable: a refused piece write must surface as a
// diagnosable error instead of a silent stall (the library default logs it and
// stops downloading). Injected, not an OS failure.
func TestStorageWriteFailureIsDiagnosable(t *testing.T) {
	content := patterned(40000, 'w')
	sel := newSeeder(t, "fixture", 16*1024, map[string][]byte{"w.bin": content})
	staging := t.TempDir()

	faults := newFaultStorage(storage.NewFile(staging))
	faults.failWrite.Store(true)
	sess := newFaultSession(t, staging, faults, sel.peer())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := sess.Fetch(ctx, sel.magnet(), []string{"fixture/w.bin"})
	if err == nil {
		t.Fatal("fetch succeeded despite a storage write failure")
	}
	if !errors.Is(err, ErrStorageFailure) {
		t.Fatalf("error = %v, want ErrStorageFailure (bounded, diagnosable)", err)
	}
	if waited := time.Since(start); waited > 25*time.Second {
		t.Errorf("failure took %s; it should not wait for the deadline", waited)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("injected storage write failure")) {
		t.Errorf("error lost the cause: %v", err)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("run manifests = %d, want 0 after a write failure", n)
	}
	job := jobRecordFor(t, staging, sel.infohash())
	if job.State.Payload == PayloadReady || job.State.Capture == CaptureCommitted {
		t.Errorf("a failed write produced state %+v", job.State)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close after write failure: %v", err)
	}

	// Removing the fault must let a retry complete.
	staging2 := t.TempDir()
	sess2 := newTestSession(t, staging2, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, sel.magnet(), []string{"fixture/w.bin"})
	if err != nil {
		t.Fatalf("retry without the fault: %v", err)
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("retry captured %s", cap.Assets[0].SHA256)
	}
	if n := manifestCount(t, staging2); n != 1 {
		t.Errorf("retry published %d manifests, want 1", n)
	}
}

// TestFinalizationFailureIsDiagnosable: pieces verify but the storage layer
// never promotes the part file (the library only logs that). The adapter must
// report it within a bounded grace, publish nothing, and recover on retry.
func TestFinalizationFailureIsDiagnosable(t *testing.T) {
	content := patterned(40000, 'm')
	sel := newSeeder(t, "fixture", 16*1024, map[string][]byte{"m.bin": content})
	staging := t.TempDir()

	faults := newFaultStorage(storage.NewFile(staging))
	faults.failMark.Store(true)
	sess := newFaultSession(t, staging, faults, sel.peer())
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	start := time.Now()
	_, err := sess.Fetch(ctx, sel.magnet(), []string{"fixture/m.bin"})
	waited := time.Since(start)
	if err == nil {
		t.Fatal("fetch succeeded although finalization never happened")
	}
	if !errors.Is(err, ErrNotFinalized) {
		t.Fatalf("error = %v, want ErrNotFinalized with a finalization diagnosis", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("never finalized")) {
		t.Errorf("error does not name the finalization failure: %v", err)
	}
	if waited > 30*time.Second {
		t.Errorf("diagnosis took %s; it should be bounded by the grace, not the deadline", waited)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("run manifests = %d, want 0 after a finalization failure", n)
	}
	job := jobRecordFor(t, staging, sel.infohash())
	if job.State.Payload == PayloadReady || job.State.Capture == CaptureCommitted {
		t.Errorf("a never-finalized file produced state %+v", job.State)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close after finalization failure: %v", err)
	}

	staging2 := t.TempDir()
	sess2 := newTestSession(t, staging2, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, sel.magnet(), []string{"fixture/m.bin"})
	if err != nil {
		t.Fatalf("retry without the fault: %v", err)
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("retry captured %s", cap.Assets[0].SHA256)
	}
}

// TestCancelDuringVerification: cancelling while retained pieces are being
// re-hashed must not publish, must close cleanly, and must leave ownership
// available to the next caller.
func TestCancelDuringVerification(t *testing.T) {
	content := patterned(512<<10, 'v')
	// A rate-limited seeder keeps the first pass genuinely partial, so the
	// second pass has retained data to verify.
	sel := newSeederRateLimited(t, "fixture", 32*1024, map[string][]byte{"v.bin": content}, 256<<10)
	staging := t.TempDir()

	cfg := testConfig(t, staging, sel.peer())
	ctx, cancel := context.WithCancel(context.Background())
	cfg.OnUpdate = func(u Update) {
		if u.Written >= 128<<10 {
			cancel()
		}
	}
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := sess.Fetch(ctx, sel.magnet(), []string{"fixture/v.bin"}); err == nil {
		t.Fatal("interrupted fetch reported success")
	}
	cancel()
	if err := sess.Close(); err != nil {
		t.Fatalf("close interrupted session: %v", err)
	}
	if parts := listPartFiles(t, staging); len(parts) == 0 {
		t.Fatal("interruption left no retained data; the test would prove nothing")
	}
	if job := jobRecordFor(t, staging, sel.infohash()); job.State.Payload != PayloadPending {
		t.Fatalf("payload state after interruption = %q, want pending", job.State.Payload)
	}

	// The double cancels exactly when verification starts reading retained
	// pieces, which only happens on the transfer path, not on adoption.
	verifyCtx, verifyCancel := context.WithCancel(context.Background())
	defer verifyCancel()
	var once sync.Once
	faults := newFaultStorage(storage.NewFile(staging))
	faults.onRead = func() { once.Do(verifyCancel) }
	sess2 := newFaultSession(t, staging, faults, sel.peer())
	_, err = sess2.Fetch(verifyCtx, sel.magnet(), []string{"fixture/v.bin"})
	if err == nil {
		t.Fatal("cancelled verification reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want a cancellation", err)
	}

	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("cancellation published %d run manifest(s)", n)
	}
	job := jobRecordFor(t, staging, sel.infohash())
	if job.State.Payload == PayloadReady {
		t.Errorf("cancelled verification left the payload ready: %+v", job.State)
	}
	if err := sess2.Close(); err != nil {
		t.Errorf("close after cancellation: %v", err)
	}

	// Ownership must be available, and a full fetch must still work.
	sess3 := newTestSession(t, staging, sel.peer())
	ctx3, cancel3 := waitCtx(t)
	defer cancel3()
	cap, err := sess3.Fetch(ctx3, sel.magnet(), []string{"fixture/v.bin"})
	if err != nil {
		t.Fatalf("fetch after cancellation: %v", err)
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("post-cancel capture hash = %s", cap.Assets[0].SHA256)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests after recovery = %d, want 1", n)
	}
}

// TestCancelDuringFinalization blocks inside MarkComplete — the boundary between
// verified pieces and a finalized file — cancels, and checks the same
// invariants.
func TestCancelDuringFinalization(t *testing.T) {
	content := patterned(40000, 'f')
	sel := newSeeder(t, "fixture", 16*1024, map[string][]byte{"f.bin": content})
	staging := t.TempDir()

	faults := newFaultStorage(storage.NewFile(staging))
	faults.enterMark = make(chan struct{}, 1)
	faults.markGate = make(chan struct{})
	sess := newFaultSession(t, staging, faults, sel.peer())

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		cap Capture
		err error
	}
	done := make(chan result, 1)
	go func() {
		cap, err := sess.Fetch(ctx, sel.magnet(), []string{"fixture/f.bin"})
		done <- result{cap, err}
	}()

	select {
	case <-faults.enterMark:
	case <-time.After(60 * time.Second):
		t.Fatal("fetch never reached piece finalization")
	}
	cancel()
	close(faults.markGate)

	var res result
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("fetch did not return after cancellation")
	}
	if res.err == nil {
		t.Fatal("cancelled finalization reported success")
	}
	if !errors.Is(res.err, context.Canceled) {
		t.Errorf("error = %v, want a cancellation", res.err)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("run manifests = %d, want 0 after cancelling before publication", n)
	}
	job := jobRecordFor(t, staging, sel.infohash())
	if job.State.Capture == CaptureCommitted || job.State.Payload == PayloadReady {
		t.Errorf("cancelled finalization produced state %+v", job.State)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close after cancellation: %v", err)
	}

	// The next caller gets ownership and completes the same job once.
	sess2 := newTestSession(t, staging, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, sel.magnet(), []string{"fixture/f.bin"})
	if err != nil {
		t.Fatalf("fetch after cancelled finalization: %v", err)
	}
	if cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Errorf("post-cancel capture hash = %s", cap.Assets[0].SHA256)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests after recovery = %d, want 1", n)
	}
}
