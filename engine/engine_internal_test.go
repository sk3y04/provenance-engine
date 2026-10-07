package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/engineerr"
	ievent "github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/resolve"
	"github.com/sk3y04/provenance-engine/internal/worker"
)

func TestEffectiveLimitClampsToMaxItems(t *testing.T) {
	e := &Engine{cfg: Config{MaxItems: 10}}
	cases := map[int]int{0: 10, 5: 5, 20: 10, -1: 10}
	for in, want := range cases {
		if got := e.effectiveLimit(in); got != want {
			t.Errorf("effectiveLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestMapKind(t *testing.T) {
	cases := map[engineerr.Kind]ErrorKind{
		engineerr.Validation:        ErrorValidation,
		engineerr.AuthRequired:      ErrorAuthRequired,
		engineerr.UnsupportedSource: ErrorUnsupportedSource,
		engineerr.RateLimited:       ErrorRateLimited,
		engineerr.Temporary:         ErrorTemporary,
		engineerr.Canceled:          ErrorCanceled,
		engineerr.ExternalTool:      ErrorExternalTool,
		engineerr.Permanent:         ErrorPermanent,
	}
	for in, want := range cases {
		if got := mapKind(in); got != want {
			t.Errorf("mapKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWrapErrorCategories(t *testing.T) {
	e := &Engine{}
	plain := errors.New("boom")

	tests := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"canceled", context.Canceled, ErrorCanceled},
		{"deadline", context.DeadlineExceeded, ErrorCanceled},
		{"permanent", worker.Permanent(plain), ErrorPermanent},
		{"engineerr-auth", engineerr.New(engineerr.AuthRequired, "op", "u", plain), ErrorAuthRequired},
		{"unknown", plain, ErrorTemporary},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := e.wrapError("op", "u", tc.err)
			if ErrorKindOf(got) != tc.want {
				t.Fatalf("ErrorKindOf = %q, want %q", ErrorKindOf(got), tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("errors.Is does not reach the original cause")
			}
		})
	}
	if e.wrapError("op", "u", nil) != nil {
		t.Fatal("wrapError(nil) should be nil")
	}
}

func TestPanicErrPrefersCancellation(t *testing.T) {
	e := &Engine{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := e.panicErr("resolve", "u", ctx, "boom")
	if ErrorKindOf(got) != ErrorCanceled {
		t.Fatalf("ErrorKindOf = %q, want canceled", ErrorKindOf(got))
	}
	other := e.panicErr("resolve", "u", context.Background(), "boom")
	if ErrorKindOf(other) != ErrorPermanent {
		t.Fatalf("ErrorKindOf = %q, want permanent", ErrorKindOf(other))
	}
}

func TestCollectArtifactsSkipsBookkeeping(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "video.mp4"), "hello")
	mustWrite(t, filepath.Join(root, "_metadata", "video.info.json"), "{}")
	mustWrite(t, filepath.Join(root, "partial.mp4.part"), "half")
	mustWrite(t, filepath.Join(root, "_provenance_cache", "archive.txt"), "ok:u\n")
	mustWrite(t, filepath.Join(root, ".provenance", "runs", "x.json"), "{}")

	got, err := collectArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Filename)
	}
	want := []string{"_metadata/video.info.json", "video.mp4"}
	if len(names) != len(want) {
		t.Fatalf("artifacts = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("artifacts = %v, want %v", names, want)
		}
	}
	if got[1].SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("sha256 = %q", got[1].SHA256)
	}
	if got[1].MIMEType != "video/mp4" {
		t.Fatalf("mime = %q, want video/mp4", got[1].MIMEType)
	}
	if got[1].Size != 5 {
		t.Fatalf("size = %d, want 5", got[1].Size)
	}
	if !filepath.IsAbs(got[1].Path) {
		t.Fatalf("path %q is not absolute", got[1].Path)
	}
}

func TestCollectArtifactsMissingDir(t *testing.T) {
	got, err := collectArtifacts(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil artifacts, got %v", got)
	}
}

func TestFromSourceMapping(t *testing.T) {
	published := time.Now().UTC().Truncate(time.Second)
	in := resolve.Source{
		URL:          "https://example.com/u",
		CanonicalURL: "https://example.com/u?canonical=1",
		Kind:         resolve.KindFeed,
		Extractor:    "ytdlp",
		Title:        "t",
		Author:       "a",
		Items: []resolve.Item{{
			ExternalID:  "id1",
			URL:         "https://example.com/i/1",
			Title:       "item",
			Author:      "author",
			PublishedAt: &published,
			Media: []resolve.MediaAsset{{
				URL: "https://example.com/i/1.mp4", Filename: "1.mp4",
				Extension: "mp4", Size: 42, Kind: resolve.MediaVideo,
			}},
			Text: &resolve.TextContent{Body: "body", Format: resolve.FormatMarkdown},
		}},
	}

	got := fromSource(in)
	if got.Kind != SourceFeed || got.Extractor != "ytdlp" || got.CanonicalURL != in.CanonicalURL {
		t.Fatalf("source mapping = %+v", got)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	it := got.Items[0]
	if it.ExternalID != "id1" || it.PublishedAt == nil || !it.PublishedAt.Equal(published) {
		t.Fatalf("item mapping = %+v", it)
	}
	if len(it.Media) != 1 || it.Media[0].Kind != MediaVideo || it.Media[0].Size != 42 {
		t.Fatalf("media mapping = %+v", it.Media)
	}
	if it.Text == nil || it.Text.Format != TextMarkdown || it.Text.Body != "body" {
		t.Fatalf("text mapping = %+v", it.Text)
	}
}

func TestFromEventMapping(t *testing.T) {
	in := ievent.Event{
		Kind: ievent.KindSummary, Stage: ievent.StageDone, URL: "u", ItemRef: "i",
		Written: 1, Total: 2, Attempt: 3, Reason: "r", Detail: "d",
		Err:     errors.New("e"),
		Summary: &ievent.Counts{Discovered: 4, Succeeded: 5, Failed: 6, Skipped: 7},
	}
	got := fromEvent(in)
	if got.Kind != EventSummary || got.Stage != StageDone || got.Written != 1 || got.Total != 2 {
		t.Fatalf("event mapping = %+v", got)
	}
	if got.Summary == nil || got.Summary.Discovered != 4 || got.Summary.Skipped != 7 {
		t.Fatalf("summary mapping = %+v", got.Summary)
	}
}

func TestWarningCollectorAndCountingReporter(t *testing.T) {
	var forwarded int
	wc := &warningCollector{}
	sink := wc.wrap(SinkFunc(func(context.Context, Event) { forwarded++ }))
	sink.Emit(context.Background(), Event{Kind: EventWarning, Reason: "ffmpeg_missing"})
	sink.Emit(context.Background(), Event{Kind: EventWarning, Detail: "detail"})
	sink.Emit(context.Background(), Event{Kind: EventProgress, Written: 1})
	if got := wc.list(); len(got) != 2 || got[0] != "ffmpeg_missing" || got[1] != "detail" {
		t.Fatalf("warnings = %v", got)
	}
	if forwarded != 3 {
		t.Fatalf("forwarded = %d, want 3", forwarded)
	}

	cr := &countingReporter{}
	cr.Queue("u", "input")
	cr.Success("u")
	cr.Failure("v", errors.New("x"))
	cr.Skip("w", "dup")
	if c := cr.snapshot(); c.Discovered != 1 || c.Succeeded != 1 || c.Failed != 1 || c.Skipped != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestSinkNilBecomesNop(t *testing.T) {
	e := &Engine{}
	s, closeFn := e.sink(nil)
	s.Emit(context.Background(), ievent.Event{Kind: ievent.KindProgress})
	closeFn()
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
