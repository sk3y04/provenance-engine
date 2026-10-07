package dispatcher

import (
	"context"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
)

func TestDispatchInvalidURLIsValidation(t *testing.T) {
	var got []event.Event
	opts := Options{Events: event.Func(func(_ context.Context, e event.Event) {
		got = append(got, e)
	})}

	err := Dispatch(context.Background(), "http://[::1", opts)
	if engineerr.KindOf(err) != engineerr.Validation {
		t.Fatalf("KindOf = %q, want validation (%v)", engineerr.KindOf(err), err)
	}
	// Classification fails before the resolving stage event is emitted.
	if len(got) != 0 {
		t.Fatalf("unexpected events: %+v", got)
	}
}

func TestScanResolvedInvalidURLIsValidation(t *testing.T) {
	_, err := ScanResolved(context.Background(), "http://[::1", Options{})
	if engineerr.KindOf(err) != engineerr.Validation {
		t.Fatalf("KindOf = %q, want validation (%v)", engineerr.KindOf(err), err)
	}
}

func TestScanInvalidURLIsValidation(t *testing.T) {
	_, err := Scan(context.Background(), "http://[::1", Options{})
	if engineerr.KindOf(err) != engineerr.Validation {
		t.Fatalf("KindOf = %q, want validation (%v)", engineerr.KindOf(err), err)
	}
}

func TestClassifyErrorIsTyped(t *testing.T) {
	_, err := Classify("http://[::1")
	if engineerr.KindOf(err) != "" {
		// Classify itself returns a plain error; categorization happens at the
		// dispatcher boundary (Dispatch/Scan). This asserts the boundary is the
		// one that types it, documented here to prevent drift.
		t.Fatalf("Classify unexpectedly typed: %v", err)
	}
}
