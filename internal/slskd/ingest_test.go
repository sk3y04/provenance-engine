package slskd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// stagingRoot builds a mount with a populated destination directory and returns
// the ingestor, the mount and the destination path.
func stagingRoot(t *testing.T, files map[string]string) (*Ingestor, string, string) {
	t.Helper()
	mount := t.TempDir()
	dest := filepath.Join(mount, "provenance", "prov-0123456789abcdef")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dest, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ing := &Ingestor{Mount: mount}
	return ing, mount, dest
}

func completedJob(t *testing.T, dest string, items ...Item) Job {
	t.Helper()
	_ = dest
	return Job{
		ID:                "prov-0123456789abcdef",
		Peer:              "peer1",
		SearchText:        "beethoven",
		SearchID:          "s1",
		BatchID:           "b1",
		DestinationSubdir: "provenance/prov-0123456789abcdef",
		Status:            JobCompletedPendingImport,
		Items:             items,
	}
}

// TestIngestHappyPath proves a completed file becomes a verified capture: the
// job reaches IMPORTED only after the bytes were hashed and a run manifest was
// written, and the staging file is preserved by default.
func TestIngestHappyPath(t *testing.T) {
	ing, mount, dest := stagingRoot(t, map[string]string{"track.flac": "audio-bytes"})
	job := completedJob(t, dest, Item{RemoteFilename: `music\Album\track.flac`, Size: 11})

	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got.Status != JobImported {
		t.Fatalf("status = %s, want IMPORTED (reason %q)", got.Status, got.FailureReason)
	}
	if got.Items[0].Sha256 == "" || got.Items[0].ByteSize != 11 {
		t.Fatalf("item not verified: %+v", got.Items[0])
	}
	if _, err := os.Stat(filepath.Join(dest, "track.flac")); err != nil {
		t.Fatalf("staging file was removed without cleanup enabled: %v", err)
	}
	if got.ImportRoot != mount {
		t.Fatalf("import root = %q", got.ImportRoot)
	}

	// The run manifest must exist, because archive import reads runs/*.json and
	// items alone are invisible to it.
	runs, err := manifest.ListRunManifests(mount)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d run manifests, want 1", len(runs))
	}
	data, err := os.ReadFile(runs[0])
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		CaptureKey string `json:"capture_key"`
		Items      []struct {
			ExternalID     string          `json:"external_id"`
			DownloadedPath string          `json:"downloaded_path"`
			Sha256         string          `json:"sha256"`
			RawMetadata    json.RawMessage `json:"raw_metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &cm); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if cm.CaptureKey != job.ID {
		t.Fatalf("capture_key = %q, want the job id", cm.CaptureKey)
	}
	if len(cm.Items) != 1 {
		t.Fatalf("%d items", len(cm.Items))
	}
	if cm.Items[0].DownloadedPath != "provenance/prov-0123456789abcdef/track.flac" {
		t.Fatalf("downloaded_path = %q (must be relative to the capture root)", cm.Items[0].DownloadedPath)
	}
	if !strings.Contains(string(cm.Items[0].RawMetadata), `"source": "soulseek"`) {
		t.Fatalf("raw metadata lacks the soulseek provenance: %s", cm.Items[0].RawMetadata)
	}
	if !strings.Contains(string(cm.Items[0].RawMetadata), `"peer_username": "peer1"`) {
		t.Fatalf("raw metadata lacks the peer: %s", cm.Items[0].RawMetadata)
	}

	// VerifyCaptureDir must accept what we wrote.
	results, err := manifest.VerifyCaptureDir(mount)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("capture verification failed: %+v", results)
	}
}

// TestIngestTicksRename proves a collision-renamed file is found: slskd appends
// "_{ticks}" before the extension, and a strict basename match would report a
// missing file that is right there.
func TestIngestTicksRename(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"track_638361234567890123.flac": "audio-bytes"})
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 11})
	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got.Status != JobImported {
		t.Fatalf("status = %s (%s)", got.Status, got.FailureReason)
	}
	if !strings.Contains(got.Items[0].LocalPath, "track_638361234567890123.flac") {
		t.Fatalf("local path = %q", got.Items[0].LocalPath)
	}
}

// TestIngestRejectsSymlink keeps a symlink planted in the staging directory from
// reading a file outside the download root.
func TestIngestRejectsSymlink(t *testing.T) {
	ing, mount, dest := stagingRoot(t, map[string]string{})
	outside := filepath.Join(t.TempDir(), "secret.flac")
	if err := os.WriteFile(outside, []byte("secret-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "track.flac")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 12})
	got, err := ing.Ingest(context.Background(), job)
	if err == nil {
		t.Fatal("symlinked file was ingested")
	}
	if got.Status != JobImportFailed {
		t.Fatalf("status = %s", got.Status)
	}
	_ = mount
}

// TestIngestRejectsTraversalDestination proves a job whose destination escapes
// the mount is refused before any walk.
func TestIngestRejectsTraversalDestination(t *testing.T) {
	ing := &Ingestor{Mount: t.TempDir()}
	job := Job{ID: "prov-0123456789abcdef", DestinationSubdir: "../../etc", Status: JobCompletedPendingImport,
		Items: []Item{{RemoteFilename: `x.flac`, Size: 1}}}
	if _, err := ing.Ingest(context.Background(), job); err == nil {
		t.Fatal("traversal destination accepted")
	}
}

// TestIngestSizeMismatchFails keeps a truncated download from being recorded as
// a verified capture.
func TestIngestSizeMismatchFails(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"track.flac": "short"})
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 999})
	got, err := ing.Ingest(context.Background(), job)
	if err == nil {
		t.Fatal("size mismatch accepted")
	}
	if got.Status != JobImportFailed {
		t.Fatalf("status = %s", got.Status)
	}
	if !strings.Contains(got.FailureReason, "999") {
		t.Fatalf("reason = %q", got.FailureReason)
	}
	if _, err := os.Stat(filepath.Join(dest, "track.flac")); err != nil {
		t.Fatal("staging file was removed on a failed import")
	}
}

// TestIngestEmptyFileFails proves a zero-byte file (the #1818 retry-loop symptom)
// is not imported as if it were content.
func TestIngestEmptyFileFails(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"track.flac": ""})
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 0})
	if _, err := ing.Ingest(context.Background(), job); err == nil {
		t.Fatal("empty file imported")
	}
}

// TestIngestMissingFileFails covers a job marked complete with no bytes on disk:
// it must not report IMPORTED.
func TestIngestMissingFileFails(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"other.flac": "x"})
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 1})
	got, err := ing.Ingest(context.Background(), job)
	if err == nil {
		t.Fatal("missing file reported as imported")
	}
	if got.Status != JobImportFailed {
		t.Fatalf("status = %s", got.Status)
	}
}

// TestIngestCleanupOnlyAfterSuccess proves the staging copy is deleted only when
// the import verified, and that the job records the cleanup.
func TestIngestCleanupOnlyAfterSuccess(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"track.flac": "audio-bytes"})
	ing.Cleanup = true
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 11})
	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !got.CleanedUp {
		t.Fatal("cleanup not recorded")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("staging directory still exists: %v", err)
	}

	// A failed import keeps everything.
	ing2, _, dest2 := stagingRoot(t, map[string]string{"track.flac": "short"})
	ing2.Cleanup = true
	job2 := completedJob(t, dest2, Item{RemoteFilename: `music\track.flac`, Size: 999})
	if _, err := ing2.Ingest(context.Background(), job2); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := os.Stat(filepath.Join(dest2, "track.flac")); err != nil {
		t.Fatal("staging file was removed on a failed import")
	}
}

// TestIngestIntoVault proves the optional vault step produces a revision and
// records its id on the job.
func TestIngestIntoVault(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"track.flac": "audio-bytes"})
	ing.VaultRoot = t.TempDir()
	ing.Collection = "soulseek"
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 11})
	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got.RevisionID == "" {
		t.Fatal("no revision id recorded")
	}
	if _, err := os.Stat(filepath.Join(ing.VaultRoot, "revisions", got.RevisionID, "manifest.json")); err != nil {
		t.Fatalf("revision not written: %v", err)
	}
}

// TestIngestRecordsThroughStore proves the ingestor updates the durable job
// rather than a copy held in memory.
func TestIngestRecordsThroughStore(t *testing.T) {
	ing, mount, dest := stagingRoot(t, map[string]string{"track.flac": "audio-bytes"})
	store := mustOpenStore(t, t.TempDir())
	ing.AttachStore(store)
	job := completedJob(t, dest, Item{RemoteFilename: `music\track.flac`, Size: 11})
	if _, err := store.Create(job); err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Ingest(context.Background(), job); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	stored, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != JobImported {
		t.Fatalf("stored status = %s", stored.Status)
	}
	if stored.ImportRoot != mount {
		t.Fatalf("stored import root = %q", stored.ImportRoot)
	}
	if pending, _ := store.Pending(); len(pending) != 0 {
		t.Fatal("imported job still pending")
	}
}
