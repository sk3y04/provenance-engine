package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/config"
)

// TestTransitionSurvivesConcurrentModification proves a status transition is
// re-applied after a conflict reload instead of being silently dropped.
func TestTransitionSurvivesConcurrentModification(t *testing.T) {
	t.Setenv("PROVENANCE_SESSION_DIR", t.TempDir())

	s, err := OpenOrCreate("conflict", config.Config{OutputDir: "downloads"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddURLs([]string{"https://example.com/a"}, "argument"); err != nil {
		t.Fatal(err)
	}

	// Simulate another process saving the session: bump the on-disk version.
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Session
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk.Version++
	bumped, err := json.MarshalIndent(&onDisk, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), bumped, 0o600); err != nil {
		t.Fatal(err)
	}

	s.Success("https://example.com/a")

	loaded, err := Load("conflict")
	if err != nil {
		t.Fatal(err)
	}
	e := loaded.Entries["https://example.com/a"]
	if e == nil || e.Status != StatusSucceeded {
		t.Fatalf("transition lost on disk after concurrent modification: %+v", e)
	}
	if got := s.Entries["https://example.com/a"]; got == nil || got.Status != StatusSucceeded {
		t.Fatalf("in-memory transition lost after concurrent modification: %+v", got)
	}
}

func TestSessionLifecycle(t *testing.T) {
	t.Setenv("PROVENANCE_SESSION_DIR", t.TempDir())

	s, err := OpenOrCreate("my videos", config.Config{
		OutputDir:   "downloads",
		Concurrency: 6,
		Quality:     "720",
	})
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}

	added, err := s.AddURLs([]string{
		"https://example.com/a",
		"https://example.com/a",
		"  https://example.com/b  ",
		"",
	}, "argument")
	if err != nil {
		t.Fatalf("AddURLs: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}

	s.Start("https://example.com/a")
	s.Success("https://example.com/a")
	s.Start("https://example.com/b")
	s.Failure("https://example.com/b", errors.New("network failed"))
	s.Queue("https://example.com/c", "discovered")
	s.Start("https://example.com/c")

	reset, err := s.ResetRunning()
	if err != nil {
		t.Fatalf("ResetRunning: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset = %d, want 1", reset)
	}

	loaded, err := Load("my videos")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	counts := loaded.Counts()
	if counts.Total != 3 || counts.Succeeded != 1 || counts.Failed != 1 || counts.Pending != 1 {
		t.Fatalf("counts = %+v, want total=3 succeeded=1 failed=1 pending=1", counts)
	}

	failed := loaded.EntriesByStatus(StatusFailed)
	if len(failed) != 1 || failed[0].URL != "https://example.com/b" || !strings.Contains(failed[0].LastError, "network failed") {
		t.Fatalf("failed entries = %+v", failed)
	}

	if _, err := os.Stat(loaded.Path()); err != nil {
		t.Fatalf("session file was not written: %v", err)
	}
}

func TestInvalidSessionNames(t *testing.T) {
	t.Setenv("PROVENANCE_SESSION_DIR", t.TempDir())

	bad := []string{"", "..", "../x", `bad\name`, "bad:name"}
	for _, name := range bad {
		if _, err := OpenOrCreate(name, config.Config{}); err == nil {
			t.Fatalf("OpenOrCreate(%q) succeeded, want error", name)
		}
	}
}

// TestConcurrentInstancesDoNotLoseUpdates simulates two processes (separate
// Session instances, separate in-process mutexes) writing the same session
// file concurrently. Every queued URL must survive: the cross-process file
// lock serializes the version-check-and-write cycle so no update is lost.
func TestConcurrentInstancesDoNotLoseUpdates(t *testing.T) {
	t.Setenv("PROVENANCE_SESSION_DIR", t.TempDir())

	a, err := OpenOrCreate("shared", config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenOrCreate("shared", config.Config{})
	if err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			a.Queue(fmt.Sprintf("https://a.example/%d", i), "test")
		}(i)
		go func(i int) {
			defer wg.Done()
			b.Queue(fmt.Sprintf("https://b.example/%d", i), "test")
		}(i)
	}
	wg.Wait()

	final, err := Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Entries) != 2*n {
		t.Fatalf("entries = %d, want %d (lost updates under concurrency)", len(final.Entries), 2*n)
	}
}
