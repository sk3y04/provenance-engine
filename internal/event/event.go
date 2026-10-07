// Package event defines presentation-neutral, structured execution events and
// the sink used to observe extraction and download progress.
//
// Events carry stable facts (stage, kind, URL, item id, byte counts, attempt,
// reason codes) so any transport - CLI, TUI, or a server-side worker - can
// render or persist them. Sinks receive events; the engine never reads
// anything back from a sink and never lets a sink influence or cancel work.
package event

import (
	"context"
	"sync"
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

// Kind is the event type.
type Kind string

const (
	KindStageChanged Kind = "stage_changed"
	KindProgress     Kind = "progress"
	KindWarning      Kind = "warning"
	KindItemDone     Kind = "item_done"
	KindRetry        Kind = "retry"
	KindSummary      Kind = "summary"
)

// Counts is the terminal tally carried by KindSummary events.
type Counts struct {
	Discovered int64
	Succeeded  int64
	Failed     int64
	Skipped    int64
}

// Event is a single structured execution event.
//
// Reason is a stable machine-readable code (for example "ffmpeg_missing",
// "already_downloaded", "ytdlp_unsupported"). Detail is an optional,
// non-localized human hint; consumers must not depend on its wording. Err is
// the categorized failure for KindItemDone failures. Summary is set only for
// KindSummary.
type Event struct {
	Kind    Kind
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

// Sink receives structured events.
//
// Contract:
//   - Emit is best-effort and MUST NOT block the engine for long periods.
//   - Emit MUST NOT influence or cancel the operation; return values are not
//     used to make progress decisions.
//   - Implementations MAY drop events under backpressure.
//   - Emit may be called concurrently from worker goroutines.
type Sink interface {
	Emit(ctx context.Context, e Event)
}

type nopSink struct{}

func (nopSink) Emit(context.Context, Event) {}

// Nop returns a sink that discards every event. A nil Sink is equivalent.
func Nop() Sink { return nopSink{} }

type funcSink func(context.Context, Event)

func (f funcSink) Emit(ctx context.Context, e Event) { f(ctx, e) }

// Func adapts a function to a Sink. A nil function yields Nop.
func Func(fn func(context.Context, Event)) Sink {
	if fn == nil {
		return Nop()
	}
	return funcSink(fn)
}

// Emit delivers e to sink. It is nil-safe and drops the event if ctx is
// already done.
func Emit(ctx context.Context, sink Sink, e Event) {
	if sink == nil {
		return
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	sink.Emit(ctx, e)
}

// Notifier is the sanctioned wrapper for a potentially slow, sharing, or
// foreign Sink. It delivers events from a single goroutine over a bounded
// buffer, so a slow sink can never stall or deadlock extraction: when the
// buffer is full, events are dropped. Panics from the wrapped sink are
// recovered.
type Notifier struct {
	sink Sink
	ch   chan Event
	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// NewNotifier starts a notifier around sink. A nil sink becomes Nop. A
// non-positive buffer defaults to 64.
func NewNotifier(sink Sink, buffer int) *Notifier {
	if sink == nil {
		sink = Nop()
	}
	if buffer <= 0 {
		buffer = 64
	}
	n := &Notifier{
		sink: sink,
		ch:   make(chan Event, buffer),
		quit: make(chan struct{}),
	}
	n.wg.Add(1)
	go n.run()
	return n
}

func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case <-n.quit:
			for {
				select {
				case e := <-n.ch:
					n.deliver(e)
				default:
					return
				}
			}
		case e := <-n.ch:
			n.deliver(e)
		}
	}
}

func (n *Notifier) deliver(e Event) {
	defer func() { _ = recover() }()
	n.sink.Emit(context.Background(), e)
}

// Emit enqueues e without blocking. The event is dropped when the buffer is
// full, the notifier is closed, or ctx is already done.
func (n *Notifier) Emit(ctx context.Context, e Event) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	select {
	case n.ch <- e:
	default:
	}
}

// Close stops delivery and drains queued events. It is safe to call multiple
// times and safe to call concurrently with Emit.
func (n *Notifier) Close() {
	n.once.Do(func() { close(n.quit) })
	n.wg.Wait()
}
