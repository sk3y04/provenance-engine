package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

// putBlob stores content in the vault's blob store and returns its hash.
func putBlob(t *testing.T, vaultRoot, content string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := blobstore.New(vaultRoot).Put(src)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// refRevision writes a revision whose artifact list references the given blob
// hashes.
func refRevision(t *testing.T, vaultRoot, id string, hashes ...string) {
	t.Helper()
	arts := make([]archive.Artifact, 0, len(hashes))
	for _, h := range hashes {
		arts = append(arts, archive.Artifact{Sha256: h})
	}
	rev := &archive.Revision{
		ID:         id,
		CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Source:     archive.Source{Kind: archive.SourceURL, Reference: "https://example.test/x"},
		Entities:   []archive.Entity{{ExternalID: id, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		Artifacts:  arts,
	}
	if err := archive.WriteRevision(vaultRoot, rev); err != nil {
		t.Fatal(err)
	}
}

// TestGarbageCollectDeletesOnlyOrphans: the core vault invariant - a blob
// referenced by any revision must survive GC; unreferenced blobs go.
func TestGarbageCollectDeletesOnlyOrphans(t *testing.T) {
	vault := t.TempDir()
	refA, refB, orphan := putBlob(t, vault, "a"), putBlob(t, vault, "b"), putBlob(t, vault, "c")
	refRevision(t, vault, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", refA)
	refRevision(t, vault, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", refB)

	removed, err := GarbageCollect(vault, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(idSet(removed), map[string]bool{orphan: true}) {
		t.Fatalf("removed = %v, want exactly the orphan %s", removed, orphan)
	}
	bs := blobstore.New(vault)
	if !bs.Exists(refA) || !bs.Exists(refB) {
		t.Fatal("GC deleted a referenced blob")
	}
	if bs.Exists(orphan) {
		t.Fatal("orphan blob survived GC")
	}
}

// TestGarbageCollectDryRunReportsButRemovesNothing.
func TestGarbageCollectDryRunReportsButRemovesNothing(t *testing.T) {
	vault := t.TempDir()
	refA, orphan := putBlob(t, vault, "a"), putBlob(t, vault, "b")
	refRevision(t, vault, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", refA)

	removed, err := GarbageCollect(vault, true)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(idSet(removed), map[string]bool{orphan: true}) {
		t.Fatalf("dry-run removed = %v, want %s", removed, orphan)
	}
	bs := blobstore.New(vault)
	if !bs.Exists(refA) || !bs.Exists(orphan) {
		t.Fatal("dry-run must not delete anything")
	}
}

// TestGarbageCollectEmptyVaultTreatsAllBlobsAsOrphans: with no revisions at
// all, every blob is unreferenced.
func TestGarbageCollectEmptyVaultTreatsAllBlobsAsOrphans(t *testing.T) {
	vault := t.TempDir()
	only := putBlob(t, vault, "orphan")

	removed, err := GarbageCollect(vault, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(idSet(removed), map[string]bool{only: true}) {
		t.Fatalf("removed = %v, want %s", removed, only)
	}
	if blobstore.New(vault).Exists(only) {
		t.Fatal("blob survived GC in a revision-less vault")
	}
}
