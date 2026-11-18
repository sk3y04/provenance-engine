package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/extractor"
)

// TestWriteLinkCacheSanitizesHost: the cache file name embeds the source
// host, which may carry a port or IPv6 brackets. Those characters are not
// portable filename characters (':' is invalid on Windows), so the host
// must be sanitized like the path tail already is.
func TestWriteLinkCacheSanitizesHost(t *testing.T) {
	for _, src := range []string{
		"https://example.com:8080/list/abc",
		"https://[2001:db8::1]:443/list/abc",
	} {
		dir := t.TempDir()
		path, err := writeLinkCache(dir, src, []string{"https://v.example/1"})
		if err != nil {
			t.Fatalf("writeLinkCache(%q): %v", src, err)
		}
		base := filepath.Base(path)
		if strings.ContainsAny(base, ":[]") {
			t.Errorf("cache file name %q contains unsafe host characters", base)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read cache: %v", err)
		}
		if !strings.Contains(string(data), "https://v.example/1") {
			t.Errorf("cache file missing the link: %q", data)
		}
	}
}

// TestBatchYtOptsPropagatesRuntimeFields: the batch/crawler path must hand
// yt-dlp the same runtime fields as the single-URL Dispatch path, or
// event-sink transports lose structured progress and auto-install.
func TestBatchYtOptsPropagatesRuntimeFields(t *testing.T) {
	var received []event.Event
	sink := event.Func(func(_ context.Context, e event.Event) { received = append(received, e) })
	opts := Options{
		Config: config.Config{OutputDir: "out", Quality: "best"},
		Events: sink,
	}
	d := &extractor.Deps{AutoInstall: true}

	got := batchYtOpts(d, opts, "/tmp/archive.txt")
	if got.Events == nil {
		t.Fatal("Events sink not propagated to batch yt-dlp options")
	}
	// Func sinks are uncomparable; prove identity by emitting through the
	// propagated sink and observing the original one.
	got.Events.Emit(context.Background(), event.Event{Kind: event.KindWarning})
	if len(received) != 1 {
		t.Error("propagated sink does not reach the original sink")
	}
	if !got.AutoInstall {
		t.Error("AutoInstall not propagated to batch yt-dlp options")
	}
	if got.DownloadArchive != "/tmp/archive.txt" {
		t.Errorf("DownloadArchive = %q, want /tmp/archive.txt", got.DownloadArchive)
	}
	if got.OutputDir != "out" || got.Quality != "best" {
		t.Errorf("config fields lost: OutputDir=%q Quality=%q", got.OutputDir, got.Quality)
	}
}
