package torrentqueue

// PR-2: the durable archive handoff. A committed capture is independent from the
// optional vault import; retrying an import must never ask the transfer layer for
// bytes again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeCommittedCapture creates the workspace and run manifest a real transfer
// would have published, then records the completed entry. It does not write
// capture item JSON files: the import path reads the run manifest, which is the
// capture commit point.
func writeCommittedCapture(t *testing.T, store *Store, jobID string, files map[string][]byte) Entry {
	t.Helper()
	entry := addEntry(t, store, jobID)
	ws := filepath.Join(store.Root(), filepath.FromSlash(entry.Workspace))
	items := make([]manifest.CaptureItem, 0, len(files))
	for rel, data := range files {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir payload: %v", err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		items = append(items, manifest.CaptureItem{
			ExternalID:     strings.TrimPrefix(jobID, "bt-") + ":" + rel,
			URL:            "magnet:?xt=urn:btih:" + strings.TrimPrefix(jobID, "bt-"),
			Title:          filepath.Base(rel),
			Kind:           "binary",
			Extractor:      "bittorrent",
			DownloadedPath: rel,
			ByteSize:       int64(len(data)),
			Sha256:         sha256Hex(data),
			CapturedAt:     time.Now().UTC(),
			Status:         "ok",
		})
	}
	cm := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    "magnet:?xt=urn:btih:" + strings.TrimPrefix(jobID, "bt-"),
		Site:         "bittorrent",
		DownloadedAt: time.Now().UTC(),
		OutputDir:    ws,
		Tool:         "provenance",
		ToolVersion:  "test",
		CaptureKey:   "cap-" + jobID,
		Items:        items,
	}
	runPath := manifest.RunPath(ws)
	if err := manifest.WriteCaptureManifest(ws, runPath, cm); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	relRun, err := filepath.Rel(ws, runPath)
	if err != nil {
		t.Fatalf("relative manifest: %v", err)
	}
	got, uerr := store.Update(entry.ID, func(e *Entry) error {
		e.Phase = PhaseCompleted
		e.Desired = DesiredRun
		e.Capture = &CaptureRef{
			Key:         cm.CaptureKey,
			Manifest:    filepath.ToSlash(relRun),
			CommittedAt: cm.DownloadedAt,
		}
		e.Note = ""
		return nil
	})
	if uerr != nil {
		t.Fatalf("record capture: %v", uerr)
	}
	return got
}

func withImportTarget(t *testing.T, store *Store, id, vault string) Entry {
	t.Helper()
	entry, err := store.SetImportTarget(id, ImportTarget{VaultRoot: vault, Collection: "torrent-test"})
	if err != nil {
		t.Fatalf("set import target: %v", err)
	}
	return entry
}

func TestQueueImportHandoffIsDurableAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	vault := t.TempDir()
	entry := writeCommittedCapture(t, store, testJobID(1), map[string][]byte{"fixture/a.bin": []byte("payload bytes")})
	withImportTarget(t, store, entry.ID, vault)

	got, err := store.ImportCapture(entry.ID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got.Phase != PhaseCompleted || got.Capture == nil || got.Capture.Key != "cap-"+testJobID(1) {
		t.Fatalf("the capture was disturbed by import: %+v", got)
	}
	if got.Import == nil || got.Import.State != ImportImported || got.Import.RevisionID == "" {
		t.Fatalf("import state = %+v", got.Import)
	}
	rev, err := os.Stat(filepath.Join(vault, "revisions", got.Import.RevisionID, "manifest.json"))
	if err != nil || !rev.Mode().IsRegular() {
		t.Fatalf("revision manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vault, "collections.json")); err != nil {
		t.Fatalf("collection metadata missing: %v", err)
	}

	// A second attempt is a no-op: the revision already exists at the same
	// content address, so no second revision and no second capture appear.
	again, err := store.ImportCapture(entry.ID)
	if err != nil {
		t.Fatalf("repeat import: %v", err)
	}
	if again.Import.RevisionID != got.Import.RevisionID {
		t.Fatalf("repeat import changed revision: %s -> %s", got.Import.RevisionID, again.Import.RevisionID)
	}
	if again.Import.Attempts != got.Import.Attempts {
		t.Fatalf("a no-op import incremented attempts: %d -> %d", got.Import.Attempts, again.Import.Attempts)
	}
}

func TestQueueImportFailureKeepsCaptureAndRetriesOnlyImport(t *testing.T) {
	store := newTestStore(t)
	vault := t.TempDir()
	files := map[string][]byte{"fixture/a.bin": []byte("correct payload")}
	entry := writeCommittedCapture(t, store, testJobID(2), files)
	withImportTarget(t, store, entry.ID, vault)

	payload := filepath.Join(store.Root(), filepath.FromSlash(entry.Workspace), "fixture", "a.bin")
	if err := os.WriteFile(payload, []byte("tampered payload"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := store.ImportCapture(entry.ID); err == nil {
		t.Fatal("import of tampered bytes succeeded")
	}
	failed, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Phase != PhaseCompleted || failed.Capture == nil {
		t.Fatalf("a failed import changed the capture: phase=%q capture=%+v", failed.Phase, failed.Capture)
	}
	if failed.Import == nil || failed.Import.State != ImportFailed || failed.Import.LastError == "" {
		t.Fatalf("import failure not recorded: %+v", failed.Import)
	}
	if failed.Import.RevisionID != "" {
		t.Fatalf("failed import recorded a revision: %q", failed.Import.RevisionID)
	}

	// Repair the bytes and retry. This proves the retry path is the import, not a
	// transfer: no backend exists in this test at all.
	if err := os.WriteFile(payload, files["fixture/a.bin"], 0o644); err != nil {
		t.Fatalf("restore: %v", err)
	}
	done, err := store.ImportCapture(entry.ID)
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	if done.Phase != PhaseCompleted || done.Capture == nil || done.Capture.Key != "cap-"+testJobID(2) {
		t.Fatalf("retry changed the capture: %+v", done)
	}
	if done.Import.State != ImportImported || done.Import.RevisionID == "" {
		t.Fatalf("retry import state = %+v", done.Import)
	}
	if done.Import.Attempts < 2 {
		t.Fatalf("retry did not count an attempt: %+v", done.Import)
	}
}

func TestQueueRunnerImportsPendingCaptureWithoutTransfer(t *testing.T) {
	store := newTestStore(t)
	vault := t.TempDir()
	entry := writeCommittedCapture(t, store, testJobID(3), map[string][]byte{"fixture/a.bin": []byte("already captured")})
	withImportTarget(t, store, entry.ID, vault)

	fake := newFakeTransfer()
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	sum, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if fake.runCount() != 0 {
		t.Fatalf("a committed capture with a pending import started %d transfers", fake.runCount())
	}
	if sum.Imported != 1 || sum.ImportFailed != 0 || len(sum.Ran) != 0 {
		t.Fatalf("summary = %+v, want one import and no download", sum)
	}
	got, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Import == nil || got.Import.State != ImportImported || got.Import.RevisionID == "" {
		t.Fatalf("import not reconciled: %+v", got.Import)
	}
}

func TestQueueRunnerImportsAfterCaptureAndKeepsCaptureOnImportFailure(t *testing.T) {
	store := newTestStore(t)
	vault := t.TempDir()
	entry := addEntry(t, store, testJobID(4))
	withImportTarget(t, store, entry.ID, vault)
	if _, err := store.Update(entry.ID, func(e *Entry) error { e.Desired = DesiredRun; return nil }); err != nil {
		t.Fatal(err)
	}

	fake := newFakeTransfer()
	fake.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		out := successOutcome(req)
		ws := filepath.Join(req.Root, filepath.FromSlash(out.Workspace))
		if err := os.MkdirAll(filepath.Join(ws, "fixture"), 0o755); err != nil {
			return Outcome{}, err
		}
		// The manifest deliberately names the wrong digest, so the capture
		// commits but the vault handoff fails.
		payload := []byte("captured bytes")
		if err := os.WriteFile(filepath.Join(ws, "fixture", "a.bin"), payload, 0o644); err != nil {
			return Outcome{}, err
		}
		cm := &manifest.CaptureManifest{
			Format:       manifest.CaptureFormatVersion,
			SourceURL:    "magnet:?xt=urn:btih:fixture",
			Site:         "bittorrent",
			DownloadedAt: time.Now().UTC(),
			OutputDir:    ws,
			Tool:         "provenance",
			ToolVersion:  "test",
			CaptureKey:   out.CaptureKey,
			Items: []manifest.CaptureItem{{
				ExternalID:     "fixture:a.bin",
				URL:            "magnet:?xt=urn:btih:fixture",
				Kind:           "binary",
				Extractor:      "bittorrent",
				DownloadedPath: "fixture/a.bin",
				ByteSize:       int64(len(payload)),
				Sha256:         strings.Repeat("0", 64),
				CapturedAt:     time.Now().UTC(),
				Status:         "ok",
			}},
		}
		runPath := manifest.RunPath(ws)
		if err := manifest.WriteCaptureManifest(ws, runPath, cm); err != nil {
			return Outcome{}, err
		}
		rel, rerr := filepath.Rel(ws, runPath)
		if rerr != nil {
			return Outcome{}, rerr
		}
		out.ManifestPath = filepath.ToSlash(rel)
		return out, nil
	}
	runner := NewRunner(store, fake, Options{PollInterval: 2 * time.Millisecond})
	sum, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Completed != 1 || sum.ImportFailed != 1 {
		t.Fatalf("summary = %+v, want one completed capture and one failed import", sum)
	}
	got, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseCompleted || got.Capture == nil || got.Capture.Key != "cap-"+testJobID(4) {
		t.Fatalf("a failed import changed the capture: %+v", got)
	}
	if got.Import == nil || got.Import.State != ImportFailed {
		t.Fatalf("import failure not recorded: %+v", got.Import)
	}

	// Fix the manifest bytes and retry only the import. The transfer must not run.
	ws := filepath.Join(store.Root(), filepath.FromSlash(got.Workspace))
	payload := []byte("captured bytes")
	cmPath := filepath.Join(ws, filepath.FromSlash(got.Capture.Manifest))
	cm, err := manifest.ReadCaptureManifest(cmPath)
	if err != nil {
		t.Fatal(err)
	}
	cm.Items[0].Sha256 = sha256Hex(payload)
	if err := manifest.WriteCaptureManifest(ws, cmPath, cm); err != nil {
		t.Fatal(err)
	}
	before := fake.runCount()
	done, err := store.ImportCapture(entry.ID)
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	if fake.runCount() != before {
		t.Fatal("retrying an import ran the transfer again")
	}
	if done.Phase != PhaseCompleted || done.Import.State != ImportImported {
		t.Fatalf("retry result = %+v", done)
	}
}

func TestQueueImportRejectsSymlinkEscape(t *testing.T) {
	store := newTestStore(t)
	vault := t.TempDir()
	entry := writeCommittedCapture(t, store, testJobID(7), map[string][]byte{"fixture/a.bin": []byte("outside bytes")})
	withImportTarget(t, store, entry.ID, vault)

	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(store.Root(), filepath.FromSlash(entry.Workspace), "fixture", "a.bin")
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, payload); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.ImportCapture(entry.ID); err == nil {
		t.Fatal("import followed a symlink out of the workspace")
	}
	got, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseCompleted || got.Capture == nil {
		t.Fatalf("a rejected import changed the capture: %+v", got)
	}
	if got.Import == nil || got.Import.State != ImportFailed {
		t.Fatalf("import failure not recorded: %+v", got.Import)
	}
	if entries, err := os.ReadDir(filepath.Join(vault, "revisions")); err == nil && len(entries) != 0 {
		t.Fatalf("a refused import wrote %d revisions", len(entries))
	}
}

func TestQueueImportTargetConflictAndAcquisition(t *testing.T) {
	store := newTestStore(t)
	job := testJobID(5)
	first, _, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(job, "bt-")},
		RawSource: "magnet:?xt=urn:btih:" + strings.TrimPrefix(job, "bt-"),
		Discovery: "direct",
		Selection: []string{"fixture/a.bin"},
		JobID:     job,
		InfoHash:  strings.TrimPrefix(job, "bt-"),
		Workspace: ".provenance/torrent/jobs/" + job,
		Import:    &ImportTarget{VaultRoot: t.TempDir(), Collection: "one"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if first.Import == nil || first.Import.Target.Collection != "one" {
		t.Fatalf("first import target = %+v", first.Import)
	}
	if _, _, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(job, "bt-")},
		RawSource: "magnet:?xt=urn:btih:" + strings.TrimPrefix(job, "bt-"),
		Discovery: "direct",
		Selection: []string{"fixture/a.bin"},
		JobID:     job,
		InfoHash:  strings.TrimPrefix(job, "bt-"),
		Workspace: ".provenance/torrent/jobs/" + job,
		Import:    &ImportTarget{VaultRoot: t.TempDir(), Collection: "two"},
	}); !errors.Is(err, ErrImportTargetConflict) {
		t.Fatalf("different target = %v, want ErrImportTargetConflict", err)
	}

	// A duplicate without a target can acquire one later without changing the
	// capture intent.
	other := testJobID(6)
	plain, _, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(other, "bt-")},
		RawSource: "magnet:?xt=urn:btih:" + strings.TrimPrefix(other, "bt-"),
		Discovery: "direct",
		Selection: []string{"fixture/b.bin"},
		JobID:     other,
		InfoHash:  strings.TrimPrefix(other, "bt-"),
		Workspace: ".provenance/torrent/jobs/" + other,
	})
	if err != nil {
		t.Fatalf("add plain: %v", err)
	}
	if plain.Import != nil {
		t.Fatalf("plain entry had an import target: %+v", plain.Import)
	}
	vault := t.TempDir()
	got, dup, err := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.TrimPrefix(other, "bt-")},
		RawSource: "magnet:?xt=urn:btih:" + strings.TrimPrefix(other, "bt-"),
		Discovery: "direct",
		Selection: []string{"fixture/b.bin"},
		JobID:     other,
		InfoHash:  strings.TrimPrefix(other, "bt-"),
		Workspace: ".provenance/torrent/jobs/" + other,
		Import:    &ImportTarget{VaultRoot: vault, Collection: "late"},
	})
	if err != nil || !dup {
		t.Fatalf("duplicate with target = dup %v err %v", dup, err)
	}
	if got.ID != plain.ID {
		t.Fatalf("duplicate returned %s, want %s", got.ID, plain.ID)
	}
	if got.Import == nil || got.Import.State != ImportPending {
		t.Fatalf("late target not recorded: %+v", got.Import)
	}
}
