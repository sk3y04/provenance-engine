package torrentqueue

// The runner is the only thing that moves entries forward. It is a foreground
// loop: it drains the queue once and returns, which is what makes the lifecycle
// observable and testable without a daemon.
//
// Locks, and what each one is for:
//
//	runner.lock   one runner per queue, held for the whole run (flock, so process
//	              death releases it; a held lock means a live runner, not a stale
//	              file)
//	store lock    short read-modify-write of one entry record, via Store.Update
//	workspace     the adapter's own lock, taken per job while it transfers
//
// The runner never holds the store lock while awaiting peers, transferring,
// hashing or closing a client, and it never takes a workspace lock itself, so
// there is no lock order between the three that could deadlock with `list`,
// `status` or `recheck` (which take none of them, or only the workspace lock).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sk3y04/provenance-engine/internal/filelock"
)

var (
	// ErrStoreBusy means another process holds the queue store lock.
	ErrStoreBusy = errors.New("torrent queue store is busy")
	// ErrEntryNotFound means no entry has that id.
	ErrEntryNotFound = errors.New("torrent queue entry not found")
	// ErrEntryFormat means a record could not be understood.
	ErrEntryFormat = errors.New("unrecognized torrent queue entry format")
	// ErrRunnerActive means another runner owns this queue.
	ErrRunnerActive = errors.New("another runner owns this queue")
	// ErrWorkspaceBusy is a retryable condition: the job's workspace is owned by
	// another process, so this entry is deferred with a bounded backoff.
	ErrWorkspaceBusy = errors.New("job workspace is busy")
)

// Request is one unit of work handed to the transfer backend.
type Request struct {
	// Root is the job root (the queue's root), absolute.
	Root string
	// Source is the raw source to resolve. For an unresolved entry it may be the
	// restricted original (trackers, peer hints); it never reaches a record.
	Source string
	// Peers are explicit peers from the restricted record.
	Peers []string
	// Selection is the immutable capture intent of the entry.
	Selection []string
	// Discovery is the profile the entry was created with. A restart uses the
	// recorded profile; there is no fallback to public.
	Discovery string
	// KnownJobID, when set, means the identity is already resolved and the backend
	// must resume the workspace offline rather than resolve over the network.
	KnownJobID string
}

// Outcome is what a backend reports after a successful run.
type Outcome struct {
	JobID      string
	InfoHash   string
	InfoHashV2 string
	// Workspace is relative to the job root.
	Workspace    string
	CaptureKey   string
	ManifestPath string
	Reconciled   bool
}

// Transfer is the worker backend. The real one is Adapter (the bittorrent
// adapter); tests use a fake with barriers.
type Transfer interface {
	// Run moves one entry forward: resolve if needed, transfer, capture.
	Run(ctx context.Context, req Request) (Outcome, error)
	// Adopt reports an already published capture for this job and selection
	// without starting a transfer. It reads records only.
	Adopt(root, jobID string, selection []string) (CaptureRef, bool)
}

// Options tunes the runner. Zero values are production defaults.
type Options struct {
	// PollInterval is how often pause intent is re-read while a job runs.
	PollInterval time.Duration
	// Backoff is how long a blocked entry waits before it may be claimed again.
	Backoff time.Duration
	// Log receives one line per transition. Nil discards.
	Log io.Writer
}

func (o Options) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 200 * time.Millisecond
}

func (o Options) backoff() time.Duration {
	if o.Backoff > 0 {
		return o.Backoff
	}
	return 5 * time.Second
}

// Runner drains a queue with one worker.
type Runner struct {
	store    *Store
	transfer Transfer
	opts     Options
}

// NewRunner builds a runner over a store and a backend.
func NewRunner(store *Store, transfer Transfer, opts Options) *Runner {
	return &Runner{store: store, transfer: transfer, opts: opts}
}

// Summary reports what one drain did.
type Summary struct {
	// Ran lists entry ids in the order they were claimed.
	Ran       []string
	Completed int
	Failed    int
	Blocked   int
	Paused    int
	// Remaining counts entries that are still runnable or blocked after the drain.
	Remaining int
}

// Run drains the queue: reconcile first, then claim and run entries in queue order
// until nothing runnable is left or ctx is cancelled. Cancelling ctx (SIGINT) stops
// the active job, records it as interrupted-by-shutdown, and preserves every
// entry's desired state.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	l, err := filelock.TryAcquire(filepath.Join(r.store.root, filepath.FromSlash(queueDirName), "runner"))
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return sum, fmt.Errorf("%w: %s is already being drained", ErrRunnerActive, r.store.root)
		}
		return sum, err
	}
	defer func() { _ = l.Release() }()

	adopted, err := r.reconcile()
	if err != nil {
		return sum, err
	}
	sum.Completed += adopted
	for {
		if ctx.Err() != nil {
			return sum, nil
		}
		entry, ok, nerr := r.next()
		if nerr != nil {
			return sum, nerr
		}
		if !ok {
			return sum, nil
		}
		sum.Ran = append(sum.Ran, entry.ID)
		r.runEntry(ctx, entry, &sum)
	}
}

// next returns the first runnable entry in deterministic queue order.
func (r *Runner) next() (Entry, bool, error) {
	entries, err := r.store.List()
	if err != nil {
		return Entry{}, false, err
	}
	now := time.Now()
	for _, e := range entries {
		// Completed work is done; a failed entry needs an explicit retry, so it is
		// never re-claimed automatically. Both rules keep a drain from looping on
		// one job while later entries wait behind it.
		if e.Desired != DesiredRun || e.Phase == PhaseCompleted || e.Phase == PhaseFailed {
			continue
		}
		if e.Phase == PhaseBlocked && !e.NotBefore.IsZero() && now.Before(e.NotBefore) {
			continue
		}
		return e, true, nil
	}
	return Entry{}, false, nil
}

// reconcile brings recorded observations back in line with reality after a
// restart: adopt a capture that was published before the queue heard about it,
// and turn an interrupted phase back into something the intent allows.
func (r *Runner) reconcile() (int, error) {
	entries, err := r.store.List()
	if err != nil {
		return 0, err
	}
	adopted := 0
	for _, e := range entries {
		if e.JobID != "" && e.Phase != PhaseCompleted {
			if ref, ok := r.transfer.Adopt(r.store.root, e.JobID, e.Selection); ok {
				if _, uerr := r.store.Update(e.ID, func(x *Entry) error {
					x.Capture = &ref
					x.Phase = PhaseCompleted
					x.LastError = ""
					x.Note = "adopted a capture published before the queue recorded it"
					return nil
				}); uerr != nil {
					return 0, uerr
				}
				adopted++
				r.logf("%s: adopted existing capture %s", e.ID, ref.Key)
				continue
			}
		}
		switch e.Phase {
		case PhaseRunning, PhaseStopping, PhaseResolving:
			phase := PhaseQueued
			if e.Desired == DesiredPaused {
				phase = PhasePaused
			}
			if _, uerr := r.store.Update(e.ID, func(x *Entry) error {
				x.Phase = phase
				x.Note = "interrupted while the runner was not running; desired state kept"
				return nil
			}); uerr != nil {
				return 0, uerr
			}
			r.logf("%s: interrupted -> %s (desired %s)", e.ID, phase, e.Desired)
		}
	}
	return adopted, nil
}

// runEntry executes one entry and records the outcome. It never returns an error:
// a failed entry is a recorded state, not a reason to stop draining, so one bad
// job cannot block the ones behind it.
func (r *Runner) runEntry(ctx context.Context, entry Entry, sum *Summary) {
	entry, err := r.store.Update(entry.ID, func(e *Entry) error {
		if e.Desired == DesiredPaused {
			return errors.New("entry was paused before it started")
		}
		e.Phase = PhaseRunning
		e.Attempts++
		e.Note = ""
		e.LastError = ""
		return nil
	})
	if err != nil {
		// Paused between selection and claim: nothing ran, nothing to report.
		sum.Paused++
		return
	}
	r.logf("%s: running (%s)", entry.ID, entry.JobID)

	raw, peers, rerr := r.store.RestrictedSource(entry.ID)
	if rerr != nil {
		raw = entry.Source.Magnet
		if raw == "" {
			raw = entry.Source.Path
		}
	}

	req := Request{
		Root:       r.store.root,
		Source:     raw,
		Peers:      peers,
		Selection:  entry.Selection,
		Discovery:  entry.Discovery,
		KnownJobID: entry.JobID,
	}

	runCtx, cancel := context.WithCancel(ctx)
	var paused syncFlag
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.watchPause(runCtx, entry.ID, cancel, &paused)
	}()

	out, runErr := r.transfer.Run(runCtx, req)
	cancel()
	wg.Wait()

	if runErr == nil {
		r.finishCompleted(entry.ID, out, sum)
		return
	}

	// A single wipe: an entry that failed after its capture was published must be
	// adopted, never refetched.
	if out.JobID != "" || entry.JobID != "" {
		jobID := out.JobID
		if jobID == "" {
			jobID = entry.JobID
		}
		if ref, ok := r.transfer.Adopt(r.store.root, jobID, entry.Selection); ok {
			if _, uerr := r.store.Update(entry.ID, func(e *Entry) error {
				e.JobID = jobID
				e.Capture = &ref
				e.Phase = PhaseCompleted
				e.Unresolved = false
				if out.Workspace != "" {
					e.Workspace = out.Workspace
				}
				e.Note = "capture was already published when this attempt failed; adopted it instead of refetching"
				e.LastError = ""
				return nil
			}); uerr != nil {
				sum.Failed++
				return
			}
			sum.Completed++
			r.logf("%s: adopted capture after failure: %s", entry.ID, ref.Key)
			return
		}
	}

	reason := SanitizeError(runErr)
	switch {
	case paused.load():
		// The user asked for this. Record it as a pause, not a failure.
		if _, uerr := r.store.Update(entry.ID, func(e *Entry) error {
			e.Phase = PhasePaused
			e.Note = "paused at request; the client closed and the workspace was released"
			return nil
		}); uerr == nil {
			sum.Paused++
			r.logf("%s: paused", entry.ID)
		} else {
			sum.Failed++
		}
	case errors.Is(runErr, context.Canceled) && ctx.Err() != nil:
		// Runner shutdown. Desired state is the user's intent and is untouched, so
		// the next runner resumes instead of reading this as a pause.
		if _, uerr := r.store.Update(entry.ID, func(e *Entry) error {
			e.Phase = PhaseQueued
			if e.Desired == DesiredPaused {
				e.Phase = PhasePaused
			}
			e.Note = "interrupted by runner shutdown; intent preserved"
			return nil
		}); uerr != nil {
			sum.Failed++
		}
	case errors.Is(runErr, ErrWorkspaceBusy):
		backoff := r.opts.backoff()
		if _, uerr := r.store.Update(entry.ID, func(e *Entry) error {
			e.Phase = PhaseBlocked
			e.LastError = reason
			e.NotBefore = time.Now().UTC().Add(backoff)
			e.Note = fmt.Sprintf("retryable: workspace busy; retry after %s or run the queue again", backoff)
			return nil
		}); uerr == nil {
			sum.Blocked++
			r.logf("%s: blocked (%s)", entry.ID, reason)
		} else {
			sum.Failed++
		}
	default:
		if _, uerr := r.store.Update(entry.ID, func(e *Entry) error {
			e.Phase = PhaseFailed
			e.LastError = reason
			e.Note = "failed; fix the cause and run `torrent queue retry` to try again"
			return nil
		}); uerr == nil {
			sum.Failed++
			r.logf("%s: failed (%s)", entry.ID, reason)
		} else {
			sum.Failed++
		}
	}
}

// finishCompleted records a successful run, attaching the canonical identity only
// now that the backend has validated it.
func (r *Runner) finishCompleted(id string, out Outcome, sum *Summary) {
	if _, err := r.store.Update(id, func(e *Entry) error {
		e.JobID = out.JobID
		e.InfoHash = out.InfoHash
		e.InfoHashV2 = out.InfoHashV2
		e.Workspace = out.Workspace
		e.Unresolved = false
		e.Phase = PhaseCompleted
		e.LastError = ""
		e.Capture = &CaptureRef{Key: out.CaptureKey, Manifest: out.ManifestPath, Reconciled: out.Reconciled}
		if e.Desired == DesiredPaused {
			e.Note = "completed; a pause arrived while the capture was being committed, so the capture was kept and the entry stays paused"
		} else {
			e.Note = ""
		}
		return nil
	}); err != nil {
		sum.Failed++
		return
	}
	sum.Completed++
	r.logf("%s: completed (%s)", id, out.CaptureKey)
}

// watchPause polls recorded intent while a job runs. A pause request switches the
// entry to "stopping" (the client may still be closing) and cancels the worker;
// the phase only becomes "paused" once Run has returned and the workspace lock is
// released.
func (r *Runner) watchPause(ctx context.Context, id string, cancel context.CancelFunc, paused *syncFlag) {
	ticker := time.NewTicker(r.opts.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entry, err := r.store.Get(id)
			if err != nil || entry.Desired != DesiredPaused {
				continue
			}
			paused.set(true)
			if _, uerr := r.store.Update(id, func(e *Entry) error {
				if e.Phase == PhaseRunning {
					e.Phase = PhaseStopping
					e.Note = "pause requested; waiting for the client to close"
				}
				return nil
			}); uerr == nil {
				r.logf("%s: stopping (pause requested)", id)
			}
			cancel()
			return
		}
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.opts.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(r.opts.Log, format+"\n", args...)
}

// syncFlag is a tiny atomic bool: the runner only needs "was this cancelled by a
// pause request".
type syncFlag struct {
	mu sync.Mutex
	v  bool
}

func (f *syncFlag) set(v bool) {
	f.mu.Lock()
	f.v = v
	f.mu.Unlock()
}

func (f *syncFlag) load() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

// SanitizeError makes a backend error safe to persist: no absolute paths, no
// tracker URLs, no credentials, one line.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	if i := strings.Index(msg, "magnet:?"); i >= 0 {
		msg = msg[:i] + "magnet:?<redacted>"
	}
	lines := strings.Fields(msg)
	out := make([]string, 0, len(lines))
	for _, tok := range lines {
		if strings.Contains(tok, "://") || strings.Contains(tok, "passkey") {
			continue
		}
		if filepath.IsAbs(tok) {
			tok = filepath.Base(tok)
		}
		out = append(out, tok)
	}
	joined := strings.Join(out, " ")
	if len(joined) > 300 {
		joined = joined[:300] + "…"
	}
	return joined
}

// EntryLabel is a short, printable description of where an entry stands. It is
// recorded state only, never a liveness claim.
func EntryLabel(e Entry) string {
	switch e.Phase {
	case PhaseCompleted:
		return "completed"
	case PhaseFailed:
		return "failed"
	case PhaseBlocked:
		return "blocked"
	case PhasePaused:
		return "paused"
	case PhaseRunning:
		return "running (recorded)"
	case PhaseStopping:
		return "stopping (recorded)"
	case PhaseResolving:
		return "resolving (recorded)"
	default:
		if e.Unresolved {
			return "queued (unresolved)"
		}
		return "queued"
	}
}

// RunnerHeld reports whether some process currently holds the runner lock. A held
// flock means a live owner on any platform this package supports, because the OS
// releases it on process exit.
func RunnerHeld(root string) bool {
	l, err := filelock.TryAcquire(filepath.Join(root, filepath.FromSlash(queueDirName), "runner"))
	if err != nil {
		return errors.Is(err, filelock.ErrLocked)
	}
	_ = l.Release()
	return false
}

// ensure os is used even when the file is trimmed by build tags in the future.
var _ = os.Getenv
