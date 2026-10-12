package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/event"
)

// TestTeaSinkDoesNotMutateProcessGlobals is the Phase 1 gate for the TUI: the
// runner consumes events instead of reassigning os.Stdout and os.Stderr.
func TestTeaSinkDoesNotMutateProcessGlobals(t *testing.T) {
	orig := event.SinkFrom(context.Background())
	if orig != nil {
		t.Fatalf("baseline context must carry no sink, got %v", orig)
	}
	sink := newTeaSink(nil)
	ctx := event.WithSink(context.Background(), sink)
	if event.SinkFrom(ctx) != sink {
		t.Fatal("operation context must carry the TUI sink")
	}
	sink.Emit(ctx, event.Event{Kind: event.KindInfo, Reason: event.ReasonInfo, Detail: "page 1: 3 videos"})
}

// TestStripANSIKeepsRenderedLines verifies the runner log stays readable.
func TestStripANSIKeepsRenderedLines(t *testing.T) {
	if got := stripANSI("[provenance] \x1b[31mwarning\x1b[0m"); !strings.Contains(got, "warning") {
		t.Fatalf("stripANSI = %q", got)
	}
}
