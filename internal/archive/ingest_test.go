package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// writeCaptureFixture builds an output dir containing the given files
// (rel path -> content) and a capture manifest whose items hash the real
// file contents.
func writeCaptureFixture(t *testing.T, files map[string]string) (*manifest.CaptureManifest, string) {
	t.Helper()
	outDir := t.TempDir()
	var items []manifest.CaptureItem
	for rel, content := range files {
		p := filepath.Join(outDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		items = append(items, manifest.CaptureItem{
			ExternalID:     rel,
			URL:            "https://example.test/" + rel,
			DownloadedPath: rel,
			ByteSize:       int64(len(content)),
			Sha256:         hex.EncodeToString(sum[:]),
			CapturedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Kind:           "image",
			Extractor:      "fixture",
		})
	}
	return &manifest.CaptureManifest{
		Format:    manifest.CaptureFormatVersion,
		SourceURL: "https://example.test/collection",
		OutputDir: outDir,
		Tool:      "provenance",
		Items:     items,
	}, outDir
}

func ingestOpts(t *testing.T) IngestOptions {
	t.Helper()
	return IngestOptions{
		VaultRoot:      t.TempDir(),
		CollectionName: "test",
		Source:         Source{Kind: SourceCollection, Reference: "test"},
		Tool:           "provenance",
	}
}

// TestIngestRejectsPathTraversal: a manifest item pointing outside the
// capture's output dir must be refused, so the vault never reads or hashes
// files the capture did not own.
func TestIngestRejectsPathTraversal(t *testing.T) {
	outDir := t.TempDir()
	evil := filepath.Join(t.TempDir(), "evil.bin")
	if err := os.WriteFile(evil, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("outside"))

	cm := &manifest.CaptureManifest{
		Format:    manifest.CaptureFormatVersion,
		OutputDir: outDir,
		Items: []manifest.CaptureItem{{
			ExternalID:     "evil",
			URL:            "https://example.test/evil",
			DownloadedPath: "../evil.bin",
			Sha256:         hex.EncodeToString(sum[:]),
		}},
	}
	if _, err := IngestCaptureManifest(blobstore.New(t.TempDir()), ingestOpts(t), cm); err == nil {
		t.Fatal("expected error for path escaping the output dir")
	}

	cm.Items[0].DownloadedPath = "/etc/passwd"
	if _, err := IngestCaptureManifest(blobstore.New(t.TempDir()), ingestOpts(t), cm); err == nil {
		t.Fatal("expected error for absolute path")
	}
}

// TestIngestRejectsHashMismatch: the recorded sha256 must match the file on
// disk; a tampered manifest must abort the ingest, not store wrong content.
func TestIngestRejectsHashMismatch(t *testing.T) {
	cm, _ := writeCaptureFixture(t, map[string]string{"a.bin": "v1"})
	cm.Items[0].Sha256 = strings.Repeat("0", 64)

	opts := ingestOpts(t)
	if _, err := IngestCaptureManifest(blobstore.New(opts.VaultRoot), opts, cm); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

// TestIngestSkipsMissingFilesAndDedupesBlobs: an item whose file is gone is
// skipped (not an error), and identical content is stored once even when
// several entities point at it.
func TestIngestSkipsMissingFilesAndDedupesBlobs(t *testing.T) {
	cm, outDir := writeCaptureFixture(t, map[string]string{
		"a.bin": "same content",
		"b.bin": "same content",
	})
	ghostSum := sha256.Sum256([]byte("ghost"))
	cm.Items = append(cm.Items, manifest.CaptureItem{
		ExternalID:     "ghost.bin",
		URL:            "https://example.test/ghost.bin",
		DownloadedPath: "ghost.bin",
		Sha256:         hex.EncodeToString(ghostSum[:]),
	})

	opts := ingestOpts(t)
	bs := blobstore.New(opts.VaultRoot)
	rev, err := IngestCaptureManifest(bs, opts, cm)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(rev.Entities) != 2 {
		t.Fatalf("entities = %d, want 2 (missing file skipped): %+v", len(rev.Entities), rev.Entities)
	}
	if len(rev.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1 (deduped blob): %+v", len(rev.Artifacts), rev.Artifacts)
	}
	if !bs.Exists(rev.Artifacts[0].Sha256) {
		t.Fatal("blob missing from store")
	}
	for _, e := range rev.Entities {
		if e.ExternalID == "ghost.bin" {
			t.Fatal("ghost entity must not be ingested")
		}
	}
	_ = outDir
}

// TestIngestSameContentProducesSameRevisionID: revisions are
// content-addressed, so re-ingesting the same capture (retry, resume) must
// produce the same ID instead of a fresh duplicate revision each time.
func TestIngestSameContentProducesSameRevisionID(t *testing.T) {
	cm1, _ := writeCaptureFixture(t, map[string]string{"a.bin": "hello"})
	cm2, _ := writeCaptureFixture(t, map[string]string{"a.bin": "hello"})

	rev1, err := IngestCaptureManifest(blobstore.New(t.TempDir()), ingestOpts(t), cm1)
	if err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	rev2, err := IngestCaptureManifest(blobstore.New(t.TempDir()), ingestOpts(t), cm2)
	if err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if rev1.ID != rev2.ID {
		t.Fatalf("revision ids differ for identical content: %s vs %s", rev1.ID, rev2.ID)
	}
}

func TestWriteReadRevisionRoundTrip(t *testing.T) {
	cm, _ := writeCaptureFixture(t, map[string]string{"a.bin": "hello"})
	cm.Items[0].Text = &manifest.TextCapture{Body: "body text", Format: "markdown"}

	opts := ingestOpts(t)
	rev, err := IngestCaptureManifest(blobstore.New(opts.VaultRoot), opts, cm)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := WriteRevision(opts.VaultRoot, rev); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadRevision(opts.VaultRoot, rev.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ID != rev.ID || len(got.Entities) != 1 || len(got.Artifacts) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Entities[0].Text == nil || got.Entities[0].Text.Content != "body text" {
		t.Fatalf("text lost in round trip: %+v", got.Entities[0].Text)
	}
}
