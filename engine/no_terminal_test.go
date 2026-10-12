package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	ievent "github.com/sk3y04/provenance-engine/internal/event"
)

// captureStd redirects os.Stdout and os.Stderr while fn runs and returns what
// each stream received. The pipes are closed before the buffers are read so the
// helper itself is race-clean.
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

// TestNilSinkStillCarriesASink is the gate for a hosted caller: a nil public
// sink becomes a discarding sink carried on the operation context, so no
// extractor path can fall back to terminal output.
func TestNilSinkStillCarriesASink(t *testing.T) {
	e, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	sink, closeSink := e.sink(nil)
	defer closeSink()
	if sink == nil {
		t.Fatal("engine must always provide a sink")
	}
	ctx := ievent.WithSink(context.Background(), sink)
	if ievent.SinkFrom(ctx) == nil {
		t.Fatal("operation context must carry the sink for nested extractor paths")
	}
}

// TestFacadeCallWithSinkWritesNoTerminalOutput proves the acceptance gate on the
// coordination layer: a facade call with a sink emits events and writes nothing
// to the terminal.
func TestFacadeCallWithSinkWritesNoTerminalOutput(t *testing.T) {
	e, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var got int
	sink := SinkFunc(func(_ context.Context, _ Event) { got++ })

	stdout, stderr := captureStd(t, func() {
		_, _ = e.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/x"}, sink)
	})
	if stdout != "" || stderr != "" {
		t.Fatalf("terminal output from a facade call with a sink: stdout=%q stderr=%q", stdout, stderr)
	}
	if got == 0 {
		t.Fatal("no events were emitted")
	}

	// The same call with a nil sink must still not write to the terminal.
	stdout, stderr = captureStd(t, func() {
		_, _ = e.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/x"}, nil)
	})
	if stdout != "" || stderr != "" {
		t.Fatalf("terminal output from a facade call with a nil sink: stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestSlowAndPanickingSink proves a foreign sink can neither stall nor cancel
// extraction: delivery is bounded and panic-recovering.
func TestSlowAndPanickingSink(t *testing.T) {
	e, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	panicking := 0
	sink := SinkFunc(func(_ context.Context, _ Event) {
		panicking++
		panic("sink panic must not escape")
	})
	_, _ = e.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/x"}, sink)
	if panicking == 0 {
		t.Fatal("sink was never invoked")
	}

	blocked := make(chan struct{})
	slow := SinkFunc(func(_ context.Context, _ Event) { <-blocked })
	done := make(chan struct{})
	go func() {
		_, _ = e.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/x"}, slow)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("a slow sink stalled the operation")
	}
	close(blocked)
}
