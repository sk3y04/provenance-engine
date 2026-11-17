package blobstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPutGetExists(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	tmp := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmp, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	hash, err := s.Put(tmp)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}

	if !s.Exists(hash) {
		t.Fatal("expected blob to exist")
	}

	f, err := s.Get(hash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer f.Close()

	data := make([]byte, 11)
	n, err := f.Read(data)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data[:n]) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data[:n]))
	}

	blobPath := s.blobPath(hash)
	if _, err := os.Stat(blobPath); os.IsNotExist(err) {
		t.Errorf("expected blob file at %s", blobPath)
	}
}

func TestPutDeduplication(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	tmp := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmp, []byte("dedup content"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	hash1, err := s.Put(tmp)
	if err != nil {
		t.Fatalf("first put: %v", err)
	}

	hash2, err := s.Put(tmp)
	if err != ErrExists {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("expected same hash, got %q vs %q", hash1, hash2)
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	tmp := filepath.Join(t.TempDir(), "test.txt")
	_ = os.WriteFile(tmp, []byte("remove me"), 0o644)

	hash, _ := s.Put(tmp)
	if !s.Exists(hash) {
		t.Fatal("expected blob to exist before remove")
	}

	if err := s.Remove(hash); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if s.Exists(hash) {
		t.Fatal("expected blob to not exist after remove")
	}
}

// TestPutConcurrentSameContent proves parallel Puts of identical content never
// corrupt the blob or each other's in-progress temp files, and leave no temp
// files behind.
func TestPutConcurrentSameContent(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	content := []byte("concurrent dedup content")
	srcs := make([]string, 8)
	for i := range srcs {
		p := filepath.Join(t.TempDir(), "src.txt")
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		srcs[i] = p
	}

	hashes := make([]string, len(srcs))
	errs := make([]error, len(srcs))
	done := make(chan struct{}, len(srcs))
	for i, src := range srcs {
		go func(i int, src string) {
			defer func() { done <- struct{}{} }()
			h, err := s.Put(src)
			hashes[i], errs[i] = h, err
		}(i, src)
	}
	for range srcs {
		<-done
	}

	want := hashes[0]
	for i, err := range errs {
		if err != nil && err != ErrExists {
			t.Fatalf("put %d: %v", i, err)
		}
		if hashes[i] != want {
			t.Fatalf("put %d hash = %q, want %q", i, hashes[i], want)
		}
	}
	f, err := s.Get(want)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer f.Close()
	got, err := os.ReadFile(f.Name())
	if err != nil || string(got) != string(content) {
		t.Fatalf("blob content corrupted: %q, err %v", got, err)
	}

	// No temp files may survive the concurrent puts.
	leftovers, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", "*", "*.tmp-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v (err %v)", leftovers, err)
	}
}

// TestListSkipsNonBlobEntries proves List (and therefore GC) ignores temp
// files and stray entries instead of treating them as orphan blobs.
func TestListSkipsNonBlobEntries(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	tmp := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(tmp, []byte("list me"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := s.Put(tmp)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(s.blobPath(hash))
	for _, junk := range []string{hash + ".tmp-12345", "not-a-hash", hash + ".bak"} {
		if err := os.WriteFile(filepath.Join(dir, junk), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != hash {
		t.Fatalf("List = %v, want only [%s]", list, hash)
	}
}
