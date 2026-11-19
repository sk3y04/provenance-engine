package bittorrent

// Offline recheck: acceptance tests for BT-06. Every case is hermetic (loopback
// seeder, direct discovery) and asserts either bytes on disk or the report, never
// a timing window.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// recheckSnapshot records every regular file under root: digest, size and mtime.
func recheckSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:]) + "|" + time.Unix(0, info.ModTime().UnixNano()).UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func recheckDiff(before, after map[string]string) []string {
	var diffs []string
	for k, v := range before {
		if after[k] != v {
			if _, ok := after[k]; !ok {
				diffs = append(diffs, "removed: "+k)
			} else {
				diffs = append(diffs, "changed: "+k)
			}
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			diffs = append(diffs, "created: "+k)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// recheckIdentity parses a bare v1 hash into an Identity.
func recheckIdentity(t *testing.T, hexHash string) Identity {
	t.Helper()
	id, err := parseIdentityString(hexHash)
	if err != nil {
		t.Fatalf("parse identity %s: %v", hexHash, err)
	}
	return id
}

// TestRecheckHealthyWorkspaceChangesNothing is acceptance 1: a committed isolated
// workspace rechecks offline to exact agreement, and every file the job owns is
// byte-identical afterwards.
func TestRecheckHealthyWorkspaceChangesNothing(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(120000, 71)}
	seed := newSeeder(t, "fixture", 32<<10, files)
	root := t.TempDir()

	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, seed.peer()), seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}
	ws := cap.StagingDir
	before := recheckSnapshot(t, ws)

	report, err := Recheck(context.Background(), ws, cap.JobID)
	if err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if !report.Verified {
		t.Fatalf("healthy workspace not verified: %+v (%s)", report.Summary, report.UnverifiedProblem())
	}
	if report.Summary.Selected != 1 || report.Summary.Match != 1 {
		t.Fatalf("summary = %+v, want 1 selected and 1 match", report.Summary)
	}
	if report.Files[0].SHA256 != sha256Hex(files["a.bin"]) {
		t.Errorf("recomputed digest = %s, want %s", report.Files[0].SHA256, sha256Hex(files["a.bin"]))
	}
	if report.Files[0].AttestedSHA256 != report.Files[0].SHA256 {
		t.Errorf("attested %s != actual %s", report.Files[0].AttestedSHA256, report.Files[0].SHA256)
	}
	if report.Capture == nil || report.Capture.Key != cap.CaptureKey || !report.Capture.Present {
		t.Errorf("capture reference = %+v, want key %s and a present manifest", report.Capture, cap.CaptureKey)
	}
	if report.Capture.Manifest != cap.ManifestPath {
		t.Errorf("manifest reference %q, want %q", report.Capture.Manifest, cap.ManifestPath)
	}
	// No lock-sidecar surprise: the same file set, same bytes, same mtimes.
	if diffs := recheckDiff(before, recheckSnapshot(t, ws)); len(diffs) != 0 {
		t.Errorf("recheck changed the workspace: %v", diffs)
	}
	lockPath := filepath.Join(ws, filepath.FromSlash(torrentStateDir), "staging.lock")
	if fi, serr := os.Stat(lockPath); serr != nil {
		t.Fatalf("lock file: %v", serr)
	} else if fi.Size() != 0 {
		t.Errorf("lock file grew to %d bytes", fi.Size())
	}
}

// TestRecheckDetectsCorruptionAndRepairReconciles is acceptance 2: same-length
// corruption is reported, the historical capture is untouched, and the existing
// transfer path repairs it back to the same capture key and one run.
func TestRecheckDetectsCorruptionAndRepairReconciles(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(80000, 73)}
	seed := newSeeder(t, "fixture", 16<<10, files)
	root := t.TempDir()
	cfg := isolatedConfig(root, seed.peer())

	cap, err := FetchIsolated(context.Background(), cfg, seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}
	ws := cap.StagingDir
	payload := filepath.Join(ws, "fixture", "a.bin")
	manifestPath := filepath.Join(ws, filepath.FromSlash(cap.ManifestPath))
	captureBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	itemsBefore := recheckSnapshot(t, filepath.Join(ws, ".provenance", "items"))

	// Flip bytes without changing the length (finalized files are 0444).
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	data[4096] ^= 0xff
	if err := os.WriteFile(payload, data, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Recheck(context.Background(), ws, cap.JobID)
	if err != nil {
		t.Fatalf("Recheck on corrupt payload: %v", err)
	}
	if report.Verified {
		t.Fatal("corrupt payload reported verified")
	}
	if report.Summary.Mismatch != 1 {
		t.Fatalf("summary = %+v, want 1 mismatch", report.Summary)
	}
	if report.Files[0].Status != RecheckMismatch {
		t.Fatalf("status = %s, want %s", report.Files[0].Status, RecheckMismatch)
	}
	if report.Files[0].Bytes != report.Files[0].AttestedBytes {
		t.Errorf("reported length change %d vs %d; the corruption kept the length",
			report.Files[0].Bytes, report.Files[0].AttestedBytes)
	}
	if !strings.Contains(report.UnverifiedProblem(), "no longer match") {
		t.Errorf("problem = %q, want a mismatch description", report.UnverifiedProblem())
	}
	// The historical capture is not touched by a failed recheck.
	if after, rerr := os.ReadFile(manifestPath); rerr != nil || string(after) != string(captureBefore) {
		t.Errorf("run manifest changed during recheck (%v)", rerr)
	}
	if diffs := recheckDiff(itemsBefore, recheckSnapshot(t, filepath.Join(ws, ".provenance", "items"))); len(diffs) != 0 {
		t.Errorf("capture items changed during recheck: %v", diffs)
	}

	// Repair stays a separate, explicit operation: the existing transfer path.
	repaired, err := FetchIsolated(context.Background(), cfg, seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("repair via FetchIsolated: %v", err)
	}
	if repaired.CaptureKey != cap.CaptureKey {
		t.Errorf("repair capture key %s, want %s", repaired.CaptureKey, cap.CaptureKey)
	}
	if got := len(captureManifests(t, ws)); got != 1 {
		t.Errorf("%d run manifests after repair, want 1", got)
	}
	restored, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(files["a.bin"]) {
		t.Error("repair did not restore the original bytes")
	}
	if !repaired.Reconciled {
		t.Error("repair wrote a new capture instead of reconciling the existing one")
	}
	// And the repaired workspace rechecks clean, offline.
	after, err := Recheck(context.Background(), ws, cap.JobID)
	if err != nil {
		t.Fatalf("Recheck after repair: %v", err)
	}
	if !after.Verified {
		t.Errorf("repaired workspace not verified: %+v", after.Summary)
	}
}

// TestRecheckNeverReportsUncheckedFilesVerified is acceptance 3: missing, partial,
// unattested, invalid and escaping cases must all land on a non-verified report.
func TestRecheckNeverReportsUncheckedFilesVerified(t *testing.T) {
	const v1 = "1111111111111111111111111111111111111111"

	// writeRoot builds a minimal job root with a record and optional payload.
	writeRoot := func(t *testing.T, selection []string, opts func(root string)) string {
		t.Helper()
		root := t.TempDir()
		for _, dir := range []string{torrentStateDir, filepath.Join(".provenance", "items"), "fixture"} {
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		rec := map[string]any{
			"format":    JobFormatVersion,
			"job_id":    "bt-" + v1,
			"backend":   Backend,
			"info_hash": v1,
			"source":    "magnet:?xt=urn:btih:" + v1,
			"selection": selection,
			"resume": map[string]any{
				"metainfo_path": metainfoRelFor(recheckIdentity(t, v1)),
				"data_dir":      ".",
				"part_files":    true,
			},
			"state":      map[string]any{"payload": "payload-pending", "capture": "capture-pending", "seeding": "not-seeding"},
			"created_at": time.Now().UTC(),
			"updated_at": time.Now().UTC(),
		}
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(torrentStateDir), "bt-"+v1+".job.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if opts != nil {
			opts(root)
		}
		return root
	}
	writeItem := func(t *testing.T, root, rel, sum string, size int64) {
		t.Helper()
		item := map[string]any{
			"external_id":     v1 + ":" + rel,
			"extractor":       Backend,
			"downloaded_path": rel,
			"byte_size":       size,
			"sha256":          sum,
			"captured_at":     time.Now().UTC(),
			"status":          "ok",
		}
		data, _ := json.MarshalIndent(item, "", "  ")
		path := manifest.ItemPath(root, v1+":"+rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("missing", func(t *testing.T) {
		root := writeRoot(t, []string{"fixture/a.bin"}, func(root string) {
			writeItem(t, root, "fixture/a.bin", strings.Repeat("ab", 32), 10)
		})
		report, err := Recheck(context.Background(), root, "bt-"+v1)
		if err != nil {
			t.Fatalf("Recheck: %v", err)
		}
		if report.Verified || report.Summary.Missing != 1 {
			t.Fatalf("missing file: verified=%v summary=%+v", report.Verified, report.Summary)
		}
	})

	t.Run("partial only", func(t *testing.T) {
		root := writeRoot(t, []string{"fixture/a.bin"}, func(root string) {
			writeItem(t, root, "fixture/a.bin", strings.Repeat("ab", 32), 10)
			if err := os.WriteFile(filepath.Join(root, "fixture", "a.bin.part"), []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		report, err := Recheck(context.Background(), root, "bt-"+v1)
		if err != nil {
			t.Fatalf("Recheck: %v", err)
		}
		if report.Verified || report.Summary.Partial != 1 {
			t.Fatalf("partial: verified=%v summary=%+v", report.Verified, report.Summary)
		}
		if report.Files[0].SHA256 != "" {
			t.Error("recheck hashed a partial file")
		}
	})

	t.Run("unattested", func(t *testing.T) {
		root := writeRoot(t, []string{"fixture/a.bin"}, func(root string) {
			if err := os.WriteFile(filepath.Join(root, "fixture", "a.bin"), []byte("unattested bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		report, err := Recheck(context.Background(), root, "bt-"+v1)
		if err != nil {
			t.Fatalf("Recheck: %v", err)
		}
		if report.Verified || report.Summary.Unverifiable != 1 {
			t.Fatalf("unattested: verified=%v summary=%+v", report.Verified, report.Summary)
		}
		if report.Files[0].SHA256 == "" {
			t.Error("recheck did not hash readable unattested bytes")
		}
		if report.Capture != nil {
			t.Error("unattested job reported a capture reference")
		}
		if report.Detail == "" {
			t.Error("unattested job gave no explanation")
		}
	})

	t.Run("escaping path", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "escape.txt")
		if err := os.WriteFile(outside, []byte("outside the root"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := writeRoot(t, []string{"../escape.txt"}, func(root string) {
			writeItem(t, root, "../escape.txt", strings.Repeat("cd", 32), 16)
		})
		report, err := Recheck(context.Background(), root, "bt-"+v1)
		if err != nil {
			t.Fatalf("Recheck: %v", err)
		}
		if report.Verified || report.Summary.Error != 1 {
			t.Fatalf("escaping path: verified=%v summary=%+v", report.Verified, report.Summary)
		}
		if report.Files[0].SHA256 != "" {
			t.Fatal("recheck hashed a file outside the job root")
		}
	})

	t.Run("symlink at selected path", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real.bin")
		if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := writeRoot(t, []string{"fixture/a.bin"}, func(root string) {
			if err := os.Symlink(target, filepath.Join(root, "fixture", "a.bin")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		})
		report, err := Recheck(context.Background(), root, "bt-"+v1)
		if err != nil {
			t.Fatalf("Recheck: %v", err)
		}
		if report.Verified || report.Summary.Error != 1 {
			t.Fatalf("symlink: verified=%v summary=%+v", report.Verified, report.Summary)
		}
	})

	t.Run("invalid record", func(t *testing.T) {
		root := writeRoot(t, []string{"fixture/a.bin"}, nil)
		path := filepath.Join(root, filepath.FromSlash(torrentStateDir), "bt-"+v1+".job.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Recheck(context.Background(), root, "bt-"+v1); err == nil {
			t.Fatal("invalid record produced a report instead of an error")
		}
	})

	t.Run("record for another identity", func(t *testing.T) {
		root := writeRoot(t, []string{"fixture/a.bin"}, nil)
		other := "2222222222222222222222222222222222222222"
		if _, err := Recheck(context.Background(), root, "bt-"+other); err == nil {
			t.Fatal("a record filed under another id was accepted")
		}
	})
}

// TestRecheckCancellationDuringHashing is acceptance 4 (first half): the chunk
// hook cancels deterministically part-way through a specific file, and the report
// says so instead of claiming verified.
func TestRecheckCancellationDuringHashing(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(3<<20, 79)}
	seed := newSeeder(t, "fixture", 64<<10, files)
	root := t.TempDir()
	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, seed.peer()), seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var chunks int
	prev := recheckHashChunkHook
	recheckHashChunkHook = func() {
		chunks++
		if chunks >= 1 {
			cancel()
		}
	}
	t.Cleanup(func() { recheckHashChunkHook = prev })

	report, err := Recheck(ctx, cap.StagingDir, cap.JobID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recheck error = %v, want context.Canceled", err)
	}
	if report.Verified {
		t.Fatal("cancelled recheck reported verified")
	}
	if report.Error == "" {
		t.Error("cancelled report carries no error field, so JSON would hide the failure")
	}
	if chunks == 0 {
		t.Fatal("the hash hook never ran; the test proves nothing")
	}
}

// TestRecheckRefusesLiveOwner is acceptance 4 (second half): a competing owner is a
// documented busy error, not a guess about liveness from a lock file.
func TestRecheckRefusesLiveOwner(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(40000, 83)}
	seed := newSeeder(t, "fixture", 16<<10, files)
	root := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cap, err := FetchIsolated(ctx, isolatedConfig(root, seed.peer()), seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}

	// A second session on that workspace is a live owner: it holds the same lock a
	// fetch takes, so recheck refuses rather than certifying bytes mid-write.
	owner, err := OpenIsolated(context.Background(), isolatedConfig(root, seed.peer()), seed.magnet())
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	if _, err := Recheck(context.Background(), owner.Workspace(), cap.JobID); !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("recheck against a live owner = %v, want ErrStagingBusy", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}

	// After the owner exits, the lock sidecar left on disk does not make the job
	// look busy: ownership is the held lock, not the file's existence.
	lockPath := filepath.Join(cap.StagingDir, filepath.FromSlash(torrentStateDir), "staging.lock")
	if _, serr := os.Stat(lockPath); serr != nil {
		t.Fatalf("expected a leftover lock file: %v", serr)
	}
	report, err := Recheck(context.Background(), cap.StagingDir, cap.JobID)
	if err != nil {
		t.Fatalf("recheck with a leftover lock file: %v", err)
	}
	if !report.Verified {
		t.Fatalf("recheck with a leftover lock file: %+v", report.Summary)
	}
}

// TestRecheckLegacyAndHybridAlias is acceptance 5: a legacy staged job rechecks
// through the legacy layout, and a hybrid isolated workspace is found from its v2
// hash through the existing alias rule.
func TestRecheckLegacyAndHybridAlias(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		files := map[string][]byte{"a.bin": patterned(50000, 89)}
		seed := newSeeder(t, "fixture", 16<<10, files)
		root := t.TempDir()
		sess := discSession(t, Config{StagingDir: root, Peers: []string{seed.peer()}, Discovery: DiscoveryDirect,
			MetadataTimeout: 30 * time.Second, Timeout: 90 * time.Second, MaxPeers: 4})
		cap, err := sess.Fetch(context.Background(), seed.magnet(), []string{"fixture/a.bin"})
		if err != nil {
			t.Fatalf("legacy fetch: %v", err)
		}
		// A live legacy session owns the root, so recheck refuses exactly as it does
		// for a workspace owner.
		if _, err := Recheck(context.Background(), root, cap.JobID); !errors.Is(err, ErrStagingBusy) {
			t.Fatalf("recheck against a live legacy session = %v, want ErrStagingBusy", err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("close legacy session: %v", err)
		}
		report, err := Recheck(context.Background(), root, cap.JobID)
		if err != nil {
			t.Fatalf("Recheck legacy: %v", err)
		}
		if !report.Verified {
			t.Fatalf("legacy job not verified: %+v", report.Summary)
		}
		if report.Capture == nil || report.Capture.Key != cap.CaptureKey {
			t.Errorf("legacy capture reference = %+v", report.Capture)
		}
	})

	t.Run("hybrid alias in a workspace", func(t *testing.T) {
		files := map[string][]byte{"a.bin": patterned(96<<10, 97)}
		mi := hybridMetaInfo(t, "hybridfixture", 32<<10, files)
		id := identityOfMetainfo(t, mi)
		seed := newMetaSeeder(t, "hybridfixture", files, mi, 0)
		root := t.TempDir()
		cap, err := FetchIsolated(context.Background(), isolatedConfig(root, seed.peer()), seed.torrentFile(t), []string{"hybridfixture/a.bin"})
		if err != nil {
			t.Fatalf("FetchIsolated: %v", err)
		}
		// The canonical id is the v1 one; ask by the v2 hash instead.
		if cap.JobID != "bt-"+id.V1Hex() {
			t.Fatalf("job id %s, want bt-%s", cap.JobID, id.V1Hex())
		}
		report, err := Recheck(context.Background(), cap.StagingDir, id.V2Hex())
		if err != nil {
			t.Fatalf("Recheck by v2 alias: %v", err)
		}
		if report.JobID != cap.JobID || !report.Verified {
			t.Fatalf("alias recheck: job %s verified=%v summary=%+v", report.JobID, report.Verified, report.Summary)
		}
		if report.InfoHashV2 != id.V2Hex() {
			t.Errorf("report v2 hash = %q, want %q", report.InfoHashV2, id.V2Hex())
		}
	})
}
