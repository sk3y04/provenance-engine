package collection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

func TestItemIdentityUsesExtractorAndStablePlatformID(t *testing.T) {
	item := manifest.Item{
		Source: "twitter", PostID: "111", ID: "111_0",
		URL: "https://pbs.twimg.com/media/a.jpg?token=SIGNED",
	}
	key, legacy := itemIdentity(item)
	if key != "twitter:111" || legacy != "111" {
		t.Fatalf("identity = %q/%q, want twitter:111/111", key, legacy)
	}
	other := item
	other.Source = "reddit"
	if key2, _ := itemIdentity(other); key2 == key {
		t.Fatalf("two extractors share the identity %q", key)
	}

	generic := manifest.Item{Source: "links", URL: "https://cdn.example/a.mp4?token=SIGNED#frag"}
	gkey, glegacy := itemIdentity(generic)
	if gkey != "links:url:https://cdn.example/a.mp4" {
		t.Fatalf("generic identity = %q, want a redacted URL key", gkey)
	}
	if strings.Contains(gkey, "SIGNED") {
		t.Fatalf("signed query leaked into the durable identity: %q", gkey)
	}
	if !strings.Contains(glegacy, "SIGNED") {
		t.Fatalf("legacy key should still match the old raw URL, got %q", glegacy)
	}

	// A store written before the extractor prefix existed still skips the item.
	_, _, _, _, skipped := partitionNewItems([]manifest.Item{item}, map[string]bool{"111": true})
	if skipped != 1 {
		t.Fatalf("legacy seen id did not skip the item: skipped=%d", skipped)
	}
}

func TestCaptureExternalIDsAreStableAndUnique(t *testing.T) {
	a := manifest.Item{Source: "twitter", PostID: "111", ID: "111_0", URL: "https://cdn/a.jpg"}
	b := manifest.Item{Source: "twitter", PostID: "111", ID: "111_1", URL: "https://cdn/b.jpg"}
	if captureExternalID(a) == captureExternalID(b) {
		t.Fatalf("two media of one post share external id %q", captureExternalID(a))
	}
	if captureExternalID(a) != captureExternalID(a) {
		t.Fatal("capture external id is not deterministic")
	}
	// No media id: distinct URLs still get distinct identities.
	c := manifest.Item{Source: "twitter", PostID: "111", URL: "https://cdn/c.jpg?token=x"}
	d := manifest.Item{Source: "twitter", PostID: "111", URL: "https://cdn/d.jpg?token=y"}
	if captureExternalID(c) == captureExternalID(d) {
		t.Fatalf("URL fallback collided: %q", captureExternalID(c))
	}
	if strings.Contains(captureExternalID(c), "token") {
		t.Fatalf("signed URL leaked into capture id: %q", captureExternalID(c))
	}
}

func downloadFilesAndReport(t *testing.T, opts dispatcher.Options, urls []string) error {
	t.Helper()
	for _, raw := range urls {
		name := filepath.Base(strings.Split(raw, "?")[0])
		if err := os.WriteFile(filepath.Join(opts.Config.OutputDir, name), []byte(raw), 0o644); err != nil {
			return err
		}
		opts.Reporter.Success(raw)
	}
	return nil
}

func TestSyncSameCollectionIsSerialized(t *testing.T) {
	syncTestEnv(t)
	out := t.TempDir()
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	scanFn = func(ctx context.Context, rawURL string, opts dispatcher.Options) (manifest.Manifest, error) {
		close(started)
		<-release
		return manifest.New(rawURL, "generic", []manifest.Item{{
			URL: "https://cdn.example/a.jpg", Filename: "a.jpg", Source: "generic", ID: "a",
		}}), nil
	}
	downloadFn = func(ctx context.Context, urls []string, batch string, opts dispatcher.Options) error {
		return downloadFilesAndReport(t, opts, urls)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Sync(context.Background(), "feed", SyncOptions{})
		done <- err
	}()
	<-started

	if _, err := Sync(context.Background(), "feed", SyncOptions{}); !errors.Is(err, ErrCollectionSyncing) {
		t.Fatalf("second sync = %v, want ErrCollectionSyncing", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first sync: %v", err)
	}
}

func TestSyncCaptureFailureLeavesItemRetryable(t *testing.T) {
	syncTestEnv(t)
	out := t.TempDir()
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	scanFn = fakeScan([]manifest.Item{{URL: "https://cdn.example/a.jpg", Filename: "a.jpg", Source: "generic", ID: "a"}})
	downloadFn = func(ctx context.Context, urls []string, batch string, opts dispatcher.Options) error {
		return downloadFilesAndReport(t, opts, urls)
	}
	recordCaptureFn = func(c Collection, sess string, items map[string]manifest.Item) ([]string, error) {
		return nil, errors.New("disk full while writing manifest")
	}

	result, err := Sync(context.Background(), "feed", SyncOptions{Record: true})
	if err == nil || !strings.Contains(err.Error(), "record capture") {
		t.Fatalf("sync = %v, want a capture failure", err)
	}
	if result == nil {
		t.Fatal("sync must return counts on capture failure")
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if c.SeenIDs["generic:a"] {
		t.Fatal("an item was marked seen before its capture committed")
	}
	if c.LastResult == nil {
		t.Fatal("the sync result was not recorded on capture failure")
	}
}

func TestSyncSeenWriteFailureKeepsCommittedCapture(t *testing.T) {
	syncTestEnv(t)
	out := t.TempDir()
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	scanFn = fakeScan([]manifest.Item{{URL: "https://cdn.example/a.jpg", Filename: "a.jpg", Source: "generic", ID: "a"}})
	downloadFn = func(ctx context.Context, urls []string, batch string, opts dispatcher.Options) error {
		return downloadFilesAndReport(t, opts, urls)
	}
	addSeenFn = func(name string, ids ...string) error { return errors.New("seen store busy") }

	if _, err := Sync(context.Background(), "feed", SyncOptions{Record: true}); err == nil {
		t.Fatal("sync succeeded although the seen write failed")
	}
	runs, err := manifest.ListRunManifests(out)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run manifests = %v (err=%v), want one committed capture", runs, err)
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if c.SeenIDs["generic:a"] {
		t.Fatal("seen id was written despite the failure")
	}

	// A retry is safe: the artifact and capture are already committed, and the
	// item becomes seen only after the replay reaches the seen write again.
	addSeenFn = AddSeen
	if _, err := Sync(context.Background(), "feed", SyncOptions{Record: true}); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	c, err = Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if !c.SeenIDs["generic:a"] {
		t.Fatal("retry did not mark the item seen")
	}
}

func TestSyncResultWriteFailureKeepsSeen(t *testing.T) {
	syncTestEnv(t)
	out := t.TempDir()
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	scanFn = fakeScan([]manifest.Item{{URL: "https://cdn.example/a.jpg", Filename: "a.jpg", Source: "generic", ID: "a"}})
	downloadFn = func(ctx context.Context, urls []string, batch string, opts dispatcher.Options) error {
		return downloadFilesAndReport(t, opts, urls)
	}
	recordSyncFn = func(name string, result SyncResult) error { return errors.New("result store busy") }

	if _, err := Sync(context.Background(), "feed", SyncOptions{Record: true}); err == nil {
		t.Fatal("sync succeeded although the result write failed")
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if !c.SeenIDs["generic:a"] {
		t.Fatal("seen must survive a later result-write failure")
	}
}

func TestSyncRecordsOnlyFullySucceededPosts(t *testing.T) {
	syncTestEnv(t)
	out := t.TempDir()
	if err := Add("feed", "https://example.com/feed", "generic", config.Config{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	scanFn = fakeScan([]manifest.Item{
		{URL: "https://cdn.example/a1.jpg", Filename: "a1.jpg", Source: "twitter", ID: "A_0", PostID: "A"},
		{URL: "https://cdn.example/a2.jpg", Filename: "a2.jpg", Source: "twitter", ID: "A_1", PostID: "A"},
		{URL: "https://cdn.example/b1.jpg", Filename: "b1.jpg", Source: "twitter", ID: "B_0", PostID: "B"},
	})
	downloadFn = func(ctx context.Context, urls []string, batch string, opts dispatcher.Options) error {
		for _, raw := range urls {
			name := filepath.Base(raw)
			if name == "a2.jpg" {
				opts.Reporter.Failure(raw, errors.New("boom"))
				continue
			}
			if err := os.WriteFile(filepath.Join(opts.Config.OutputDir, name), []byte(raw), 0o644); err != nil {
				return err
			}
			opts.Reporter.Success(raw)
		}
		return errors.New("partial")
	}

	if _, err := Sync(context.Background(), "feed", SyncOptions{Record: true}); err == nil {
		t.Fatal("partial download must surface an error")
	}
	c, err := Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if c.SeenIDs["twitter:A"] {
		t.Fatal("partial post A was marked seen")
	}
	if !c.SeenIDs["twitter:B"] {
		t.Fatal("fully succeeded post B was not marked seen")
	}
	runs, err := manifest.ListRunManifests(out)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run manifests = %v (err=%v)", runs, err)
	}
	cm, err := manifest.ReadCaptureManifest(runs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.Items) != 1 || cm.Items[0].ExternalID != "twitter:B_0" {
		t.Fatalf("capture items = %+v, want only post B", cm.Items)
	}
}
