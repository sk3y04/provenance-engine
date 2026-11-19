package torrentqueue

// The runner is the only thing that moves entries forward. It is a foreground
// loop: it drains the queue once and returns, which is what makes the lifecycle
// observable and testable without a daemon.
//
// Concurrency (BT-08, first slice): one coordinator claims eligible entries in
// queue order and dispatches up to Options.MaxActive attempts. A slot is an active
// attempt - from resolution through the client closing and the workspace lock
// being released - so a slot is never freed early because a pause was requested or
// a capture was published. FIFO applies to claims, not to completion.
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

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
	// ErrUsage marks a caller mistake, such as a seed command for an entry that has
	// not completed a capture.
	ErrUsage = errors.New("invalid queue operation")
	// ErrSelectionNotSelected rejects a priority for a path outside the selection.
	ErrSelectionNotSelected = errors.New("path is not in this entry's selection")
	// ErrWorkspaceBusy is a retryable condition: the job's workspace is owned by
	// another process, so this entry is deferred with a bounded backoff.
	ErrWorkspaceBusy = errors.New("job workspace is busy")
	// errStaleAttempt means the result belongs to an attempt that has been
	// superseded (retried, paused and restarted, or cancelled), so applying it
	// would overwrite a newer user command.
	errStaleAttempt = errors.New("attempt superseded")
)

// Burst sizes for the shared rate limiters. Both are explicit and nonzero: the
// pinned library panics on a zero or too-small burst rather than stalling.
//
//   - download: the library truncates each read to the burst and reserves tokens
//     for it, so the burst must exceed a block read. 256 KiB is far above the
//     16 KiB block size and bounds how much credit an idle attempt can bank.
//   - upload: a peer request is reserved as one lump and the library *panics* when
//     the request is larger than the burst ("upload rate limiter burst size < N").
//     MaxAllocPeerRequestDataPerConn defaults to 1 MiB, so the burst has to cover
//     that, which is also the burst upstream gives itself by default.
const (
	DownloadBurstBytes = 256 << 10
	UploadBurstBytes   = 1 << 20

	// MaxRateBytes is the largest accepted rate per direction, in bytes per second.
	// It exists to reject nonsense and overflow, not to be a meaningful ceiling.
	MaxRateBytes = 1 << 40

	// MaxPeersPerJob is the largest accepted per-attempt peer limit. Each attempt is
	// a full client, so a big number is a big number of sockets, not a budget.
	MaxPeersPerJob = 200
)

// MaxActiveLimit bounds --max-active. Above this a single foreground drain stops
// being a bounded resource user: every attempt costs a client, sockets, a
// completion database and a payload write path.
const MaxActiveLimit = 32

// MaxSeedLimit bounds --max-seeds: each serving seed is a client, a listener and a
// payload write path that runs until the user stops it.
const MaxSeedLimit = 32

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
	// DownloadLimiter and UploadLimiter are the runner's shared budgets. Every
	// attempt of one runner receives the same pointers; nil means unlimited.
	DownloadLimiter *rate.Limiter
	UploadLimiter   *rate.Limiter
	// Priorities carries the entry's download overrides, keyed by selected path.
	// They are applied before payload requests begin and cannot change the
	// selection.
	Priorities map[string]string
	// PeersPerAttempt and HalfOpenPerAttempt are the per-job peer limits; zero
	// keeps the adapter default.
	PeersPerAttempt    int
	HalfOpenPerAttempt int
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
	// MaxActive is the number of concurrent attempts. Zero means the default of 1;
	// negative is invalid and values above MaxActiveLimit are refused.
	MaxActive int
	// DownloadLimit and UploadLimit are the aggregate budgets in bytes per second
	// shared by every attempt this runner starts. Zero means unlimited, which is
	// the previous behaviour. The budget is not divided by MaxActive: a lone
	// attempt may use all of it.
	DownloadLimit int64
	UploadLimit   int64
	// MaxSeeds is the size of the seed-slot pool. Zero (the default) disables
	// managed seeding for this invocation, so an ordinary drain never uploads.
	// Seed slots are separate from MaxActive: a serving seed consumes neither a
	// download slot nor a workspace lock while it waits for one.
	MaxSeeds int
	// PeersPerJob and HalfOpenPerJob are per-attempt limits: established peers per
	// torrent, and outbound handshakes in progress per torrent. Zero keeps the
	// adapter defaults. They are per job, not a runner-wide pool, so the runner's
	// observed bound is MaxActive x PeersPerJob - which holds only because an
	// attempt never holds a resolver and a payload client at the same time.
	PeersPerJob    int
	HalfOpenPerJob int
	// Log receives one line per transition. Nil discards.
	Log io.Writer
	// BeforeSeedClose runs after a seed has recorded stopping and before its client
	// is closed. It exists so a test can hold Close at a barrier; it is nil in
	// production.
	BeforeSeedClose func(entryID string)
	// BeforeResultPersist runs after a backend returns and before its result is
	// persisted. It exists so a test can hold a result at the exact
	// publish-before-queue-update boundary; it is nil in production.
	BeforeResultPersist func(entryID string)
}

// Validate rejects a configuration instead of silently clamping it.
func (o Options) Validate() error {
	if o.MaxSeeds < 0 || o.MaxSeeds > MaxSeedLimit {
		return fmt.Errorf("%w: --max-seeds must be between 1 and %d, got %d", ErrUsage, MaxSeedLimit, o.MaxSeeds)
	}
	if o.MaxActive < 0 {
		return fmt.Errorf("max-active must be at least 1, got %d", o.MaxActive)
	}
	if o.MaxActive > MaxActiveLimit {
		return fmt.Errorf("max-active %d is too high: the limit is %d concurrent attempts in one runner", o.MaxActive, MaxActiveLimit)
	}
	if o.PollInterval < 0 {
		return errors.New("poll interval must not be negative")
	}
	if o.Backoff < 0 {
		return errors.New("backoff must not be negative")
	}
	if o.DownloadLimit < 0 {
		return fmt.Errorf("download rate must not be negative, got %d bytes/s", o.DownloadLimit)
	}
	if o.UploadLimit < 0 {
		return fmt.Errorf("upload rate must not be negative, got %d bytes/s", o.UploadLimit)
	}
	if o.DownloadLimit > MaxRateBytes {
		return fmt.Errorf("download rate %d bytes/s is above the supported maximum %d", o.DownloadLimit, int64(MaxRateBytes))
	}
	if o.UploadLimit > MaxRateBytes {
		return fmt.Errorf("upload rate %d bytes/s is above the supported maximum %d", o.UploadLimit, int64(MaxRateBytes))
	}
	if o.PeersPerJob < 0 || o.PeersPerJob > MaxPeersPerJob {
		return fmt.Errorf("peers-per-job must be between 1 and %d, got %d", MaxPeersPerJob, o.PeersPerJob)
	}
	if o.HalfOpenPerJob < 0 || o.HalfOpenPerJob > MaxPeersPerJob {
		return fmt.Errorf("half-open-per-job must be between 1 and %d, got %d", MaxPeersPerJob, o.HalfOpenPerJob)
	}
	return nil
}

func (o Options) maxActive() int {
	if o.MaxActive < 1 {
		return 1
	}
	return o.MaxActive
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

// Runner drains a queue with up to MaxActive workers.
type Runner struct {
	store    *Store
	transfer Transfer
	opts     Options
	// downloadLimiter and uploadLimiter are built once per runner and shared by
	// every attempt, so concurrent jobs contend on one budget instead of each
	// getting its own.
	downloadLimiter *rate.Limiter
	uploadLimiter   *rate.Limiter
	// live is the in-memory view this process publishes over its local endpoint. It
	// is nil when no endpoint is running.
	live *liveState
}

// liveAdd, livePhase, liveStopping, liveUploaded and liveRemove are no-ops without an
// endpoint, so the runner has one code path with or without live status.
func (r *Runner) liveAdd(id, kind, phase string) {
	if r.live != nil {
		r.live.add(id, kind, phase)
	}
}

func (r *Runner) livePhase(id, phase string) {
	if r.live != nil {
		r.live.phase(id, phase)
	}
}

func (r *Runner) liveStopping(id string) {
	if r.live != nil {
		r.live.stopping(id)
	}
}

func (r *Runner) liveUploaded(id string, n int64) {
	if r.live != nil {
		r.live.uploaded(id, n)
	}
}

func (r *Runner) liveRemove(id string) {
	if r.live != nil {
		r.live.remove(id)
	}
}

// newSharedLimiter builds one limiter for a rate, or nil for unlimited. The burst
// is explicit because the pinned library panics on a zero burst.
func newSharedLimiter(bytesPerSecond int64, burst int) *rate.Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(bytesPerSecond), burst)
}

// NewRunner builds a runner over a store and a backend.
func NewRunner(store *Store, transfer Transfer, opts Options) *Runner {
	return &Runner{
		store:           store,
		transfer:        transfer,
		opts:            opts,
		downloadLimiter: newSharedLimiter(opts.DownloadLimit, DownloadBurstBytes),
		uploadLimiter:   newSharedLimiter(opts.UploadLimit, UploadBurstBytes),
	}
}

// Summary reports what one drain did.
type Summary struct {
	// Ran lists entry ids in the order they were claimed.
	Ran       []string
	Completed int
	Failed    int
	Blocked   int
	Paused    int
	// Seeded lists entries this runner started serving, and SeedFinished counts
	// the seed slots that closed. SeedBlocked counts workspaces that could not be
	// served (damaged, selective, ineligible).
	Seeded       []string
	SeedFinished int
	SeedBlocked  int
	// Stale counts attempt results that arrived after the attempt was superseded.
	Stale int
	// Remaining counts entries that are still runnable or blocked after the drain.
	Remaining int
}

// Run drains the queue: reconcile first, then claim and dispatch entries in queue
// order until nothing is running and nothing is claimable, or ctx is cancelled.
// Cancelling ctx (SIGINT) stops claiming, cancels every active attempt, waits for
// the workers to return, and preserves every entry's desired state.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	if err := r.opts.Validate(); err != nil {
		return sum, err
	}
	l, err := r.store.LockRunner()
	if err != nil {
		if errors.Is(err, ErrRunnerActive) {
			return sum, fmt.Errorf("%w: %s is already being drained", ErrRunnerActive, r.store.root)
		}
		return sum, err
	}
	defer func() { _ = l.Release() }()

	// Publish the live endpoint only while this process owns the queue, and stop
	// accepting readers before the workers finish shutting down.
	live := newLiveState(r.store.root)
	live.mu.Lock()
	live.downloads = LiveSlots{Max: r.opts.maxActive()}
	live.seeds = LiveSlots{Max: r.opts.maxSeeds()}
	live.mu.Unlock()
	r.live = live
	srv, lerr := startLiveServer(r.store.root, live)
	if lerr != nil {
		// Live status is an observer, never a precondition: log and run without it.
		r.logf("live status unavailable: %v", lerr)
		r.live = nil
	} else {
		r.logf("live endpoint %s", srv.sockPath)
		defer func() {
			r.logf("live endpoint closing")
			srv.close()
		}()
	}

	adopted, err := r.reconcile()
	if err != nil {
		return sum, err
	}
	sum.Completed += adopted

	limit := r.opts.maxActive()
	var mu sync.Mutex
	// The seed pool runs beside the download loop with its own slots. It is started
	// once and reports done when it has nothing left to serve.
	seedsOn := r.opts.maxSeeds() > 0
	seedDone := make(chan struct{})
	if seedsOn {
		go func() {
			defer close(seedDone)
			r.runSeeds(ctx, &mu, &sum)
		}()
	} else {
		close(seedDone)
	}
	done := make(chan string, limit)
	inflight := map[string]context.CancelFunc{}
	active := 0

	// take releases the slot of one finished attempt: with wait=false only if one is
	// already available, otherwise it parks until one finishes (or the run is
	// cancelled). It takes exactly one, so a finished attempt frees its slot for the
	// next claim instead of the coordinator waiting for every active attempt.
	take := func(wait bool) bool {
		if active == 0 {
			return false
		}
		var id string
		if wait {
			select {
			case id = <-done:
			case <-ctx.Done():
				return false
			}
		} else {
			select {
			case id = <-done:
			default:
				return false
			}
		}
		active--
		delete(inflight, id)
		return true
	}

	for {
		for take(false) {
		}
		if ctx.Err() != nil {
			break
		}
		// Claim in FIFO order, skipping anything already in flight. A claim that
		// loses a race with another writer is skipped for this round rather than
		// retried in a loop.
		skipped := map[string]bool{}
		for active < limit {
			entry, ok, nerr := r.nextExcept(inflight, skipped)
			if nerr != nil {
				r.shutdown(inflight, done, active)
				return sum, nerr
			}
			if !ok {
				break
			}
			claimed, cerr := r.claim(entry)
			if cerr != nil {
				r.logf("%s: claim failed (%v); trying the next entry this round", entry.ID, cerr)
				skipped[entry.ID] = true
				continue
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			inflight[claimed.ID] = cancel
			active++
			mu.Lock()
			sum.Ran = append(sum.Ran, claimed.ID)
			mu.Unlock()
			go func(e Entry, token string, c context.Context) {
				defer func() { done <- e.ID }()
				r.runEntry(c, e, token, &mu, &sum)
			}(claimed, claimed.Attempt, attemptCtx)
		}
		if active == 0 {
			if seedsOn {
				// Downloads drained. With managed seeding requested the runner stays
				// alive while the seed pool has work and exits when it has none, so
				// an active seed is never cut off by a finished download queue.
				r.logf("downloads drained; managing seeds until they stop")
				<-seedDone
				break
			}
			// Drained: nothing is running and nothing is eligible. An entry blocked
			// on a future not_before is not eligible, so a drain ends instead of
			// polling until the backoff expires - the same contract as the
			// single-worker runner.
			break
		}
		take(true)
	}
	r.shutdown(inflight, done, active)
	if seedsOn {
		<-seedDone
	}
	return sum, nil
}

// shutdown cancels every active attempt and waits for the workers to return, so
// Run never returns while a worker could still mutate a record.
func (r *Runner) shutdown(inflight map[string]context.CancelFunc, done chan string, active int) {
	for id, cancel := range inflight {
		cancel()
		delete(inflight, id)
	}
	for ; active > 0; active-- {
		<-done
	}
}

// nextExcept returns the first runnable entry in queue order that is not already
// in flight and was not skipped this round.
func (r *Runner) nextExcept(inflight map[string]context.CancelFunc, skipped map[string]bool) (Entry, bool, error) {
	entries, err := r.store.List()
	if err != nil {
		return Entry{}, false, err
	}
	now := time.Now()
	for _, e := range entries {
		if _, busy := inflight[e.ID]; busy || skipped[e.ID] {
			continue
		}
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

// claim takes ownership of one attempt: it re-reads the record under the store
// lock, re-checks eligibility, and stamps a fresh attempt token so a result from a
// superseded attempt cannot overwrite a newer one.
func (r *Runner) claim(entry Entry) (Entry, error) {
	token := newAttemptToken()
	return r.store.Update(entry.ID, func(e *Entry) error {
		if e.Desired != DesiredRun {
			return errors.New("entry is no longer runnable")
		}
		if e.Phase == PhaseCompleted || e.Phase == PhaseFailed {
			return errors.New("entry is no longer runnable")
		}
		if e.Phase == PhaseBlocked && !e.NotBefore.IsZero() && time.Now().Before(e.NotBefore) {
			return errors.New("entry is still backing off")
		}
		e.Phase = PhaseRunning
		e.Attempts++
		e.Attempt = token
		e.Note = ""
		e.LastError = ""
		return nil
	})
}

// updateAttempt applies a mutation only to the attempt that produced it.
func (r *Runner) updateAttempt(id, token string, mutate func(*Entry) error) (Entry, error) {
	return r.store.Update(id, func(e *Entry) error {
		if e.Attempt != token {
			return errStaleAttempt
		}
		return mutate(e)
	})
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
					x.Attempt = ""
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
				x.Attempt = ""
				x.Note = "interrupted while the runner was not running; desired state kept"
				return nil
			}); uerr != nil {
				return 0, uerr
			}
			r.logf("%s: interrupted -> %s (desired %s)", e.ID, phase, e.Desired)
		}
		// A recorded serving/verifying/stopping phase is an observation from a process
		// that no longer exists: the runner lock was free, so no client can still own
		// the workspace. Reset it to what intent allows, or the entry would be stuck
		// uneligible forever and never resume.
		switch e.SeedPhase {
		case SeedServing, SeedVerifying, SeedStopping:
			seedPhase := SeedStopped
			if e.SeedWanted() {
				seedPhase = SeedIdle
			}
			if _, uerr := r.store.Update(e.ID, func(x *Entry) error {
				x.SeedPhase = seedPhase
				x.SeedAttempt = ""
				x.Note = "seed interrupted by process death; intent kept"
				return nil
			}); uerr != nil {
				return 0, uerr
			}
			r.logf("%s: stale seed phase %s -> %s (intent %s)", e.ID, e.SeedPhase, seedPhase, e.SeedDesired)
		}
	}
	return adopted, nil
}

// runEntry executes one attempt and records the outcome. It never returns an
// error: a failed entry is a recorded state, not a reason to stop draining, so one
// bad job cannot block the ones behind it.
func (r *Runner) runEntry(ctx context.Context, entry Entry, token string, mu *sync.Mutex, sum *Summary) {
	r.logf("%s: running (%s)", entry.ID, entry.JobID)
	r.liveAdd(entry.ID, "download", "running")
	defer r.liveRemove(entry.ID)

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
		// Shared instances, not copies and not rate/MaxActive: a lone job may use
		// the whole budget.
		DownloadLimiter: r.downloadLimiter,
		UploadLimiter:   r.uploadLimiter,

		Priorities: map[string]string{},

		PeersPerAttempt:    r.opts.PeersPerJob,
		HalfOpenPerAttempt: r.opts.HalfOpenPerJob,
	}
	for p, lvl := range entry.Priorities {
		req.Priorities[p] = lvl
	}

	runCtx, cancel := context.WithCancel(ctx)
	var paused syncFlag
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.watchPause(runCtx, entry.ID, token, cancel, &paused)
	}()

	out, runErr := r.transfer.Run(runCtx, req)
	// Cancelling and waiting here is what makes the slot release honest: Run only
	// returns once the backend has closed its client and released the workspace.
	cancel()
	wg.Wait()

	if r.opts.BeforeResultPersist != nil {
		r.opts.BeforeResultPersist(entry.ID)
	}

	if runErr == nil {
		r.finishCompleted(entry.ID, token, out, mu, sum)
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
			if _, uerr := r.updateAttempt(entry.ID, token, func(e *Entry) error {
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
				r.recordStale(entry.ID, uerr, mu, sum)
				return
			}
			r.count(mu, sum, func(s *Summary) { s.Completed++ })
			r.logf("%s: adopted capture after failure: %s", entry.ID, ref.Key)
			return
		}
	}

	reason := SanitizeError(runErr)
	switch {
	case paused.load():
		// The user asked for this. Record it as a pause, not a failure.
		if _, uerr := r.updateAttempt(entry.ID, token, func(e *Entry) error {
			e.Phase = PhasePaused
			e.Note = "paused at request; the client closed and the workspace was released"
			return nil
		}); uerr != nil {
			r.recordStale(entry.ID, uerr, mu, sum)
			return
		}
		r.count(mu, sum, func(s *Summary) { s.Paused++ })
		r.logf("%s: paused", entry.ID)
	case errors.Is(runErr, context.Canceled) && ctx.Err() != nil:
		// Runner shutdown. Desired state is the user's intent and is untouched, so
		// the next runner resumes instead of reading this as a pause.
		if _, uerr := r.updateAttempt(entry.ID, token, func(e *Entry) error {
			e.Phase = PhaseQueued
			if e.Desired == DesiredPaused {
				e.Phase = PhasePaused
			}
			e.Note = "interrupted by runner shutdown; intent preserved"
			return nil
		}); uerr != nil {
			r.recordStale(entry.ID, uerr, mu, sum)
			return
		}
	case errors.Is(runErr, ErrWorkspaceBusy):
		backoff := r.opts.backoff()
		if _, uerr := r.updateAttempt(entry.ID, token, func(e *Entry) error {
			e.Phase = PhaseBlocked
			e.LastError = reason
			e.NotBefore = time.Now().UTC().Add(backoff)
			e.Note = fmt.Sprintf("retryable: workspace busy; retry after %s or run the queue again", backoff)
			return nil
		}); uerr != nil {
			r.recordStale(entry.ID, uerr, mu, sum)
			return
		}
		r.count(mu, sum, func(s *Summary) { s.Blocked++ })
		r.logf("%s: blocked (%s)", entry.ID, reason)
	default:
		if _, uerr := r.updateAttempt(entry.ID, token, func(e *Entry) error {
			e.Phase = PhaseFailed
			e.LastError = reason
			e.Note = "failed; fix the cause and run `torrent queue retry` to try again"
			return nil
		}); uerr != nil {
			r.recordStale(entry.ID, uerr, mu, sum)
			return
		}
		r.count(mu, sum, func(s *Summary) { s.Failed++ })
		r.logf("%s: failed (%s)", entry.ID, reason)
	}
}

// finishCompleted records a successful run, attaching the canonical identity only
// now that the backend has validated it.
func (r *Runner) finishCompleted(id, token string, out Outcome, mu *sync.Mutex, sum *Summary) {
	if _, err := r.updateAttempt(id, token, func(e *Entry) error {
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
		r.recordStale(id, err, mu, sum)
		return
	}
	r.count(mu, sum, func(s *Summary) { s.Completed++ })
	r.logf("%s: completed (%s)", id, out.CaptureKey)
}

// recordStale counts a result that arrived too late to apply. It is not a failure:
// the record was already moved on by a newer command or attempt.
func (r *Runner) recordStale(id string, err error, mu *sync.Mutex, sum *Summary) {
	if errors.Is(err, errStaleAttempt) {
		r.count(mu, sum, func(s *Summary) { s.Stale++ })
		r.logf("%s: stale attempt result ignored", id)
		return
	}
	r.count(mu, sum, func(s *Summary) { s.Failed++ })
}

func (r *Runner) count(mu *sync.Mutex, sum *Summary, fn func(*Summary)) {
	mu.Lock()
	fn(sum)
	mu.Unlock()
}

// watchPause polls recorded intent while a job runs. A pause request switches the
// entry to "stopping" (the client may still be closing) and cancels the worker;
// the phase only becomes "paused" once Run has returned and the workspace lock is
// released. It only ever acts on its own attempt: a superseded attempt stops
// watching without touching the record.
func (r *Runner) watchPause(ctx context.Context, id, token string, cancel context.CancelFunc, paused *syncFlag) {
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
			r.liveStopping(id)
			if _, uerr := r.updateAttempt(id, token, func(e *Entry) error {
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
