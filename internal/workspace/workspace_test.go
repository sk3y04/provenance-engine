package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreatedReportsOnlyFilesCreatedByTheOperation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "earlier.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bookkeeping and partial files are never artifacts.
	if err := os.MkdirAll(filepath.Join(w.Root, "_provenance_cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, "_provenance_cache", "archive.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, "partial.mp4.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := w.Created()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "new.txt" {
		t.Fatalf("created = %+v, want only new.txt", got)
	}
}

// TestConcurrentOperationsDoNotReportEachOthersFiles is the race gate for a
// long-lived agent or worker sharing a parent directory.
func TestConcurrentOperationsDoNotReportEachOthersFiles(t *testing.T) {
	root := t.TempDir()
	a, err := New(root, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Root == b.Root {
		t.Fatal("isolated workspaces must not share a root")
	}
	if err := os.WriteFile(filepath.Join(a.Root, "a.bin"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Root, "b.bin"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	ga, err := a.Created()
	if err != nil {
		t.Fatal(err)
	}
	gb, err := b.Created()
	if err != nil {
		t.Fatal(err)
	}
	if len(ga) != 1 || ga[0].Name != "a.bin" {
		t.Fatalf("workspace A reported %+v", ga)
	}
	if len(gb) != 1 || gb[0].Name != "b.bin" {
		t.Fatalf("workspace B reported %+v", gb)
	}
}
