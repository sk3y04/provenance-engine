package extractor

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/sk3y04/provenance-engine/internal/downloader"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/redact"
)

// legacyPrefixPattern matches the tool prefix used by historical CLI narration,
// so the machine-readable Detail can be derived without the presentation text.
var legacyPrefixPattern = regexp.MustCompile(`^\[[^\]]+\] `)

// Short aliases keep narration call sites readable without importing the event
// package in every extractor file.
const (
	kindInfo   = event.KindInfo
	kindWarn   = event.KindWarning
	kindRetry  = event.KindRetry
	kindItem   = event.KindItemDone
	kindStage  = event.KindStageChanged
	reasonInfo = event.ReasonInfo
)

// narrate emits a structured event when the operation carries an event sink on
// ctx, and otherwise preserves the legacy CLI narration byte-for-byte on w.
//
// fact is the stable, redactable value an event may carry. When it is empty the
// detail is derived from the legacy line with its tool prefix removed. Callers
// that would otherwise put a raw response body in an event must pass a stable
// fact (for example a status code) instead.
func narrate(ctx context.Context, w io.Writer, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	line := fmt.Sprintf(legacyFormat, args...)
	detail := fact
	if detail == "" {
		detail = legacyPrefixPattern.ReplaceAllString(line, "")
	}
	if sink := event.SinkFrom(ctx); sink != nil {
		event.Emit(ctx, sink, event.Event{
			Kind:   kind,
			URL:    redact.URL(url),
			Reason: reason,
			Detail: redact.Detail(detail),
		})
		return
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, legacyFormat, args...)
	}
}

// narratef narrates to the legacy stderr stream.
func narratef(ctx context.Context, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	narrate(ctx, os.Stderr, kind, reason, url, fact, legacyFormat, args...)
}

// narrateOut narrates to the legacy stdout stream.
func narrateOut(ctx context.Context, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	narrate(ctx, os.Stdout, kind, reason, url, fact, legacyFormat, args...)
}

// sleepCtx waits d unless ctx is done first. Retry and backoff paths must be
// cancellable, otherwise a canceled operation keeps burning a platform quota.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		time.Sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maxRetryAfter caps server-supplied Retry-After delays so a hostile or broken
// platform response cannot stall an operation for hours. The wait itself stays
// cancellable through sleepCtx.
const maxRetryAfter = 5 * time.Minute

// retryAfterDelay picks the wait before retrying a rate-limited request: the
// server's Retry-After seconds when positive, capped at maxRetryAfter, else
// fallback. capped reports whether the server asked for more than the cap.
func retryAfterDelay(header string, fallback time.Duration) (delay time.Duration, capped bool) {
	sec, err := strconv.Atoi(header)
	if err != nil || sec <= 0 {
		return fallback, false
	}
	if sec > int(maxRetryAfter/time.Second) {
		return maxRetryAfter, true
	}
	return time.Duration(sec) * time.Second, false
}

// progressFor returns the reporter a download should use. An explicit reporter
// wins; otherwise an event-backed reporter is used when the operation carries a
// sink, so a non-terminal caller never falls back to the terminal progressbar.
func progressFor(ctx context.Context, rep downloader.ProgressReporter) downloader.ProgressReporter {
	if rep != nil {
		return rep
	}
	if sink := event.SinkFrom(ctx); sink != nil {
		return newEventProgress(ctx, sink)
	}
	return nil
}
