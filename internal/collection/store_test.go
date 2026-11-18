package collection

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/config"
)

func TestCollectionLifecycle(t *testing.T) {
	file := filepath.Join(t.TempDir(), "collections.json")
	t.Setenv("PROVENANCE_COLLECTION_FILE", file)

	if err := Add("test-col", "https://reddit.com/r/wallpapers", "reddit", config.Config{
		OutputDir: "./downloads",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	all, err := List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(all))
	}
	if all[0].Name != "test-col" {
		t.Errorf("expected test-col, got %q", all[0].Name)
	}
	if all[0].URL != "https://reddit.com/r/wallpapers" {
		t.Errorf("unexpected URL: %q", all[0].URL)
	}

	c, err := Get("test-col")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.Site != "reddit" {
		t.Errorf("expected site reddit, got %q", c.Site)
	}

	if len(c.SeenIDs) != 0 {
		t.Errorf("expected empty seen IDs, got %d", len(c.SeenIDs))
	}

	if err := AddSeen("test-col", "abc123", "def456"); err != nil {
		t.Fatalf("add seen: %v", err)
	}

	c, err = Get("test-col")
	if err != nil {
		t.Fatalf("get after add seen: %v", err)
	}
	if len(c.SeenIDs) != 2 {
		t.Errorf("expected 2 seen IDs, got %d", len(c.SeenIDs))
	}
	if !c.SeenIDs["abc123"] {
		t.Error("expected abc123 in seen IDs")
	}

	if err := RecordSync("test-col", SyncResult{
		Total:   100,
		New:     50,
		Skipped: 50,
		Failed:  0,
	}); err != nil {
		t.Fatalf("record sync: %v", err)
	}

	c, err = Get("test-col")
	if err != nil {
		t.Fatalf("get after sync: %v", err)
	}
	if c.LastSync.IsZero() {
		t.Error("expected non-zero LastSync")
	}
	if c.LastResult == nil {
		t.Fatal("expected non-nil LastResult")
	}
	if c.LastResult.Total != 100 {
		t.Errorf("expected total 100, got %d", c.LastResult.Total)
	}

	if err := Remove("test-col"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	all, err = List()
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 collections, got %d", len(all))
	}
}

func TestCollectionInvalidNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "collections.json")
	t.Setenv("PROVENANCE_COLLECTION_FILE", file)

	for _, name := range []string{"", ".", "..", "path/name", "bad\\name"} {
		if err := Add(name, "https://example.com", "generic", config.Config{}); err == nil {
			t.Errorf("expected error for name %q", name)
		}
	}
}

// TestCrossProcessAddSeenNoLostUpdates spawns real child processes that each
// record seen IDs into the same collection store. Without cross-process
// locking the load-modify-save cycles overwrite each other and IDs vanish.
func TestCrossProcessAddSeenNoLostUpdates(t *testing.T) {
	if os.Getenv("PROVENANCE_TEST_HELPER") == "1" {
		// Child: record 20 unique IDs, then exit.
		for i := 0; i < 20; i++ {
			if err := AddSeen("shared", fmt.Sprintf("id-%s-%d", os.Getenv("PROVENANCE_TEST_CHILD"), i)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}

	file := filepath.Join(t.TempDir(), "collections.json")
	t.Setenv("PROVENANCE_COLLECTION_FILE", file)
	if err := Add("shared", "https://example.com/feed", "generic", config.Config{}); err != nil {
		t.Fatal(err)
	}

	const children = 4
	var wg sync.WaitGroup
	errs := make(chan error, children)
	for c := 0; c < children; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessAddSeenNoLostUpdates$")
			cmd.Env = append(os.Environ(),
				"PROVENANCE_TEST_HELPER=1",
				fmt.Sprintf("PROVENANCE_TEST_CHILD=%d", c),
				"PROVENANCE_COLLECTION_FILE="+file,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("child %d: %v\n%s", c, err, out)
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	col, err := Get("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(col.SeenIDs) != children*20 {
		t.Fatalf("SeenIDs = %d, want %d (lost updates across processes)", len(col.SeenIDs), children*20)
	}
}

func TestCollectionUpsertPreservesSeenIDs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "collections.json")
	t.Setenv("PROVENANCE_COLLECTION_FILE", file)

	cfg := config.Config{OutputDir: "downloads", Concurrency: 4}
	if err := Add("upsert-test", "https://reddit.com/r/test", "reddit", cfg); err != nil {
		t.Fatalf("initial add: %v", err)
	}

	if err := AddSeen("upsert-test", "id-1", "id-2", "id-3"); err != nil {
		t.Fatalf("add seen: %v", err)
	}

	cfg2 := config.Config{OutputDir: "other-dir", Concurrency: 8}
	if err := Add("upsert-test", "https://reddit.com/r/test", "reddit", cfg2); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	c, err := Get("upsert-test")
	if err != nil {
		t.Fatalf("get after upsert: %v", err)
	}
	if len(c.SeenIDs) != 3 {
		t.Errorf("expected 3 seen IDs after upsert, got %d", len(c.SeenIDs))
	}
	if c.Options.Concurrency != 8 {
		t.Errorf("expected concurrency 8 after upsert, got %d", c.Options.Concurrency)
	}
}
