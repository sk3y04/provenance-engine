package encode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/event"
)

// recordingSink captures the structured events an operation emits.
type recordingSink struct {
	mu  sync.Mutex
	got []event.Event
}

func (s *recordingSink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}

func (s *recordingSink) events() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.got...)
}

// captureStderr runs fn with os.Stderr redirected and returns whatever was
// written there, so legacy terminal narration can be asserted byte-for-byte.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	fn()

	_ = w.Close()
	os.Stderr = orig
	<-done
	return buf.String()
}

// TestEncodeEmitsStructuredEvents proves the sink path: with an event sink the
// encode batch emits a start stage, one config info, one item_done per file,
// and a terminal summary carrying the tallies.
func TestEncodeEmitsStructuredEvents(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")
	mkfile(t, dir, "b.mp4")

	sink := &recordingSink{}
	res, err := Run(context.Background(), Options{
		Dir: dir, Encoder: "qsv", Quality: 30, Preset: "medium", GOPSeconds: 10,
		Devices: []string{"d0"}, Events: sink,
	}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded != 2 {
		t.Fatalf("succeeded=%d, want 2", res.Succeeded)
	}

	var stages, infos, items, summaries int
	var sum *event.Counts
	for _, e := range sink.events() {
		switch e.Kind {
		case event.KindStageChanged:
			stages++
		case event.KindInfo:
			infos++
		case event.KindItemDone:
			items++
		case event.KindSummary:
			summaries++
			sum = e.Summary
		}
	}
	if stages != 1 {
		t.Fatalf("stage_changed events = %d, want 1", stages)
	}
	if infos != 1 {
		t.Fatalf("info events = %d, want 1 (encoder config line)", infos)
	}
	if items != 2 {
		t.Fatalf("item_done events = %d, want 2", items)
	}
	if summaries != 1 {
		t.Fatalf("summary events = %d, want 1", summaries)
	}
	if sum == nil || sum.Succeeded != 2 || sum.Discovered != 2 {
		t.Fatalf("summary counts = %+v, want 2 succeeded / 2 discovered", sum)
	}
}

// TestEncodeSinkSilencesTerminal proves the transport gate: with a sink,
// nothing is written to the terminal.
func TestEncodeSinkSilencesTerminal(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	sink := &recordingSink{}
	stderr := captureStderr(t, func() {
		_, _ = Run(context.Background(), Options{
			Dir: dir, Encoder: "qsv", Quality: 30, Events: sink,
		}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"})
	})
	if stderr != "" {
		t.Fatalf("terminal output with a sink: %q", stderr)
	}
	if len(sink.events()) == 0 {
		t.Fatal("expected events on the sink")
	}
}

// TestEncodeEmitsItemFailedEvent proves a failed encode carries a
// KindItemDone/item_failed event with the error and the input as ItemRef.
func TestEncodeEmitsItemFailedEvent(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	sink := &recordingSink{}
	_, err := Run(context.Background(), Options{
		Dir: dir, Encoder: "qsv", Quality: 30, Events: sink,
	}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24", encodeErr: errors.New("device busy")})
	if err == nil {
		t.Fatal("expected a non-nil error when an encode fails")
	}
	var failed *event.Event
	for i := range sink.events() {
		e := sink.events()[i]
		if e.Kind == event.KindItemDone && e.Reason == event.ReasonItemFailed {
			failed = &e
		}
	}
	if failed == nil {
		t.Fatal("no item_failed event emitted")
	}
	if failed.Err == nil {
		t.Fatal("expected Err on the failed item event")
	}
	if failed.ItemRef != "a.mp4" {
		t.Fatalf("ItemRef = %q, want a.mp4", failed.ItemRef)
	}
}

// TestEncodeLegacyOutputWithoutSink pins the no-sink contract: the legacy
// [provenance] CLI lines are preserved byte-for-byte on stderr.
func TestEncodeLegacyOutputWithoutSink(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	stderr := captureStderr(t, func() {
		_, _ = Run(context.Background(), Options{
			Dir: dir, Encoder: "qsv", Quality: 30, Preset: "medium", GOPSeconds: 10,
			Devices: []string{"d0"},
		}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"})
	})
	for _, want := range []string{
		"[provenance] encode: 1 input(s), 1 device worker(s): d0\n",
		"[provenance] encoder: av1_qsv | quality=30 | preset=\"medium\" | suffix=\"av1-q30\" | gop=10s | dry-run=false\n",
		"[provenance] d0: done 1/1  a.av1-q30.mkv (fps=24.00 gop=240)\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("legacy output missing %q;\n got: %s", want, stderr)
		}
	}
	if !strings.Contains(stderr, "done in ") {
		t.Errorf("legacy output missing summary line;\n got: %s", stderr)
	}
}

// TestEncodeDryRunLegacyLine pins the legacy dry-run command echo.
func TestEncodeDryRunLegacyLine(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	stderr := captureStderr(t, func() {
		_, _ = Run(context.Background(), Options{
			Dir: dir, Encoder: "qsv", Quality: 30, DryRun: true,
		}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"})
	})
	if !strings.Contains(stderr, "[provenance] dry-run (av1_qsv): ffmpeg ") {
		t.Errorf("legacy dry-run line missing;\n got: %s", stderr)
	}
}

// TestEncodeDryRunEmitsEventAndSilencesTerminal proves the dry-run command
// echo is routed through the sink (no terminal) when one is present.
func TestEncodeDryRunEmitsEventAndSilencesTerminal(t *testing.T) {
	stubBinaries(t)
	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	sink := &recordingSink{}
	stderr := captureStderr(t, func() {
		_, _ = Run(context.Background(), Options{
			Dir: dir, Encoder: "qsv", Quality: 30, DryRun: true, Events: sink,
		}, &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"})
	})
	if stderr != "" {
		t.Fatalf("terminal output with a sink: %q", stderr)
	}
	found := false
	for _, e := range sink.events() {
		if e.Reason == event.ReasonDryRun {
			found = true
		}
	}
	if !found {
		t.Fatal("no dry_run event emitted")
	}
}
