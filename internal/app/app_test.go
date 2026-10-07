package app

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
)

func TestDownloadRequiresURLOrBatch(t *testing.T) {
	err := Download(context.Background(), nil, "", dispatcher.Options{})
	if engineerr.KindOf(err) != engineerr.Validation {
		t.Fatalf("KindOf = %q, want validation (%v)", engineerr.KindOf(err), err)
	}
}

func TestDownloadInvalidURLWithEventsWritesNoTerminalOutput(t *testing.T) {
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

func TestDownloadLegacyPrintsToStderr(t *testing.T) {
	stderr := captureStderr(t, func() {
		_ = Download(context.Background(), []string{"http://[::1"}, "", dispatcher.Options{})
	})
	if !strings.Contains(stderr, "[provenance]") {
		t.Fatalf("legacy stderr output missing: %q", stderr)
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
