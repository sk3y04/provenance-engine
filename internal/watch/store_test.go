package watch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/config"
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

// TestWatchMarkInterruptedIfRunning proves a run left "running" by a crash is
// recovered as interrupted, preserving its start time and partial duration,
// and that completed runs are untouched.
func TestWatchMarkInterruptedIfRunning(t *testing.T) {
	t.Setenv("PROVENANCE_WATCH_FILE", filepath.Join(t.TempDir(), "watch.json"))

	if err := Add("feed", "https://example.com/feed", config.Config{OutputDir: "downloads"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// No run yet: nothing to recover.
	if recovered, err := MarkInterruptedIfRunning("feed"); err != nil || recovered {
		t.Fatalf("recovered = %v, err = %v; want false, nil", recovered, err)
	}

	if err := MarkRunWithStatus("feed", 0, "running"); err != nil {
		t.Fatalf("MarkRunWithStatus: %v", err)
	}
	before, _ := Get("feed")

	recovered, err := MarkInterruptedIfRunning("feed")
	if err != nil || !recovered {
		t.Fatalf("recovered = %v, err = %v; want true, nil", recovered, err)
	}
	sub, _ := Get("feed")
	if sub.LastRunStatus != "interrupted" {
		t.Fatalf("LastRunStatus = %q, want interrupted", sub.LastRunStatus)
	}
	if !sub.LastRunAt.Equal(before.LastRunAt) || sub.LastRunDuration != before.LastRunDuration {
		t.Fatalf("crashed run's start/duration not preserved: %+v vs %+v", sub, before)
	}

	// Idempotent: a second call finds nothing to recover.
	if recovered, err := MarkInterruptedIfRunning("feed"); err != nil || recovered {
		t.Fatalf("second call recovered = %v, err = %v; want false, nil", recovered, err)
	}

	// A completed run is not rewritten.
	if err := MarkRunWithStatus("feed", time.Second, "success"); err != nil {
		t.Fatalf("MarkRunWithStatus: %v", err)
	}
	if recovered, err := MarkInterruptedIfRunning("feed"); err != nil || recovered {
		t.Fatalf("success run recovered = %v, err = %v; want false, nil", recovered, err)
	}
	sub, _ = Get("feed")
	if sub.LastRunStatus != "success" {
		t.Fatalf("LastRunStatus = %q, want success untouched", sub.LastRunStatus)
	}
}
