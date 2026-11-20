package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

func writeBlob(t *testing.T, root string, data []byte) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := blobstore.New(root).Put(src)
	if err != nil && err != blobstore.ErrExists {
		t.Fatalf("put blob: %v", err)
	}
	return hash
}

func writeRevision(t *testing.T, root string, artifacts []Artifact, entityArtifacts [][]string) *Revision {
	t.Helper()
	rev := &Revision{
		CapturedAt:  time.Now().UTC(),
		Source:      Source{Kind: SourceImport, Reference: "test"},
		Tool:        "test",
		ToolVersion: "test",
		Artifacts:   artifacts,
		Entities:    make([]Entity, 0, len(entityArtifacts)),
		Documents:   []Document{},
		Relations:   []Relation{},
	}
	for i, shas := range entityArtifacts {
		rev.Entities = append(rev.Entities, Entity{
			ExternalID: "entity-" + string(rune('a'+i)),
			CapturedAt: time.Now().UTC(),
			Artifacts:  shas,
		})
	}
	id, err := computeRevisionID(rev)
	if err != nil {
		t.Fatal(err)
	}
	rev.ID = id
	if err := WriteRevision(root, rev); err != nil {
		t.Fatalf("write revision: %v", err)
	}
	return rev
}

func TestVerifyVaultCleanSharedBlob(t *testing.T) {
	root := t.TempDir()
	data := []byte("shared blob")
	shared := writeBlob(t, root, data)
	writeRevision(t, root, []Artifact{{Sha256: shared, Path: "a.bin", Size: int64(len(data)), Kind: ArtifactBinary}}, nil)
	writeRevision(t, root, []Artifact{{Sha256: shared, Path: "copy.bin", Size: int64(len(data)), Kind: ArtifactBinary}}, nil)

	report, err := VerifyVault(root)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.Healthy {
		t.Fatalf("clean vault reported unhealthy: %+v", report)
	}
	if report.Revisions != 2 || report.UniqueBlobs != 1 || report.CheckedBlobs != 1 || report.OK != 1 {
		t.Fatalf("counts = %+v", report)
	}
	if len(report.Blobs) != 1 || report.Blobs[0].References != 2 || report.Blobs[0].Status != VerifyOK {
		t.Fatalf("blob result = %+v", report.Blobs)
	}
}

func TestVerifyVaultReportsMissingMismatchAndUnreadableManifest(t *testing.T) {
	root := t.TempDir()
	good := writeBlob(t, root, []byte("good"))
	corrupt := writeBlob(t, root, []byte("before"))
	missingSum := sha256.Sum256([]byte("never stored"))
	missing := hex.EncodeToString(missingSum[:])

	// Corrupt the bytes without changing the address referenced by the manifest.
	f, err := blobstore.New(root).Get(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	corruptPath := f.Name()
	_ = f.Close()
	if err := os.WriteFile(corruptPath, []byte("after!"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeRevision(t, root, []Artifact{
		{Sha256: good, Path: "good.bin", Size: 4, Kind: ArtifactBinary},
		{Sha256: corrupt, Path: "bad.bin", Size: 6, Kind: ArtifactBinary},
		{Sha256: missing, Path: "gone.bin", Size: 12, Kind: ArtifactBinary},
	}, nil)

	badID := hex.EncodeToString(sha256.New().Sum(nil))
	badDir := filepath.Join(root, "revisions", badID)
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := VerifyVault(root)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Healthy {
		t.Fatal("damaged vault reported healthy")
	}
	if report.Revisions != 1 || report.UniqueBlobs != 3 {
		t.Fatalf("counts = revisions %d blobs %d", report.Revisions, report.UniqueBlobs)
	}
	if report.OK != 1 || report.Mismatch != 1 || report.Missing != 1 || report.Unreadable != 1 {
		t.Fatalf("outcome counts = %+v", report)
	}
	status := map[string]VerifyStatus{}
	for _, b := range report.Blobs {
		status[b.Sha256] = b.Status
	}
	if status[good] != VerifyOK || status[corrupt] != VerifyMismatch || status[missing] != VerifyMissing {
		t.Fatalf("blob statuses = %v", status)
	}
	if len(report.Manifests) != 1 || report.Manifests[0].Status != VerifyUnreadable {
		t.Fatalf("manifest findings = %+v", report.Manifests)
	}

	// Verification is read-only: the corrupt and missing blobs are unchanged.
	after, err := os.ReadFile(corruptPath)
	if err != nil || string(after) != "after!" {
		t.Fatalf("verify rewrote a corrupt blob: %q err=%v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", "sha256", missing[:2], missing)); !os.IsNotExist(err) {
		t.Fatalf("verify created a missing blob: %v", err)
	}
}

func TestVerifyVaultEmptyIsHealthy(t *testing.T) {
	report, err := VerifyVault(t.TempDir())
	if err != nil {
		t.Fatalf("verify empty: %v", err)
	}
	if !report.Healthy || report.Revisions != 0 || report.UniqueBlobs != 0 {
		t.Fatalf("empty report = %+v", report)
	}
}
