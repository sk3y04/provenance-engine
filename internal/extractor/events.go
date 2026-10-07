package extractor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sk3y04/provenance-engine/internal/downloader"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
)

// eventProgress adapts an event.Sink to downloader.ProgressReporter so the
// existing yt-dlp/HTTP progress machinery can feed structured events.
type eventProgress struct {
	sink event.Sink
}

func newEventProgress(sink event.Sink) downloader.ProgressReporter {
	return &eventProgress{sink: sink}
}

func (p *eventProgress) OnStart(url, dest string, total int64) {
	event.Emit(context.Background(), p.sink, event.Event{
		Kind: event.KindProgress, Stage: event.StageDownloading,
		URL: url, ItemRef: dest, Total: total, Reason: "start",
	})
}

func (p *eventProgress) OnProgress(url string, written, total int64) {
	event.Emit(context.Background(), p.sink, event.Event{
		Kind: event.KindProgress, Stage: event.StageDownloading,
		URL: url, Written: written, Total: total,
	})
}

func (p *eventProgress) OnDone(url string, err error) {
	event.Emit(context.Background(), p.sink, event.Event{
		Kind: event.KindItemDone, Stage: event.StageDownloading,
		URL: url, Err: err,
	})
}

// classifyYtdlpError categorizes a yt-dlp subprocess failure using the Go error
// and captured stderr, without callers matching strings.
func classifyYtdlpError(rawURL string, err error, stderr string) error {
	if err == nil {
		return nil
	}
	// Preserve the historical "yt-dlp: ..." message while categorizing it.
	cause := fmt.Errorf("yt-dlp: %w", err)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return engineerr.New(engineerr.Canceled, "yt-dlp", rawURL, cause)
	}
	h := strings.ToLower(cause.Error() + "\n" + stderr)
	switch {
	case containsAnySub(h, "unsupported url", "no suitable extractor", "unable to extract", "is not a valid url"):
		return engineerr.New(engineerr.UnsupportedSource, "yt-dlp", rawURL, cause)
	case containsAnySub(h, "http error 401", "http error 403", "forbidden", "unauthorized",
		"login required", "private video", "members-only", "sign in to confirm", "cookies"):
		return engineerr.New(engineerr.AuthRequired, "yt-dlp", rawURL, cause)
	case containsAnySub(h, "http error 429", "too many requests", "rate limit", "temporarily blocked"):
		return engineerr.New(engineerr.RateLimited, "yt-dlp", rawURL, cause)
	case containsAnySub(h, "http error 404", "http error 410", "not found", "gone",
		"video unavailable", "this video is unavailable"):
		return engineerr.New(engineerr.Permanent, "yt-dlp", rawURL, cause)
	default:
		return engineerr.New(engineerr.ExternalTool, "yt-dlp", rawURL, cause)
	}
}

func containsAnySub(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
