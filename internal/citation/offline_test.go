package citation

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("copy vault: %v", err)
	}
}

// TestRestoredVaultResolvesCitationOffline proves a copied vault can resolve a
// citation to its archived bytes without the source file, network or database.
func TestRestoredVaultResolvesCitationOffline(t *testing.T) {
	vault := t.TempDir()
	content := []byte("archived pdf bytes")
	src := filepath.Join(t.TempDir(), "paper.pdf")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := blobstore.New(vault).Put(src)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("revision-identity"))
	revID := hex.EncodeToString(sum[:])
	rev := &archive.Revision{
		ID:          revID,
		CapturedAt:  time.Now().UTC(),
		Source:      archive.Source{Kind: archive.SourceImport, Reference: "paper"},
		Tool:        "test",
		ToolVersion: "test",
		Artifacts: []archive.Artifact{{
			Sha256: hash,
			Path:   "paper.pdf",
			Size:   int64(len(content)),
			Kind:   archive.ArtifactBinary,
		}},
		Entities: []archive.Entity{{
			ExternalID: "paper-1",
			Title:      "Paper",
			CapturedAt: time.Now().UTC(),
			Artifacts:  []string{hash},
		}},
	}
	if err := archive.WriteRevision(vault, rev); err != nil {
		t.Fatalf("write revision: %v", err)
	}

	restored := t.TempDir()
	copyTree(t, vault, restored)
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}

	got, err := archive.ReadRevision(restored, revID)
	if err != nil {
		t.Fatalf("read restored revision: %v", err)
	}
	if len(got.Entities) != 1 {
		t.Fatalf("entities = %d", len(got.Entities))
	}
	ref := FromEntity("papers", revID, &got.Entities[0])
	parsed, err := Parse(ref.String())
	if err != nil {
		t.Fatalf("parse citation: %v", err)
	}
	if parsed.RevisionID != revID || parsed.EntityID != "paper-1" {
		t.Fatalf("parsed citation = %+v", parsed)
	}
	f, err := blobstore.New(restored).Get(ref.Sha256)
	if err != nil {
		t.Fatalf("resolve archived bytes: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != string(content) {
		t.Fatalf("archived bytes = %q err=%v", data, err)
	}
}
