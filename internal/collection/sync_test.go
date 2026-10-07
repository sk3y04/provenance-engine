package collection

import (
	"path/filepath"
	"testing"

	"github.com/sk3y04/provenance/internal/config"
	"github.com/sk3y04/provenance/internal/manifest"
)

func TestPartitionNewItemsSkipsSeenHashtagPosts(t *testing.T) {
	items := []manifest.Item{
		{ID: "111_0", URL: "https://pbs.twimg.com/media/a.jpg", PostID: "111", Kind: "photo"},
		{ID: "111_1", URL: "https://pbs.twimg.com/media/b.jpg", PostID: "111", Kind: "photo"},
		{ID: "111_post", URL: "post://twitter/111", PostID: "111", Kind: "post"},
		{ID: "222_0", URL: "https://pbs.twimg.com/media/c.jpg", PostID: "222", Kind: "photo"},
	}
	seen := map[string]bool{"111": true}

	newURLs, newIDs, urlToItem, total, skipped := partitionNewItems(items, seen)

	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3 (all items of post 111)", skipped)
	}
	if len(newURLs) != 1 || newURLs[0] != "https://pbs.twimg.com/media/c.jpg" {
		t.Errorf("newURLs = %v, want only post 222's photo", newURLs)
	}
	if len(newIDs) != 1 || newIDs[0] != "222" {
		t.Errorf("newIDs = %v, want [222]", newIDs)
	}
	if _, ok := urlToItem["https://pbs.twimg.com/media/c.jpg"]; !ok {
		t.Error("urlToItem missing the new item")
	}
}

func TestCollectionStoresCanonicalHashtagSource(t *testing.T) {
	file := filepath.Join(t.TempDir(), "collections.json")
	t.Setenv("PROVENANCE_COLLECTION_FILE", file)

	if err := Add("golang", "x:#golang", "twitter", config.Config{OutputDir: "./downloads"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	c, err := Get("golang")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.URL != "x:#golang" {
		t.Errorf("collection URL = %q, want x:#golang", c.URL)
	}
	if c.Site != "twitter" {
		t.Errorf("collection site = %q, want twitter", c.Site)
	}
}
