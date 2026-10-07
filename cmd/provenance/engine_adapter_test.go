package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/engine"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// saveGrabFlags snapshots the package-level grab flags and restores them after
// the test, since these tests mutate globals.
func saveGrabFlags(t *testing.T) {
	t.Helper()
	oldDryRun := flagDryRun
	oldBatch := flagBatch
	oldSession := flagSession
	oldTemplate := flagOutputTemplate
	oldQuality := flagQuality
	t.Cleanup(func() {
		flagDryRun = oldDryRun
		flagBatch = oldBatch
		flagSession = oldSession
		flagOutputTemplate = oldTemplate
		flagQuality = oldQuality
	})
}

func TestFacadeGrabCompatible(t *testing.T) {
	saveGrabFlags(t)

	cases := []struct {
		name string
		set  func()
		args []string
		want bool
	}{
		{
			name: "https url",
			set:  func() {},
			args: []string{"https://example.com/a"},
			want: true,
		},
		{
			name: "http url",
			set:  func() {},
			args: []string{"http://example.com/a"},
			want: true,
		},
		{
			name: "multiple https urls",
			set:  func() {},
			args: []string{"https://example.com/a", "https://example.com/b"},
			want: true,
		},
		{
			name: "dry run",
			set:  func() { flagDryRun = true },
			args: []string{"https://example.com/a"},
			want: false,
		},
		{
			name: "batch file",
			set:  func() { flagBatch = "urls.txt" },
			args: []string{"https://example.com/a"},
			want: false,
		},
		{
			name: "session",
			set:  func() { flagSession = "job1" },
			args: []string{"https://example.com/a"},
			want: false,
		},
		{
			name: "filename template",
			set:  func() { flagOutputTemplate = "%(id)s.%(ext)s" },
			args: []string{"https://example.com/a"},
			want: false,
		},
		{
			name: "non-standard quality",
			set:  func() { flagQuality = "720p" },
			args: []string{"https://example.com/a"},
			want: false,
		},
		{
			name: "standard quality",
			set:  func() { flagQuality = "1080" },
			args: []string{"https://example.com/a"},
			want: true,
		},
		{
			name: "bare host falls back",
			set:  func() {},
			args: []string{"example.com/a.mp4"},
			want: false,
		},
		{
			name: "mixed sources fall back",
			set:  func() {},
			args: []string{"https://example.com/a", "example.com/b"},
			want: false,
		},
		{
			name: "no args",
			set:  func() {},
			args: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flagDryRun = false
			flagBatch = ""
			flagSession = ""
			flagOutputTemplate = ""
			flagQuality = "best"
			tc.set()
			if got := facadeGrabCompatible(tc.args); got != tc.want {
				t.Fatalf("facadeGrabCompatible(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestFacadeURLHashtagSource(t *testing.T) {
	if !facadeURL("#cats") && !facadeURL("https://x.com/hashtag/cats") {
		t.Fatal("expected at least one hashtag spelling accepted")
	}
}

func TestEngineConfigFromOptionsMapping(t *testing.T) {
	opts := dispatcher.Options{
		DryRun: true,
	}
	opts.OutputDir = "/tmp/out"
	opts.CookiesFile = "/tmp/cookies.txt"
	opts.CookiesFromBrowser = "chrome"
	opts.Quality = "720"
	opts.AudioOnly = true
	opts.Concurrency = 2
	opts.SpeedLimit = 4096
	opts.NoArchive = true
	opts.OutputLayout = "creator"
	opts.ChromePath = "/usr/bin/chrome"
	opts.PostLimit = 25

	cfg := engineConfigFromOptions(opts)

	if cfg.WorkDir != "/tmp/out" ||
		cfg.CookiesFile != "/tmp/cookies.txt" ||
		cfg.CookiesFromBrowser != "chrome" ||
		cfg.Quality != "720" ||
		!cfg.AudioOnly ||
		cfg.Concurrency != 2 ||
		cfg.SpeedLimit != 4096 ||
		!cfg.NoArchive ||
		cfg.OutputLayout != "creator" ||
		cfg.ChromePath != "/usr/bin/chrome" {
		t.Fatalf("unexpected config mapping: %+v", cfg)
	}
	if cfg.MaxItems != 25 {
		t.Fatalf("MaxItems = %d, want 25", cfg.MaxItems)
	}
}

func TestEngineConfigUnlimitedMapsToSentinel(t *testing.T) {
	opts := dispatcher.Options{}
	opts.OutputDir = "/tmp/out"
	opts.Quality = "best"
	cfg := engineConfigFromOptions(opts)
	if cfg.MaxItems != cliUnlimitedItems {
		t.Fatalf("MaxItems = %d, want %d", cfg.MaxItems, cliUnlimitedItems)
	}
}

func TestEngineFilterFromOptionsMapping(t *testing.T) {
	in := manifest.FilterOptions{
		IncludeExt:  []string{"mp4", "jpg"},
		ExcludeExt:  []string{"zip"},
		MinSize:     10,
		MaxSize:     20,
		TitleMatch:  "a.*",
		TitleReject: "b.*",
	}
	got := engineFilterFromOptions(in)
	if strings.Join(got.IncludeExt, ",") != "mp4,jpg" ||
		strings.Join(got.ExcludeExt, ",") != "zip" ||
		got.MinSize != 10 || got.MaxSize != 20 ||
		got.TitleMatch != "a.*" || got.TitleReject != "b.*" {
		t.Fatalf("unexpected filter mapping: %+v", got)
	}
}

func TestCLIEventSinkRendering(t *testing.T) {
	var buf bytes.Buffer
	s := newCLIEventSink(&buf)
	ctx := context.Background()

	s.Emit(ctx, engine.Event{Kind: engine.EventStageChanged, Stage: engine.StageResolving, URL: "u1"})
	s.Emit(ctx, engine.Event{Kind: engine.EventProgress, Written: 5, Total: 10})
	s.Emit(ctx, engine.Event{Kind: engine.EventWarning, Reason: "ffmpeg_missing", Detail: "ffmpeg not found"})
	s.Emit(ctx, engine.Event{Kind: engine.EventRetry, Attempt: 2, URL: "u1"})
	s.Emit(ctx, engine.Event{Kind: engine.EventItemDone, URL: "u1"})
	s.Emit(ctx, engine.Event{Kind: engine.EventItemDone, URL: "u2", Err: errors.New("boom")})
	s.Emit(ctx, engine.Event{Kind: engine.EventSummary, Summary: &engine.Counts{Discovered: 2, Succeeded: 1, Failed: 1}})

	want := "[provenance] resolving: u1\n" +
		"[provenance] WARNING: ffmpeg not found\n" +
		"[provenance] retry 2 for u1\n" +
		"[provenance] OK: u1\n" +
		"[provenance] FAILED u2: boom\n"
	if got := buf.String(); got != want {
		t.Fatalf("rendered output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Fatalf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("boom")); got != 1 {
		t.Fatalf("exitCode(err) = %d, want 1", got)
	}
	if got := exitCode(context.Canceled); got != 1 {
		t.Fatalf("exitCode(canceled) = %d, want 1", got)
	}
}

func TestRunGrabViaFacadeRejectsEmpty(t *testing.T) {
	err := runGrabViaFacade(context.Background(), nil, dispatcher.Options{})
	if err == nil || !strings.Contains(err.Error(), "provide at least one URL") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestRunGrabViaFacadeRejectsInvalidConfig(t *testing.T) {
	opts := dispatcher.Options{}
	opts.OutputDir = "/tmp/out"
	opts.Quality = "480p" // not a facade quality
	err := runGrabViaFacade(context.Background(), []string{"https://example.com/a"}, opts)
	if err == nil || engine.ErrorKindOf(err) != engine.ErrorValidation {
		t.Fatalf("expected validation error, got %v (kind %q)", err, engine.ErrorKindOf(err))
	}
}
