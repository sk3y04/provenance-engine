package torrentqueue

// Managed seeding inside the foreground runner (BT-08d.3).
//
// Seeding is a second pool with its own slots, not an extension of a download
// attempt: a published capture releases the download slot whether or not the job
// then seeds. The pool holds a seed slot from claim through verification, Serve and
// Close, and releases it only after the workspace lock is gone.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// SeedSession is the slice of a verified seed session the queue needs.
type SeedSession interface {
	Serve(ctx context.Context) error
	Close() error
	Address() string
	UploadedBytes() int64
}

// SeedOpener builds a verified seed session for one completed entry. The adapter
// implements it; a test may wrap it.
type SeedOpener interface {
	OpenSeed(entry Entry, req Request) (SeedSession, error)
}

// seedAttemptKey is the token written into SeedAttempt for one dispatch.
func seedAttemptKey(entryID string, seq uint64) string {
	return fmt.Sprintf("%s/seed-%d", entryID, seq)
}

// maxSeeds is the validated seed-slot cap; zero disables managed seeding entirely.
func (o Options) maxSeeds() int { return o.MaxSeeds }

// runSeeds is the seed coordinator. It is one goroutine beside the download loop,
// claims in queue order, never holds a workspace lock while waiting for a slot, and
// returns when nothing is left that it is responsible for.
func (r *Runner) runSeeds(ctx context.Context, mu *sync.Mutex, sum *Summary) {
	limit := r.opts.maxSeeds()
	if limit <= 0 {
		return
	}
	type handle struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	active := map[string]*handle{}
	workspaces := map[string]string{}
	ticker := time.NewTicker(r.opts.pollInterval())
	defer ticker.Stop()

	reap := func() {
		for id, h := range active {
			select {
			case <-h.done:
				delete(active, id)
				for ws, owner := range workspaces {
					if owner == id {
						delete(workspaces, ws)
					}
				}
				r.count(mu, sum, func(s *Summary) { s.SeedFinished++ })
			default:
			}
		}
	}

	for {
		reap()
		if ctx.Err() != nil {
			break
		}
		for len(active) < limit {
			skip := make(map[string]bool, len(active))
			for id := range active {
				skip[id] = true
			}
			cands, err := r.store.SeedCandidates(time.Now(), skip)
			if err != nil {
				r.logf("seed: cannot read candidates (%v)", err)
				break
			}
			started := false
			for _, entry := range cands {
				if ws := entry.Workspace; ws != "" {
					if _, taken := workspaces[ws]; taken {
						// One canonical workspace has exactly one owner; the second
						// entry waits rather than racing the lock.
						continue
					}
				}
				claimed, cerr := r.claimSeed(entry)
				if cerr != nil {
					if errors.Is(cerr, errStaleAttempt) {
						continue
					}
					r.logf("%s: seed claim failed (%v)", entry.ID, cerr)
					continue
				}
				seedCtx, cancel := context.WithCancel(ctx)
				h := &handle{cancel: cancel, done: make(chan struct{})}
				active[claimed.ID] = h
				workspaces[claimed.Workspace] = claimed.ID
				if claimed.Workspace == "" {
					delete(workspaces, "")
				}
				r.count(mu, sum, func(s *Summary) {
					s.Seeded = append(s.Seeded, claimed.ID)
				})
				go func(e Entry, c context.Context, done chan struct{}) {
					defer close(done)
					r.runSeed(c, e, mu, sum)
				}(claimed, seedCtx, h.done)
				started = true
				break
			}
			if !started {
				break
			}
		}
		if len(active) == 0 {
			pending, perr := r.store.SeedPending()
			if perr != nil || !pending {
				// No eligible seed and nothing pending. A drain ends here: a seed
				// enabled after the pool emptied belongs to a later invocation. A
				// watch session keeps scanning instead, so an intent saved by
				// another process starts serving without a restart.
				if !r.opts.Watch {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	for _, h := range active {
		h.cancel()
	}
	for _, h := range active {
		<-h.done
	}
}

// claimSeed takes the record for one dispatch. The token makes a late result from a
// superseded attempt a stale result rather than a silent overwrite.
func (r *Runner) claimSeed(entry Entry) (Entry, error) {
	token := seedAttemptKey(entry.ID, entry.Seq)
	return r.store.UpdateSeed(entry.ID, "", func(e *Entry) error {
		if !e.SeedEligible(time.Now()) {
			return errStaleAttempt
		}
		e.SeedAttempt = token
		e.SeedPhase = SeedVerifying
		e.SeedError = ""
		return nil
	})
}

// runSeed owns one seed slot for its whole life: verification, serving, close and the
// release of the workspace lock. The coordinator reaps it only after done closes.
func (r *Runner) runSeed(ctx context.Context, entry Entry, mu *sync.Mutex, sum *Summary) {
	token := entry.SeedAttempt
	r.liveAdd(entry.ID, "seed", "verifying")
	defer r.liveRemove(entry.ID)
	sess, err := r.seedOpener().OpenSeed(entry, r.seedRequest(entry))
	if err != nil {
		r.seedFailed(entry, token, err, mu, sum)
		return
	}

	if _, uerr := r.store.UpdateSeed(entry.ID, token, func(e *Entry) error {
		e.SeedPhase = SeedServing
		e.SeedError = ""
		return nil
	}); uerr != nil {
		// The intent changed while verification ran: do not serve.
		r.closeSeed(sess, entry, token)
		return
	}

	r.livePhase(entry.ID, "serving")
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(r.opts.pollInterval())
		defer ticker.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-ticker.C:
				next, gerr := r.store.Get(entry.ID)
				if gerr != nil {
					continue
				}
				if !next.SeedWanted() {
					cancel()
					return
				}
			}
		}
	}()

	serr := sess.Serve(serveCtx)
	cancel()
	<-watchDone
	// Closing is the part a reader must see: the slot is still held.
	r.liveStopping(entry.ID)
	if n := sess.UploadedBytes(); n > 0 {
		r.liveUploaded(entry.ID, n)
	}
	r.closeSeed(sess, entry, token)
	_ = serr
}

// closeSeed records stopping until the client has closed, then publishes the durable
// outcome. The lock is released inside Close, so recording after it is honest.
func (r *Runner) closeSeed(sess SeedSession, entry Entry, token string) {
	_, _ = r.store.UpdateSeed(entry.ID, token, func(e *Entry) error {
		e.SeedPhase = SeedStopping
		return nil
	})
	if r.opts.BeforeSeedClose != nil {
		r.opts.BeforeSeedClose(entry.ID)
	}
	if err := sess.Close(); err != nil {
		r.logf("%s: seed close: %v", entry.ID, err)
	}
	_, err := r.store.UpdateSeed(entry.ID, token, func(e *Entry) error {
		if e.SeedDesired == SeedOn {
			// Interrupted, not stopped by the user: intent survives, so a later run
			// resumes it.
			e.SeedPhase = SeedIdle
			e.Note = "seed interrupted; intent kept"
		} else {
			e.SeedPhase = SeedStopped
			e.Note = ""
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStaleAttempt) {
		r.logf("%s: seed result: %v", entry.ID, err)
	}
}

// seedFailed records an ineligible or unverifiable workspace. It never repairs,
// fetches or republishes: the entry waits for a person, not for a retry timer.
func (r *Runner) seedFailed(entry Entry, token string, err error, mu *sync.Mutex, sum *Summary) {
	actionable := actionForSeed(err)
	_, uerr := r.store.UpdateSeed(entry.ID, token, func(e *Entry) error {
		e.SeedPhase = SeedBlocked
		e.SeedError = SanitizeError(err)
		e.Note = actionable
		e.SeedNotBefore = time.Now().Add(r.opts.backoff())
		return nil
	})
	if uerr != nil && !errors.Is(uerr, errStaleAttempt) {
		r.logf("%s: seed failure not recorded: %v", entry.ID, uerr)
	}
	r.count(mu, sum, func(s *Summary) { s.SeedBlocked++ })
	r.logf("%s: cannot seed: %s", entry.ID, SanitizeError(err))
}

// actionForSeed turns a verification refusal into what the user should do next.
func actionForSeed(err error) string {
	switch {
	case errors.Is(err, bittorrent.ErrSeedSelective):
		return "this job captured only part of the torrent; it cannot be served"
	case errors.Is(err, bittorrent.ErrSeedDataIncomplete):
		return "local data no longer verifies; rerun the fetch to repair it"
	case errors.Is(err, bittorrent.ErrSeedIneligible):
		return "the workspace has no published capture to serve"
	default:
		return "seeding was refused; inspect the job workspace"
	}
}

// seedOpener returns the backend, or a refusing stub when the runner was configured
// with seed slots but the backend cannot serve.
func (r *Runner) seedOpener() SeedOpener {
	if o, ok := r.transfer.(SeedOpener); ok {
		return o
	}
	return refusingOpener{}
}

type refusingOpener struct{}

func (refusingOpener) OpenSeed(Entry, Request) (SeedSession, error) {
	return nil, errors.New("this transfer backend cannot serve a completed workspace")
}

// seedRequest is the request a seed session needs: the recorded discovery profile,
// the same shared upload budget and the same per-job peer limit as a download.
func (r *Runner) seedRequest(entry Entry) Request {
	return Request{
		Root:               r.store.root,
		Peers:              nil,
		Selection:          entry.Selection,
		Discovery:          entry.Discovery,
		KnownJobID:         entry.JobID,
		UploadLimiter:      r.uploadLimiter,
		DownloadLimiter:    r.downloadLimiter,
		PeersPerAttempt:    r.opts.PeersPerJob,
		HalfOpenPerAttempt: r.opts.HalfOpenPerJob,
	}
}
