// Package render turns structured engine events into stable CLI text. It is a
// presentation adapter: terminal formatting lives here, not in the reusable
// engine packages.
package render

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/sk3y04/provenance-engine/internal/diagnose"
	"github.com/sk3y04/provenance-engine/internal/event"
)

// Sink writes event lines to w. It is safe for concurrent use.
type Sink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSink returns a CLI event sink writing to w.
func NewSink(w io.Writer) *Sink { return &Sink{w: w} }

// Emit renders e. Progress events are intentionally not rendered (the CLI
// keeps its per-file bars until it migrates to the facade in Phase 5); item,
// stage, warning, retry, and summary events carry the useful facts.
func (s *Sink) Emit(_ context.Context, e event.Event) {
	if s == nil || s.w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case event.KindStageChanged:
		_, _ = fmt.Fprintf(s.w, "[provenance] %s: %s\n", e.Stage, e.URL)
	case event.KindWarning:
		msg := e.Detail
		if msg == "" {
			msg = e.Reason
		}
		_, _ = fmt.Fprintf(s.w, "[provenance] WARNING: %s\n", msg)
	case event.KindRetry:
		_, _ = fmt.Fprintf(s.w, "[provenance] retry %d for %s\n", e.Attempt, e.URL)
	case event.KindItemDone:
		if e.Err == nil {
			_, _ = fmt.Fprintf(s.w, "[provenance] OK: %s\n", e.URL)
			return
		}
		_, _ = fmt.Fprintf(s.w, "[provenance] FAILED %s: %v\n", e.URL, e.Err)
		if hint := diagnose.Hint(e.Err); hint != "" {
			_, _ = fmt.Fprintf(s.w, "[provenance] hint: %s\n", hint)
		}
	case event.KindSummary:
		if e.Summary == nil {
			return
		}
		_, _ = fmt.Fprintf(s.w, "\ndiscovered: %d\n", e.Summary.Discovered)
		_, _ = fmt.Fprintf(s.w, "downloaded: %d\n", e.Summary.Succeeded)
		_, _ = fmt.Fprintf(s.w, "skipped:    %d\n", e.Summary.Skipped)
		_, _ = fmt.Fprintf(s.w, "failed:     %d\n", e.Summary.Failed)
	}
}
