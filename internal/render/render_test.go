package render

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
)

func TestSinkRendering(t *testing.T) {
	var buf bytes.Buffer
	s := NewSink(&buf)
	ctx := context.Background()

	s.Emit(ctx, event.Event{Kind: event.KindStageChanged, Stage: event.StageResolving, URL: "u1"})
	s.Emit(ctx, event.Event{Kind: event.KindWarning, Reason: "ffmpeg_missing", Detail: "ffmpeg not found"})
	s.Emit(ctx, event.Event{Kind: event.KindRetry, Attempt: 2, URL: "u1"})
	s.Emit(ctx, event.Event{Kind: event.KindItemDone, URL: "u1"})
	s.Emit(ctx, event.Event{Kind: event.KindItemDone, URL: "u2", Err: errors.New("boom")})
	s.Emit(ctx, event.Event{Kind: event.KindSummary, Summary: &event.Counts{
		Discovered: 3, Succeeded: 1, Failed: 1, Skipped: 1,
	}})

	want := "[provenance] resolving: u1\n" +
		"[provenance] WARNING: ffmpeg not found\n" +
		"[provenance] retry 2 for u1\n" +
		"[provenance] OK: u1\n" +
		"[provenance] FAILED u2: boom\n" +
		"\ndiscovered: 3\n" +
		"downloaded: 1\n" +
		"skipped:    1\n" +
		"failed:     1\n"
	if got := buf.String(); got != want {
		t.Fatalf("rendered output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestSinkRendersTypedErrorHint(t *testing.T) {
	var buf bytes.Buffer
	s := NewSink(&buf)
	s.Emit(context.Background(), event.Event{
		Kind: event.KindItemDone,
		URL:  "u",
		Err:  engineerr.New(engineerr.AuthRequired, "yt-dlp", "u", errors.New("HTTP Error 403: Forbidden")),
	})
	got := buf.String()
	if !bytes.Contains([]byte(got), []byte("FAILED u:")) || !bytes.Contains([]byte(got), []byte("hint:")) {
		t.Fatalf("expected failure line and hint, got %q", got)
	}
}

func TestSinkIgnoresProgressAndNilWriter(t *testing.T) {
	var buf bytes.Buffer
	NewSink(&buf).Emit(context.Background(), event.Event{Kind: event.KindProgress, Written: 5, Total: 10})
	if buf.Len() != 0 {
		t.Fatalf("progress should not render, got %q", buf.String())
	}
	NewSink(nil).Emit(context.Background(), event.Event{Kind: event.KindItemDone, URL: "u"})
}
