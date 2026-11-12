package encode

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/redact"
)

// legacyPrefixPattern matches the tool prefix used by historical CLI
// narration, so the machine-readable Detail can be derived without the
// presentation text.
var legacyPrefixPattern = regexp.MustCompile(`^\[[^\]]+\] `)

// Short aliases keep narration call sites readable.
const (
	kindEncodeStart = event.KindStageChanged
	kindEncodeInfo  = event.KindInfo
	kindEncodeWarn  = event.KindWarning
	kindEncodeItem  = event.KindItemDone
)

// narrateMu serializes legacy terminal writes; worker hooks and dry-run lines
// are emitted from multiple goroutines.
var narrateMu sync.Mutex

// narrate delivers one line of encode narration on exactly one transport.
// When the operation's context carries an event sink, the structured event is
// emitted and nothing is written to the terminal. Otherwise the legacy
// [provenance] text is written to w byte-for-byte (a nil w silences it).
//
// The legacy format must not carry a trailing newline; one is appended. When
// fact is empty the event Detail is derived from the legacy line with its tool
// prefix removed, and is always run through redact.Detail.
func narrate(ctx context.Context, w io.Writer, kind event.Kind, reason, itemRef, fact string, err error, legacyFormat string, args ...any) {
	line := fmt.Sprintf(legacyFormat, args...)
	if sink := event.SinkFrom(ctx); sink != nil {
		detail := fact
		if detail == "" {
			detail = legacyPrefixPattern.ReplaceAllString(line, "")
		}
		e := event.Event{
			Kind:    kind,
			Reason:  reason,
			Detail:  redact.Detail(detail),
			ItemRef: itemRef,
			Err:     err,
		}
		if kind == event.KindStageChanged {
			e.Stage = event.StageProcessing
		}
		event.Emit(ctx, sink, e)
		return
	}
	if w != nil {
		narrateMu.Lock()
		defer narrateMu.Unlock()
		_, _ = fmt.Fprintln(w, line)
	}
}

// narrateSummary emits the terminal tally as a KindSummary event (carrying the
// structured counts) when a sink is present, and otherwise preserves the
// legacy summary line on stderr byte-for-byte.
func narrateSummary(ctx context.Context, total, succeeded, skipped, failed int, outDir string, durRound string) {
	if sink := event.SinkFrom(ctx); sink != nil {
		detail := fmt.Sprintf("done in %s — success: %d, skipped: %d, failed: %d, output: %s",
			durRound, succeeded, skipped, failed, outDir)
		event.Emit(ctx, sink, event.Event{
			Kind:   event.KindSummary,
			Reason: event.ReasonInfo,
			Detail: redact.Detail(detail),
			Summary: &event.Counts{
				Discovered: int64(total),
				Succeeded:  int64(succeeded),
				Failed:     int64(failed),
				Skipped:    int64(skipped),
			},
		})
		return
	}
	narrateMu.Lock()
	defer narrateMu.Unlock()
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] done in %s — success: %d, skipped: %d, failed: %d, output: %s\n",
		durRound, succeeded, skipped, failed, outDir)
}
