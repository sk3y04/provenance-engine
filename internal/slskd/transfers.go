package slskd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultSnapshotTTL bounds how stale a live transfer listing may be. UI reads
// come from this cache; every mutation invalidates it immediately.
const DefaultSnapshotTTL = 3 * time.Second

// DefaultQueuePositionInterval rate-limits the per-transfer queue probe, which
// costs a message round-trip to the peer.
const DefaultQueuePositionInterval = 10 * time.Second

// ErrPositionThrottled is returned when a queue-position probe is requested
// again before the per-transfer interval has elapsed.
var ErrPositionThrottled = errors.New("queue position was checked recently; try again in a few seconds")

// EnqueueRequest describes a batch to grab from one peer.
type EnqueueRequest struct {
	SearchID   string
	SearchText string
	Peer       string
	// Files are the remote files to download; locked files are refused here.
	Files []File
	// Alternates are ranked fallback candidates, kept with the job so "try
	// another source" needs no new search.
	Alternates []Candidate
	// DestinationSubdir overrides the pinned "provenance/<jobID>" default.
	DestinationSubdir string
}

// EnqueueResult is the outcome of an enqueue attempt.
type EnqueueResult struct {
	Job *Job
	// Duplicate means an active job already covers one of these files and
	// nothing new was enqueued; Job is that existing job.
	Duplicate bool
	// Failures lists files slskd refused (a 207 partial success).
	Failures []EnqueueBatchFailure
}

// Reconciler correlates slskd transfers with durable jobs.
type Reconciler struct {
	client *Client
	store  *JobStore
	now    func() time.Time

	mu          sync.Mutex
	snapshot    []Transfer
	snapshotAt  time.Time
	snapshotTTL time.Duration
	// snapshotValid is false after any mutation so the next read refetches.
	snapshotValid bool

	posMu       sync.Mutex
	posLast     map[string]time.Time
	posInterval time.Duration
}

// NewReconciler wires a reconciler over a client and a job store.
func NewReconciler(c *Client, store *JobStore) *Reconciler {
	return &Reconciler{
		client:      c,
		store:       store,
		now:         func() time.Time { return time.Now().UTC() },
		snapshotTTL: DefaultSnapshotTTL,
		posLast:     map[string]time.Time{},
		posInterval: DefaultQueuePositionInterval,
	}
}

// Store exposes the job store.
func (r *Reconciler) Store() *JobStore { return r.store }

// Client exposes the client.
func (r *Reconciler) Client() *Client { return r.client }

// Enqueue validates a batch, persists the job, then asks slskd to fetch it.
//
// The order matters: a crash between the two leaves a QUEUED job with no slskd
// batch, which recovery can re-enqueue, whereas the reverse would leave
// downloads running with no record of why.
func (r *Reconciler) Enqueue(ctx context.Context, req EnqueueRequest) (*EnqueueResult, error) {
	peer := strings.TrimSpace(req.Peer)
	if peer == "" {
		return nil, errors.New("peer username is required")
	}
	if len(req.Files) == 0 {
		return nil, errors.New("no files to download")
	}
	for _, f := range req.Files {
		if err := ValidateRemotePath(f.Filename); err != nil {
			return nil, err
		}
		if f.Size < 0 {
			return nil, fmt.Errorf("file %q has a negative size", f.Filename)
		}
		if f.IsLocked {
			return nil, fmt.Errorf("file %q is locked by the peer and cannot be downloaded", f.Filename)
		}
	}

	// Attach to an existing job rather than double-enqueueing the same bytes.
	for _, f := range req.Files {
		if existing, ok, err := r.store.FindActiveItem(peer, f.Filename); err != nil {
			return nil, err
		} else if ok {
			return &EnqueueResult{Job: &existing, Duplicate: true}, nil
		}
	}

	jobID, err := NewJobID()
	if err != nil {
		return nil, err
	}
	job := Job{
		ID:         jobID,
		SearchID:   req.SearchID,
		SearchText: req.SearchText,
		Peer:       peer,
		Alternates: req.Alternates,
		Status:     JobQueued,
	}
	if req.DestinationSubdir != "" {
		if err := validateDestinationSubdir(req.DestinationSubdir); err != nil {
			return nil, err
		}
		job.DestinationSubdir = req.DestinationSubdir
	} else {
		job.DestinationSubdir = "provenance/" + jobID
	}
	items := make([]Item, 0, len(req.Files))
	batchFiles := make([]EnqueueBatchItem, 0, len(req.Files))
	for _, f := range req.Files {
		items = append(items, Item{RemoteFilename: f.Filename, Size: f.Size})
		batchFiles = append(batchFiles, EnqueueBatchItem{Filename: f.Filename, Size: f.Size})
	}
	job.Items = items

	created, err := r.store.Create(job)
	if err != nil {
		return nil, err
	}
	r.invalidateSnapshot()

	batchID := newUUID()
	resp, err := r.client.EnqueueBatch(ctx, EnqueueBatchRequest{
		ID:       batchID,
		SearchID: req.SearchID,
		Username: peer,
		Files:    batchFiles,
		Options:  &EnqueueBatchOptions{Destination: created.DestinationSubdir, ExternalID: created.ID},
	})
	if err != nil {
		failed, updateErr := r.store.Update(created.ID, func(j *Job) error {
			j.Status = JobFailed
			j.FailureReason = err.Error()
			return nil
		})
		if updateErr != nil {
			return nil, errors.Join(err, updateErr)
		}
		_ = failed
		return nil, err
	}

	updated, err := r.store.Update(created.ID, func(j *Job) error {
		j.BatchID = batchID
		if resp.Batch != nil && resp.Batch.ID != "" {
			j.BatchID = resp.Batch.ID
		}
		applyBatchTransfers(j, resp)
		applyBatchFailures(j, resp.Failures)
		j.Status = jobStatusFromItems(j)
		if j.Status == JobFailed {
			j.FailureReason = failureSummary(j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &EnqueueResult{Job: &updated, Failures: resp.Failures}, nil
}

// applyBatchTransfers records slskd's transfer ids so a later poll can correlate
// by id rather than by filename.
func applyBatchTransfers(j *Job, resp *EnqueueBatchResponse) {
	if resp == nil || resp.Batch == nil {
		return
	}
	byName := map[string]string{}
	for _, t := range resp.Batch.Transfers {
		byName[strings.ToLower(t.Filename)] = t.ID
	}
	for i := range j.Items {
		if id, ok := byName[strings.ToLower(j.Items[i].RemoteFilename)]; ok {
			j.Items[i].TransferID = id
		}
	}
	// A transfer record carries the state slskd already knows about.
	for _, t := range resp.Batch.Transfers {
		for i := range j.Items {
			if strings.EqualFold(j.Items[i].RemoteFilename, t.Filename) && t.State != "" {
				j.Items[i].State = t.State
			}
		}
	}
}

func validateDestinationSubdir(sub string) error {
	if strings.TrimSpace(sub) == "" {
		return errors.New("destination subdirectory is empty")
	}
	norm := strings.ReplaceAll(sub, "\\", "/")
	if strings.HasPrefix(norm, "/") {
		return fmt.Errorf("destination %q must be relative to the slskd download root", sub)
	}
	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("destination %q contains an empty or traversal segment", sub)
		}
	}
	return ValidateRemotePath(sub)
}

// Poll reconciles every pending job against slskd's transfer list.
func (r *Reconciler) Poll(ctx context.Context) ([]Job, error) {
	transfers, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := r.store.Pending()
	if err != nil {
		return nil, err
	}
	updated := make([]Job, 0, len(pending))
	for _, job := range pending {
		next, err := r.applyTransfers(job, transfers)
		if err != nil {
			return updated, err
		}
		updated = append(updated, next)
	}
	return updated, nil
}

// applyTransfers folds one transfer listing into one job.
func (r *Reconciler) applyTransfers(job Job, transfers []Transfer) (Job, error) {
	matched := matchTransfers(job, transfers)
	if len(matched) == 0 {
		// No transfer record: either slskd accepted the batch and has not created
		// the transfers yet, or its retention dropped them. Either way there is
		// nothing to fold in and nothing to fail here (recovery owns the second
		// case).
		return job, nil
	}
	return r.store.Update(job.ID, func(j *Job) error {
		var firstReason string
		for i := range j.Items {
			t, ok := matched[strings.ToLower(normalizeSep(j.Items[i].RemoteFilename))]
			if !ok {
				continue
			}
			j.Items[i].TransferID = t.ID
			j.Items[i].State = t.State
			if failedNow, reason := terminalFailure(t.State); failedNow {
				j.Items[i].FailureReason = reason
				if firstReason == "" {
					firstReason = reason
					if t.Exception != "" {
						firstReason = reason + ": " + t.Exception
					}
				}
			}
		}
		// Derive the status from the recorded item states, never from this
		// listing alone: a transfer slskd already dropped must not silently reset
		// progress, and a file refused at enqueue has no transfer at all.
		j.Status = jobStatusFromItems(j)
		if j.Status == JobFailed {
			if summary := failureSummary(j); summary != "" {
				j.FailureReason = summary
			} else if firstReason != "" {
				j.FailureReason = firstReason
			}
		}
		return nil
	})
}

// applyBatchFailures marks the files slskd refused in a partial (207) batch.
// Without the marker such a file has no transfer and no state, so the job would
// wait for it forever.
func applyBatchFailures(j *Job, failures []EnqueueBatchFailure) {
	if len(failures) == 0 {
		return
	}
	byName := make(map[string]string, len(failures))
	for _, f := range failures {
		byName[itemKey(f.Filename)] = f.Message
	}
	for i := range j.Items {
		msg, ok := byName[itemKey(j.Items[i].RemoteFilename)]
		if !ok {
			continue
		}
		j.Items[i].Rejected = true
		if strings.TrimSpace(msg) == "" {
			msg = "no reason given"
		}
		j.Items[i].FailureReason = "slskd refused this file: " + msg
	}
}

func itemKey(remote string) string { return strings.ToLower(normalizeSep(remote)) }

// jobStatusFromItems derives one status from every item's recorded state. It is
// the single place that decides when a job is finished, so a partial batch, a
// terminal failure and a completed job cannot disagree with each other.
func jobStatusFromItems(j *Job) JobStatus {
	if len(j.Items) == 0 {
		return JobQueued
	}
	succeeded, failed, pending := 0, 0, 0
	for _, it := range j.Items {
		if it.Rejected {
			failed++
			continue
		}
		terminal, ok := transferOutcome(it.State)
		switch {
		case terminal && ok:
			succeeded++
		case terminal:
			failed++
		default:
			pending++
		}
	}
	switch {
	case succeeded == len(j.Items):
		return JobCompletedPendingImport
	case failed == len(j.Items):
		return JobFailed
	case pending == 0 && succeeded > 0:
		// Everything has settled but some files failed: finished, with a partial
		// result the user has to see.
		return JobFailed
	default:
		return JobInProgress
	}
}

// failureSummary names every failed file and why, for the job record.
func failureSummary(j *Job) string {
	var parts []string
	for _, it := range j.Items {
		name := RemoteBaseName(it.RemoteFilename)
		switch {
		case it.Rejected:
			parts = append(parts, name+": "+it.FailureReason)
		default:
			if failed, reason := terminalFailure(it.State); failed {
				parts = append(parts, name+": "+reason)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// matchTransfers correlates transfers with a job, preferring the pinned batch id
// and falling back to (peer, filename) for a batch slskd no longer reports.
func matchTransfers(job Job, transfers []Transfer) map[string]Transfer {
	out := map[string]Transfer{}
	for _, t := range transfers {
		if job.BatchID != "" && t.BatchID == job.BatchID {
			out[strings.ToLower(normalizeSep(t.Filename))] = t
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, t := range transfers {
		if !strings.EqualFold(t.Username, job.Peer) {
			continue
		}
		key := strings.ToLower(normalizeSep(t.Filename))
		for _, it := range job.Items {
			if strings.ToLower(normalizeSep(it.RemoteFilename)) == key {
				out[key] = t
			}
		}
	}
	return out
}

func normalizeSep(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// Snapshot returns the live transfer list, served from a short-lived cache.
// The cache is invalidated by every mutation, so a cancel is visible at once.
func (r *Reconciler) Snapshot(ctx context.Context) ([]Transfer, error) {
	r.mu.Lock()
	if r.snapshotValid && r.now().Sub(r.snapshotAt) < r.snapshotTTL {
		out := r.snapshot
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	transfers, err := r.client.ListDownloads(ctx, true)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.snapshot = transfers
	r.snapshotAt = r.now()
	r.snapshotValid = true
	r.mu.Unlock()
	return transfers, nil
}

func (r *Reconciler) invalidateSnapshot() {
	r.mu.Lock()
	r.snapshotValid = false
	r.mu.Unlock()
}

// Cancel stops every transfer in a job and marks it cancelled.
func (r *Reconciler) Cancel(ctx context.Context, jobID string, remove bool) (Job, error) {
	job, err := r.store.Get(jobID)
	if err != nil {
		return Job{}, err
	}
	var errs []error
	for _, it := range job.Items {
		if it.TransferID == "" {
			continue
		}
		if err := r.client.CancelDownload(ctx, job.Peer, it.TransferID, remove); err != nil && !IsStatus(err, http.StatusNotFound) {
			errs = append(errs, err)
		}
	}
	r.invalidateSnapshot()
	if len(errs) > 0 {
		return Job{}, errors.Join(errs...)
	}
	return r.store.Update(jobID, func(j *Job) error {
		j.Status = JobCancelled
		j.FailureReason = "cancelled by user"
		return nil
	})
}

// Retry re-enqueues a failed, cancelled or interrupted job as a new job. With
// anotherSource it picks the best remaining alternate peer instead of the one
// that failed.
func (r *Reconciler) Retry(ctx context.Context, jobID string, anotherSource bool) (*EnqueueResult, error) {
	job, err := r.store.Get(jobID)
	if err != nil {
		return nil, err
	}
	if !job.Status.Terminal() && job.Status != JobInterrupted {
		return nil, fmt.Errorf("job %s is still %s; cancel it before retrying", job.ID, job.Status)
	}
	peer := job.Peer
	files := make([]File, 0, len(job.Items))
	for _, it := range job.Items {
		files = append(files, File{Filename: it.RemoteFilename, Size: it.Size})
	}
	alternates := job.Alternates
	if anotherSource {
		next, rest, err := pickAlternate(job)
		if err != nil {
			return nil, err
		}
		peer = next.Username
		files = append([]File(nil), next.Files...)
		alternates = rest
	}
	return r.Enqueue(ctx, EnqueueRequest{
		SearchID:   job.SearchID,
		SearchText: job.SearchText,
		Peer:       peer,
		Files:      files,
		Alternates: alternates,
	})
}

// pickAlternate selects the highest-ranked alternate that is not the peer that
// just failed.
func pickAlternate(job Job) (Candidate, []Candidate, error) {
	for i, c := range job.Alternates {
		if strings.EqualFold(c.Username, job.Peer) || len(c.Files) == 0 {
			continue
		}
		rest := make([]Candidate, 0, len(job.Alternates)-1)
		rest = append(rest, job.Alternates[:i]...)
		rest = append(rest, job.Alternates[i+1:]...)
		return c, rest, nil
	}
	return Candidate{}, nil, fmt.Errorf("no alternate source is available for job %s", job.ID)
}

// QueuePosition probes a peer for this job's place in its queue, rate-limited per
// job so a UI button cannot hammer a peer.
func (r *Reconciler) QueuePosition(ctx context.Context, jobID string) (map[string]int, error) {
	job, err := r.store.Get(jobID)
	if err != nil {
		return nil, err
	}
	r.posMu.Lock()
	last, ok := r.posLast[jobID]
	if ok && r.now().Sub(last) < r.posInterval {
		r.posMu.Unlock()
		return nil, ErrPositionThrottled
	}
	r.posMu.Unlock()

	out := map[string]int{}
	for _, it := range job.Items {
		if it.TransferID == "" {
			continue
		}
		pos, err := r.client.QueuePosition(ctx, job.Peer, it.TransferID)
		if err != nil {
			continue
		}
		if pos != nil && pos.Known {
			out[it.RemoteFilename] = pos.Position
		}
	}
	if len(out) == 0 {
		// A failed probe costs nothing: the interval exists to spare the peer,
		// not to lock a user out of retrying a transient error.
		return nil, errors.New("no queue position is available yet")
	}
	r.posMu.Lock()
	r.posLast[jobID] = r.now()
	r.posMu.Unlock()
	return out, nil
}

// ClearCompleted removes completed download records from slskd. Provenance job
// history is untouched.
func (r *Reconciler) ClearCompleted(ctx context.Context) error {
	r.invalidateSnapshot()
	return r.client.ClearCompletedDownloads(ctx)
}
