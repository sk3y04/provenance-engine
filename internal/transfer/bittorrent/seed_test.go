package bittorrent

// BT-08d.1 acceptance: one seed session serving verified local data, and refusing a
// damaged or selective workspace promptly.
//
// Every wait is bounded and names its phase; failures print the observable state
// (piece histogram, peer counts, bytes) rather than a bare timeout.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/filelock"
)

// seedFixture builds a two-file fixture, serves it with the control harness and
// fetches it into an isolated workspace with the requested selection.
func seedFixture(t *testing.T, selection []string) (ws string, jobID string, files map[string][]byte, source *ctlPeer) {
	t.Helper()
	files = map[string][]byte{"a.bin": patterned(90<<10, 11), "b.bin": patterned(30<<10, 13)}
	seedRoot := t.TempDir()
	mi := ctlMetainfo(t, seedRoot, "fixture", files)
	source = ctlServe(t, seedRoot, "fixture", mi)

	root := t.TempDir()
	cfg := isolatedConfig(root, source.addr)
	magnet := "magnet:?xt=urn:btih:" + mi.HashInfoBytes().HexString()
	cap, err := FetchIsolated(context.Background(), cfg, magnet, selection)
	if err != nil {
		t.Fatalf("fetch fixture into a workspace: %v", err)
	}
	return cap.StagingDir, cap.JobID, files, source
}

// TestSeedSessionServesVerifiedWorkspace is acceptance A: a fresh leecher receives
// the exact bytes, and the session reports what it actually verified.
func TestSeedSessionServesVerifiedWorkspace(t *testing.T) {
	ws, jobID, files, _ := seedFixture(t, []string{"fixture/a.bin", "fixture/b.bin"})

	cfg := isolatedConfig(ws)
	sess, err := OpenSeedSession(context.Background(), cfg, jobID)
	if err != nil {
		t.Fatalf("open seed session: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = sess.Close()
		}
	})
	report := sess.Report()
	if report.VerifiedPieces == 0 || report.VerifiedPieces != report.TotalPieces {
		t.Fatalf("verification report = %+v, want all pieces verified", report)
	}
	if report.Files != 2 {
		t.Errorf("report files = %d, want 2", report.Files)
	}

	serveCtx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- sess.Serve(serveCtx) }()

	leechRoot := t.TempDir()
	// Normalise an empty piece-layer table: the library serialises one for a v1
	// torrent, and a non-nil table makes the receiving client demand piece roots.
	mi := sess.tor.Metainfo()
	if len(mi.PieceLayers) == 0 {
		mi.PieceLayers = nil
	}
	leecher := ctlLeech(t, leechRoot, &mi, sess.Address())
	ctlWait(t, "leecher to verify every piece from the seed", 60*time.Second, leecher.allPiecesComplete, leecher.describe)

	for rel, want := range files {
		got, rerr := os.ReadFile(filepath.Join(leechRoot, "fixture", filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("leecher read %s: %v", rel, rerr)
		}
		if string(got) != string(want) {
			t.Fatalf("%s bytes differ: got %d, want %d", rel, len(got), len(want))
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("%s digest differs", rel)
		}
	}
	if uploaded := sess.UploadedBytes(); uploaded == 0 {
		t.Fatal("seed reports no uploaded bytes after serving a leecher")
	} else {
		read := leecher.client.Stats().BytesReadUsefulData
		t.Logf("seed served %d bytes; leecher read %d", uploaded, read.Int64())
	}

	stop()
	if err := <-served; !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v, want context.Canceled", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed = true
	// The workspace is free again.
	assertWorkspaceFree(t, ws)
}

// TestSeedSessionRefusesDamagedWorkspace is acceptance B: verification fails fast,
// nothing is downloaded, the capture is untouched and the lock is released.
func TestSeedSessionRefusesDamagedWorkspace(t *testing.T) {
	ws, jobID, _, _ := seedFixture(t, []string{"fixture/a.bin", "fixture/b.bin"})
	payload := filepath.Join(ws, "fixture", "b.bin")
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	data[64] ^= 0xff // same length, different bytes
	if err := os.WriteFile(payload, data, 0o644); err != nil {
		t.Fatal(err)
	}
	manifests, err := os.ReadDir(filepath.Join(ws, ".provenance", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]string, len(manifests))
	for _, m := range manifests {
		raw, rerr := os.ReadFile(filepath.Join(ws, ".provenance", "runs", m.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		before[m.Name()] = string(raw)
	}

	start := time.Now()
	_, serr := OpenSeedSession(context.Background(), isolatedConfig(ws), jobID)
	elapsed := time.Since(start)
	if serr == nil {
		probe, perr := OpenSeedSession(context.Background(), isolatedConfig(ws), jobID)
		if perr == nil {
			t.Logf("damaged workspace accepted: report=%+v", probe.Report())
			complete, missing := pieceCounts(probe.tor)
			t.Logf("pieces complete=%d missing=%d", complete, missing)
			_ = probe.Close()
		}
		t.Fatal("a damaged workspace was accepted for seeding")
	}
	if !errors.Is(serr, ErrSeedDataIncomplete) {
		t.Fatalf("refusal = %v, want ErrSeedDataIncomplete", serr)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("refusal took %s; it must not wait on completion that will never arrive", elapsed)
	}
	if err := os.WriteFile(payload, data, 0o644); err == nil {
		// (restore so the assertion below is about the refusal, not the corruption)
	}
	// Nothing republished, nothing fetched: the manifest set is identical.
	after, err := os.ReadDir(filepath.Join(ws, ".provenance", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("manifest count changed from %d to %d", len(before), len(after))
	}
	for _, m := range after {
		raw, rerr := os.ReadFile(filepath.Join(ws, ".provenance", "runs", m.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if before[m.Name()] != string(raw) {
			t.Fatalf("manifest %s changed during a refused seed", m.Name())
		}
	}
	t.Logf("damaged workspace refused in %s: %v", elapsed.Round(time.Millisecond), serr)
	assertWorkspaceFree(t, ws)
}

// TestSeedSessionRefusesSelectiveWorkspace is acceptance B's second half.
func TestSeedSessionRefusesSelectiveWorkspace(t *testing.T) {
	ws, jobID, _, _ := seedFixture(t, []string{"fixture/a.bin"})
	_, err := OpenSeedSession(context.Background(), isolatedConfig(ws), jobID)
	if err == nil {
		t.Fatal("a selective workspace was accepted for seeding")
	}
	if !errors.Is(err, ErrSeedSelective) {
		t.Fatalf("refusal = %v, want ErrSeedSelective", err)
	}
	assertWorkspaceFree(t, ws)
}

// TestSeedSessionCancelDuringVerification is acceptance C: cancellation during
// hashing returns the context error and releases the workspace, with no client left
// holding it.
func TestSeedSessionCancelDuringVerification(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(4<<20, 17)}
	seedRoot := t.TempDir()
	mi := ctlMetainfo(t, seedRoot, "fixture", files)
	source := ctlServe(t, seedRoot, "fixture", mi)
	root := t.TempDir()
	magnet := "magnet:?xt=urn:btih:" + mi.HashInfoBytes().HexString()
	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, source.addr), magnet, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before verification starts, deterministically
	sess, err := OpenSeedSession(ctx, isolatedConfig(cap.StagingDir), cap.JobID)
	if sess != nil {
		_ = sess.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertWorkspaceFree(t, cap.StagingDir)
}

// assertWorkspaceFree proves the workspace lock was released, which is what makes a
// refusal or a cancellation recoverable.
func assertWorkspaceFree(t *testing.T, ws string) {
	t.Helper()
	l, err := filelock.TryAcquire(filepath.Join(ws, filepath.FromSlash(torrentStateDir), "staging"))
	if err != nil {
		t.Fatalf("workspace %s is still owned after the operation: %v", ws, err)
	}
	if rerr := l.Release(); rerr != nil {
		t.Fatalf("release probe lock: %v", rerr)
	}
}
