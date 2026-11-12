package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/history"
)

func TestDownloadRequiresURLOrBatch(t *testing.T) {
	err := Download(context.Background(), nil, "", dispatcher.Options{})
	if engineerr.KindOf(err) != engineerr.Validation {
		t.Fatalf("KindOf = %q, want validation (%v)", engineerr.KindOf(err), err)
	}
}

func TestDownloadLegacyPrintsToStderr(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stderr := captureStderr(t, func() {
		_ = Download(context.Background(), []string{"http://[::1"}, "", dispatcher.Options{})
	})
	if !strings.Contains(stderr, "[provenance]") {
		t.Fatalf("legacy stderr output missing: %q", stderr)
	}
}

// TestDownloadRecordsHistory: a completed CLI run must leave a history
// entry with its URLs, outcome counts, and any error.
func TestDownloadRecordsHistory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	started := time.Now()
	err := Download(context.Background(), []string{"http://[::1"}, "", dispatcher.Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	runs, lerr := history.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(runs) != 1 {
		t.Fatalf("history runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if len(run.URLs) != 1 || run.URLs[0] != "http://[::1" {
		t.Fatalf("urls = %v", run.URLs)
	}
	if run.Succeeded != 0 || run.Failed != 1 {
		t.Fatalf("counts = ok:%d fail:%d, want 0/1", run.Succeeded, run.Failed)
	}
	if run.StartedAt.IsZero() || run.StartedAt.Before(started.Add(-time.Second)) {
		t.Fatalf("started = %v", run.StartedAt)
	}
	if run.CompletedAt.Before(run.StartedAt) {
		t.Fatalf("completed before started: %v vs %v", run.CompletedAt, run.StartedAt)
	}
	if run.Error == "" {
		t.Fatal("run error not recorded")
	}
}

// TestDownloadDryRunSkipsHistory: dry runs must not leave history entries.
func TestDownloadDryRunSkipsHistory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	_ = Download(context.Background(), []string{"http://[::1"}, "", dispatcher.Options{DryRun: true})
	runs, err := history.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("dry run left %d history entries", len(runs))
	}
}

// TestBatchDownloadRecordsHistoryFromBatchFile: a batch run is recorded with
// the batch file as title and its URLs as the run's URL list.
func TestBatchDownloadRecordsHistoryFromBatchFile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	batch := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(batch, []byte("http://[::1\n# comment\nftp://[::2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = Download(context.Background(), nil, batch, dispatcher.Options{})

	runs, err := history.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("history runs = %d, want 1", len(runs))
	}
	if !strings.Contains(runs[0].Title, "urls.txt") {
		t.Fatalf("title = %q, want the batch file name", runs[0].Title)
	}
	if len(runs[0].URLs) != 2 {
		t.Fatalf("urls = %v, want the two batch URLs", runs[0].URLs)
	}
}

func TestDownloadInvalidURLWithEventsWritesNoTerminalOutput(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var got []event.Event
	stderr := captureStderr(t, func() {
		opts := dispatcher.Options{Events: event.Func(func(_ context.Context, e event.Event) {
			got = append(got, e)
		})}
		if err := Download(context.Background(), []string{"http://[::1"}, "", opts); err == nil {
			t.Fatal("expected an error")
		}
	})
	if stderr != "" {
		t.Fatalf("expected no terminal output, got %q", stderr)
	}
	if len(got) == 0 {
		t.Fatal("expected structured events")
	}
	var sawItem, sawSummary bool
	for _, e := range got {
		switch e.Kind {
		case event.KindItemDone:
			sawItem = true
		case event.KindSummary:
			sawSummary = true
		}
	}
	if !sawItem || !sawSummary {
		t.Fatalf("missing item/summary events: %+v", got)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	_ = w.Close()
	data, _ := io.ReadAll(r)
	_ = r.Close()
	return string(data)
}
