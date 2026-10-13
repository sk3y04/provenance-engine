package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestBatchDispatchWithSinkWritesNoTerminalOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(path, []byte("http://[::1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []event.Event
	opts := Options{Events: event.Func(func(_ context.Context, e event.Event) {
		got = append(got, e)
	})}

	out, errStr := captureStd(t, func() {
		err := BatchDispatch(context.Background(), path, opts)
		if engineerr.KindOf(err) != engineerr.Validation {
			t.Fatalf("BatchDispatch kind = %q, want validation (%v)", engineerr.KindOf(err), err)
		}
	})
	if out != "" || errStr != "" {
		t.Fatalf("terminal output with sink: stdout=%q stderr=%q", out, errStr)
	}
	if len(got) == 0 {
		t.Fatal("expected structured events")
	}
	for _, e := range got {
		if e.Kind == event.KindInfo && e.Detail == "" {
			t.Fatalf("info event missing detail: %+v", e)
		}
	}
}

func captureStd(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	var ob, eb bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&ob, outR)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&eb, errR)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	return ob.String(), eb.String()
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

// TestEmitEventRedacts proves the sanctioned direct-emission path cannot carry a
// canary cookie, token, or query string in URL, Detail, or Err.
func TestEmitEventRedacts(t *testing.T) {
	var got []event.Event
	sink := event.Func(func(_ context.Context, e event.Event) { got = append(got, e) })

	emitEvent(context.Background(), sink, event.Event{
		Kind:   event.KindWarning,
		URL:    "https://x.com/a?token=CANARYQUERY",
		Reason: event.ReasonResponseError,
		Detail: "auth_token=CANARYTOKEN; sessionid=CANARYCOOKIE",
		Err:    errors.New("failed https://cdn.example/f?token=CANARYERR"),
	})

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	e := got[0]
	for _, secret := range []string{"CANARYQUERY", "CANARYTOKEN", "CANARYCOOKIE", "CANARYERR"} {
		if strings.Contains(e.URL, secret) || strings.Contains(e.Detail, secret) || strings.Contains(e.Err.Error(), secret) {
			t.Fatalf("canary %q leaked into event: %+v", secret, e)
		}
	}
	if !strings.Contains(e.URL, "https://x.com/a") {
		t.Fatalf("url = %q", e.URL)
	}
}
