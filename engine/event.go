package engine

import (
	"context"
	"time"
)

// Stage is the coarse phase of an operation.
type Stage string

const (
	StageResolving   Stage = "resolving"
	StageDownloading Stage = "downloading"
	StageProcessing  Stage = "processing"
	StageUploading   Stage = "uploading"
	StageDone        Stage = "done"
)

// EventKind is the type of a structured Event.
type EventKind string

const (
	EventStageChanged EventKind = "stage_changed"
	EventProgress     EventKind = "progress"
	EventWarning      EventKind = "warning"
	EventItemDone     EventKind = "item_done"
	EventRetry        EventKind = "retry"
	EventSummary      EventKind = "summary"
)

// Event is a single structured execution event.
//
// Reason is a stable machine-readable code; Detail is an optional,
// non-localized human hint whose wording callers must not depend on. Err is the
// categorized failure for a failed EventItemDone. Summary is set only on
// EventSummary.
type Event struct {
	Kind    EventKind
	Stage   Stage
	URL     string
	ItemRef string
	Written int64
	Total   int64
	Attempt int
	Reason  string
	Detail  string
	Err     error
	Summary *Counts
	At      time.Time
}

// EventSink receives structured events.
//
// Contract:
//   - Emit must be best-effort and must not block the engine for long periods.
//   - Emit must not influence or cancel the operation.
//   - Emit may be called concurrently from worker goroutines.
//   - Implementations may be handed events that were dropped under
//     backpressure if they cannot keep up.
type EventSink interface {
	Emit(ctx context.Context, e Event)
}

type nopSink struct{}

func (nopSink) Emit(context.Context, Event) {}

// NopSink returns a sink that discards every event. A nil EventSink is
// equivalent and safe.
func NopSink() EventSink { return nopSink{} }

type sinkFunc func(context.Context, Event)

func (f sinkFunc) Emit(ctx context.Context, e Event) { f(ctx, e) }

// SinkFunc adapts a function to an EventSink. A nil function yields NopSink.
func SinkFunc(fn func(context.Context, Event)) EventSink {
	if fn == nil {
		return NopSink()
	}
	return sinkFunc(fn)
}
