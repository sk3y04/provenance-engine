package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/extractor"
	"github.com/sk3y04/provenance-engine/internal/redact"
)

// legacyPrefixPattern matches the tool prefix used by historical CLI narration,
// so the machine-readable Detail can be derived without the presentation text.
var legacyPrefixPattern = regexp.MustCompile(`^\[[^\]]+\] `)

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

func narratef(ctx context.Context, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	narrate(ctx, os.Stderr, kind, reason, url, fact, legacyFormat, args...)
}

func narrateOut(ctx context.Context, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	narrate(ctx, os.Stdout, kind, reason, url, fact, legacyFormat, args...)
}

// emitEvent delivers a structured event with its URL, Detail, and Err redacted,
// so direct emissions from the reusable paths cannot carry secret-bearing URLs or
// raw response bodies. It is the sanctioned way to emit outside narrate.
func emitEvent(ctx context.Context, sink event.Sink, e event.Event) {
	e.URL = redact.URL(e.URL)
	e.Detail = redact.Detail(e.Detail)
	e.Err = redact.Error(e.Err)
	event.Emit(ctx, sink, e)
}

// depsFor returns the engine-owned Deps, falling back to the shared default.
func depsFor(d *extractor.Deps) *extractor.Deps {
	if d == nil {
		return extractor.Default()
	}
	return d
}
