package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestResolveLocatorPinsArchivedBytes(t *testing.T) {
	root := t.TempDir()
	data := []byte("page bytes")
	sha := writeBlob(t, root, data)
	rev := writeRevision(t, root, []Artifact{{Sha256: sha, Path: "a.pdf", Size: int64(len(data)), Kind: ArtifactBinary}}, nil)

	f, err := ResolveLocator(root, rev.ID, Locator{ArtifactSha256: sha, PageIndex: 0})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer f.Close()
	got := make([]byte, len(data))
	if _, err := f.Read(got); err != nil || string(got) != string(data) {
		t.Fatalf("bytes = %q err=%v", got, err)
	}

	if _, err := ResolveLocator(root, rev.ID, Locator{ArtifactSha256: sha, PageIndex: -1}); err == nil {
		t.Fatal("negative page index accepted")
	}
	other := sha256.Sum256([]byte("other"))
	if _, err := ResolveLocator(root, rev.ID, Locator{ArtifactSha256: hex.EncodeToString(other[:]), PageIndex: 0}); err == nil {
		t.Fatal("artifact outside the revision accepted")
	}
	if _, err := ResolveLocator(root, "not-a-revision", Locator{ArtifactSha256: sha, PageIndex: 0}); err == nil {
		t.Fatal("invalid revision id accepted")
	}
}
