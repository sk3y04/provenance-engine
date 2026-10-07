package watch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance/internal/config"
)

func TestWatchStoreLifecycle(t *testing.T) {
	t.Setenv("PROVENANCE_WATCH_FILE", filepath.Join(t.TempDir(), "watch.json"))

	if err := Add("creator one", "https://example.com/a", config.Config{OutputDir: "downloads"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := Add("creator two", "https://example.com/b", config.Config{Quality: "720"}); err != nil {
		t.Fatalf("Add second: %v", err)
	}
	subs, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(subs) != 2 || subs[0].Name != "creator-one" {
		t.Fatalf("subs = %+v", subs)
	}
	sub, err := Get("creator one")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sub.URL != "https://example.com/a" {
		t.Fatalf("URL = %q", sub.URL)
	}
	if err := MarkRun("creator one"); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	sub, _ = Get("creator one")
	if sub.LastRunAt.IsZero() {
		t.Fatalf("LastRunAt was not set")
	}
	if err := Remove("creator two"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	subs, _ = List()
	if len(subs) != 1 {
		t.Fatalf("len(subs) = %d, want 1", len(subs))
	}
}

func TestWatchMarkRunWithStatus(t *testing.T) {
	t.Setenv("PROVENANCE_WATCH_FILE", filepath.Join(t.TempDir(), "watch.json"))

	if err := Add("feed", "https://example.com/feed", config.Config{OutputDir: "downloads"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	dur := 42 * time.Second
	if err := MarkRunWithStatus("feed", dur, "success"); err != nil {
		t.Fatalf("MarkRunWithStatus: %v", err)
	}
	sub, err := Get("feed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sub.LastRunAt.IsZero() {
		t.Fatal("LastRunAt was not set")
	}
	if sub.LastRunDuration != dur {
		t.Fatalf("LastRunDuration = %v, want %v", sub.LastRunDuration, dur)
	}
	if sub.LastRunStatus != "success" {
		t.Fatalf("LastRunStatus = %q, want success", sub.LastRunStatus)
	}

	// A subsequent failed run should overwrite status and duration.
	if err := MarkRunWithStatus("feed", time.Second, "failed"); err != nil {
		t.Fatalf("MarkRunWithStatus (second): %v", err)
	}
	sub, _ = Get("feed")
	if sub.LastRunStatus != "failed" {
		t.Fatalf("LastRunStatus = %q, want failed", sub.LastRunStatus)
	}
	if sub.LastRunDuration != time.Second {
		t.Fatalf("LastRunDuration = %v, want 1s", sub.LastRunDuration)
	}
}
