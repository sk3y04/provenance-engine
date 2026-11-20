package collection

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/version"
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

func TestFullySucceededIDsRequiresAllPostURLs(t *testing.T) {
	urlToItem := map[string]manifest.Item{
		"https://cdn/a1.jpg": {URL: "https://cdn/a1.jpg", PostID: "A"},
		"https://cdn/a2.jpg": {URL: "https://cdn/a2.jpg", PostID: "A"},
		"https://cdn/b1.jpg": {URL: "https://cdn/b1.jpg", PostID: "B"},
		"https://cdn/c1.jpg": {URL: "https://cdn/c1.jpg"}, // identity falls back to URL
		"https://cdn/d1.jpg": {URL: "https://cdn/d1.jpg", PostID: "D"},
	}
	succeeded := map[string]bool{
		"https://cdn/a1.jpg": true, // A is partial: a2 failed
		"https://cdn/b1.jpg": true,
		"https://cdn/c1.jpg": true,
		// D failed entirely
	}
	got := fullySucceededIDs(urlToItem, succeeded)
	want := []string{"B", "https://cdn/c1.jpg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fullySucceededIDs = %v, want %v (A and D must stay retryable)", got, want)
	}
}

func TestFullySucceededIDsNoSuccesses(t *testing.T) {
	urlToItem := map[string]manifest.Item{
		"https://cdn/a1.jpg": {URL: "https://cdn/a1.jpg", PostID: "A"},
	}
	if got := fullySucceededIDs(urlToItem, map[string]bool{}); len(got) != 0 {
		t.Fatalf("fullySucceededIDs = %v, want empty", got)
	}
}

// syncTestEnv isolates the collection and session stores and swaps the
// scan/download seams for fakes, so sync behavior is tested without network.
func syncTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PROVENANCE_COLLECTION_FILE", filepath.Join(dir, "collections.json"))
	t.Setenv("PROVENANCE_SESSION_DIR", filepath.Join(dir, "sessions"))
	origScan, origDownload := scanFn, downloadFn
	origRecord, origAddSeen, origRecordSync := recordCaptureFn, addSeenFn, recordSyncFn
	t.Cleanup(func() {
		scanFn, downloadFn = origScan, origDownload
		recordCaptureFn, addSeenFn, recordSyncFn = origRecord, origAddSeen, origRecordSync
	})
	recordCaptureFn, addSeenFn, recordSyncFn = origRecord, origAddSeen, origRecordSync
}

func fakeScan(items []manifest.Item) func(context.Context, string, dispatcher.Options) (manifest.Manifest, error) {
	return func(ctx context.Context, rawURL string, opts dispatcher.Options) (manifest.Manifest, error) {
		return manifest.New(rawURL, "generic", items), nil
	}
}

func TestSyncPartialFailureKeepsFailedPostRetryable(t *testing.T) {
	syncTestEnv(t)
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: t.TempDir()}); err != nil {
		t.Fatalf("add: %v", err)
	}
	scanFn = fakeScan([]manifest.Item{
		{URL: "https://cdn.example.com/a1.jpg", PostID: "A", Kind: "photo"},
		{URL: "https://cdn.example.com/a2.jpg", PostID: "A", Kind: "photo"},
		{URL: "https://cdn.example.com/b1.jpg", PostID: "B", Kind: "photo"},
	})
	downloadFn = func(ctx context.Context, urls []string, batchPath string, opts dispatcher.Options) error {
		opts.Reporter.Success("https://cdn.example.com/a1.jpg")
		opts.Reporter.Failure("https://cdn.example.com/a2.jpg", errors.New("boom"))
		opts.Reporter.Success("https://cdn.example.com/b1.jpg")
		return errors.New("partial failure")
	}

	result, err := Sync(context.Background(), "feed", SyncOptions{})
	if err == nil {
		t.Fatal("Sync must surface the partial-failure error")
	}
	if result == nil {
		t.Fatal("Sync must return counts even on partial failure")
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.SeenIDs["A"] {
		t.Error("post A must stay retryable when one of its files failed")
	}
	if !c.SeenIDs["B"] {
		t.Error("fully succeeded post B must be marked seen even when other items failed")
	}
	if c.LastResult == nil {
		t.Error("sync result must be recorded on partial failure")
	}
}

func TestSyncMarksSeenAfterFullSuccess(t *testing.T) {
	syncTestEnv(t)
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: t.TempDir()}); err != nil {
		t.Fatalf("add: %v", err)
	}
	scanFn = fakeScan([]manifest.Item{
		{URL: "https://cdn.example.com/a1.jpg", PostID: "A", Kind: "photo"},
		{URL: "https://cdn.example.com/a2.jpg", PostID: "A", Kind: "photo"},
		{URL: "https://cdn.example.com/b1.jpg", PostID: "B", Kind: "photo"},
	})
	downloadFn = func(ctx context.Context, urls []string, batchPath string, opts dispatcher.Options) error {
		for _, u := range urls {
			opts.Reporter.Success(u)
		}
		return nil
	}

	result, err := Sync(context.Background(), "feed", SyncOptions{})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Failed != 0 || result.New != 3 {
		t.Fatalf("result = %+v, want 3 new / 0 failed", result)
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !c.SeenIDs["A"] || !c.SeenIDs["B"] {
		t.Fatalf("SeenIDs = %v, want both posts seen after full success", c.SeenIDs)
	}
}

func TestSyncDryRunDoesNotMarkSeen(t *testing.T) {
	syncTestEnv(t)
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: t.TempDir()}); err != nil {
		t.Fatalf("add: %v", err)
	}
	scanFn = fakeScan([]manifest.Item{
		{URL: "https://cdn.example.com/a1.jpg", PostID: "A", Kind: "photo"},
	})
	downloadFn = func(ctx context.Context, urls []string, batchPath string, opts dispatcher.Options) error {
		t.Error("dry-run must not download")
		return nil
	}

	if _, err := Sync(context.Background(), "feed", SyncOptions{DryRun: true}); err != nil {
		t.Fatalf("dry-run sync: %v", err)
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(c.SeenIDs) != 0 {
		t.Fatalf("dry-run marked items seen: %v", c.SeenIDs)
	}
}

func TestRecordCaptureStampsCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	c := Collection{
		Name:    "feed",
		URL:     "https://example.com/feed",
		Site:    "generic",
		Options: config.Config{OutputDir: dir},
	}
	if _, err := recordCapture(c, "sess-test", map[string]manifest.Item{}); err != nil {
		t.Fatalf("recordCapture: %v", err)
	}
	runs, err := manifest.ListRunManifests(dir)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run manifests = %v (err=%v), want exactly 1", runs, err)
	}
	cm, err := manifest.ReadCaptureManifest(runs[0])
	if err != nil {
		t.Fatalf("read capture manifest: %v", err)
	}
	if cm.ToolVersion != version.Version {
		t.Errorf("capture ToolVersion = %q, want %q", cm.ToolVersion, version.Version)
	}
}

func TestSyncAllStopsOnCancelledContext(t *testing.T) {
	syncTestEnv(t)
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: t.TempDir()}); err != nil {
		t.Fatalf("add: %v", err)
	}
	calls := 0
	scanFn = func(ctx context.Context, rawURL string, opts dispatcher.Options) (manifest.Manifest, error) {
		calls++
		return manifest.Manifest{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SyncAll(ctx, SyncOptions{}); err == nil {
		t.Fatal("SyncAll must return the context error")
	}
	if calls != 0 {
		t.Fatalf("SyncAll scanned %d collections after cancellation, want 0", calls)
	}
}
