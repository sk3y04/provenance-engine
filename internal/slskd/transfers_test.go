package slskd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// transferServer is a scripted slskd downloads endpoint. It records every batch
// request and serves a mutable transfer list.
type transferServer struct {
	mu          sync.Mutex
	batches     []map[string]any
	batchStatus int
	batchBody   string
	transfers   []Transfer
	listCalls   int
	cancels     []string
	onBatch     func(batch map[string]any)
}

func (s *transferServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transfers/downloads/batches"):
			body, _ := io.ReadAll(r.Body)
			var decoded map[string]any
			_ = json.Unmarshal(body, &decoded)
			s.mu.Lock()
			s.batches = append(s.batches, decoded)
			cb := s.onBatch
			status := s.batchStatus
			raw := s.batchBody
			s.mu.Unlock()
			if cb != nil {
				cb(decoded)
			}
			if status != 0 && status != http.StatusCreated {
				if raw != "" {
					http.Error(w, raw, status)
					return
				}
				http.Error(w, "enqueue failed", status)
				return
			}
			if raw != "" {
				_, _ = io.WriteString(w, raw)
				return
			}
			id, _ := decoded["id"].(string)
			writeJSON(w, http.StatusCreated, map[string]any{
				"batch": map[string]any{
					"id": id, "username": decoded["username"], "createdAt": time.Now().UTC(),
					"transfers": []any{map[string]any{
						"id": "t1", "batchId": id, "username": decoded["username"],
						"filename": firstFilename(decoded), "state": "Queued, Remotely",
					}},
				},
			})

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transfers/downloads"):
			s.mu.Lock()
			s.listCalls++
			list := s.transfers
			s.mu.Unlock()
			writeJSON(w, 200, list)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/position"):
			writeJSON(w, 200, map[string]any{"position": 7})

		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/transfers/downloads/"):
			s.mu.Lock()
			s.cancels = append(s.cancels, r.URL.Path)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			http.NotFound(w, r)
		}
	})
}

func firstFilename(batch map[string]any) string {
	files, _ := batch["files"].([]any)
	if len(files) == 0 {
		return ""
	}
	m, _ := files[0].(map[string]any)
	name, _ := m["filename"].(string)
	return name
}

func newReconciler(t *testing.T, ts *transferServer) (*Reconciler, *JobStore, *transferServer) {
	t.Helper()
	srv := httptest.NewServer(ts.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	store := mustOpenStore(t, t.TempDir())
	r := NewReconciler(c, store)
	return r, store, ts
}

func flacFile(name string, size int64) File {
	return File{Filename: name, Size: size}
}

// TestEnqueuePersistsBeforePosting is the crash-safety ordering: a job record is
// on disk before slskd is asked to download, so a crash in between leaves a
// recoverable QUEUED job rather than unrecorded transfers.
func TestEnqueuePersistsBeforePosting(t *testing.T) {
	ts := &transferServer{}
	r, store, _ := newReconciler(t, ts)
	var seen int32
	ts.onBatch = func(map[string]any) {
		jobs, err := store.List()
		if err != nil {
			t.Errorf("list jobs: %v", err)
			return
		}
		atomic.StoreInt32(&seen, int32(len(jobs)))
	}

	res, err := r.Enqueue(context.Background(), EnqueueRequest{
		Peer:  "peer1",
		Files: []File{flacFile(`music\Album\track.flac`, 1234)},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := atomic.LoadInt32(&seen); got != 1 {
		t.Fatalf("%d job records existed at POST time, want 1 (job persisted after the call)", got)
	}
	if res.Job.Status != JobInProgress {
		t.Fatalf("status = %s", res.Job.Status)
	}
	if res.Job.BatchID == "" {
		t.Fatal("batch id not recorded")
	}
}

// TestEnqueuePinsExternalIDAndDestination pins the restart-safe contract: the
// batch carries our externalId and a destination derived from it.
func TestEnqueuePinsExternalIDAndDestination(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{
		Peer:       "peer1",
		SearchID:   "search-1",
		SearchText: "beethoven",
		Files:      []File{flacFile(`music\Album\track.flac`, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := ts.batches[0]
	opts, _ := body["options"].(map[string]any)
	if opts["externalId"] != res.Job.ID {
		t.Fatalf("externalId = %v, want %s", opts["externalId"], res.Job.ID)
	}
	if opts["destination"] != "provenance/"+res.Job.ID {
		t.Fatalf("destination = %v", opts["destination"])
	}
	if body["searchId"] != "search-1" {
		t.Fatalf("searchId = %v", body["searchId"])
	}
	if body["username"] != "peer1" {
		t.Fatalf("username = %v", body["username"])
	}
}

// TestEnqueueRejectsOverlongPathSegment covers #1818: a >255-byte segment retries
// forever inside slskd's incomplete directory, so it is refused before the call.
func TestEnqueueRejectsOverlongPathSegment(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	long := strings.Repeat("é", 126) + ".flac"
	if _, err := r.Enqueue(context.Background(), EnqueueRequest{
		Peer:  "peer1",
		Files: []File{flacFile(`music\`+long, 10)},
	}); err == nil {
		t.Fatal("overlong path segment accepted")
	}
	if len(ts.batches) != 0 {
		t.Fatal("slskd was called for a path we refuse")
	}
}

// TestEnqueueRejectsTraversal keeps a peer-supplied path from escaping the
// download destination.
func TestEnqueueRejectsTraversal(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	for _, name := range []string{`..\..\etc\passwd`, `music\..\..\x.flac`} {
		if _, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "p", Files: []File{flacFile(name, 1)}}); err == nil {
			t.Fatalf("traversal path %q accepted", name)
		}
	}
	if len(ts.batches) != 0 {
		t.Fatal("slskd was called with a traversal path")
	}
}

// TestEnqueueRejectsLockedFile proves a locked result cannot be grabbed.
func TestEnqueueRejectsLockedFile(t *testing.T) {
	r, _, _ := newReconciler(t, &transferServer{})
	f := flacFile(`music\Album\track.flac`, 10)
	f.IsLocked = true
	if _, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "p", Files: []File{f}}); err == nil {
		t.Fatal("locked file accepted")
	}
}

// TestEnqueueDeduplicates proves a second grab of the same (peer, file) attaches
// to the existing job instead of enqueueing a duplicate download.
func TestEnqueueDeduplicates(t *testing.T) {
	r, store, ts := newReconciler(t, &transferServer{})
	first, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.Job.ID != first.Job.ID {
		t.Fatalf("second enqueue did not attach: %+v", second)
	}
	if len(ts.batches) != 1 {
		t.Fatalf("%d batches posted, want 1", len(ts.batches))
	}
	jobs, _ := store.List()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(jobs))
	}
}

// TestEnqueueOfflinePeerFailsJob proves an enqueue rejection is recorded on the
// job, not just returned: the history must say why the bytes never arrived.
func TestEnqueueOfflinePeerFailsJob(t *testing.T) {
	r, store, _ := newReconciler(t, &transferServer{
		batchStatus: http.StatusNotFound,
		batchBody:   `"User peer1 appears to be offline"`,
	})
	_, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err == nil {
		t.Fatal("offline peer reported as success")
	}
	jobs, _ := store.List()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	if jobs[0].Status != JobFailed {
		t.Fatalf("status = %s, want FAILED", jobs[0].Status)
	}
	if !strings.Contains(jobs[0].FailureReason, "offline") {
		t.Fatalf("reason = %q", jobs[0].FailureReason)
	}
}

// TestPollClassifiesEveryTerminalFailure is the 0s-stall rule: all five
// failure-qualified states end polling immediately with the exact reason kept.
func TestPollClassifiesEveryTerminalFailure(t *testing.T) {
	for _, state := range []string{
		"Completed, Rejected",
		"Completed, TimedOut",
		"Completed, Errored",
		"Completed, Aborted",
		"Completed, Cancelled",
		"Cancelled",
	} {
		r, store, ts := newReconciler(t, &transferServer{})
		res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
		if err != nil {
			t.Fatal(err)
		}
		ts.mu.Lock()
		ts.transfers = []Transfer{{
			ID: "t1", BatchID: res.Job.BatchID, Username: "peer1",
			Filename: `music\a.flac`, State: state, Size: 10,
		}}
		ts.mu.Unlock()

		jobs, err := r.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].Status != JobFailed {
			t.Fatalf("%s: jobs = %+v", state, jobs)
		}
		if jobs[0].Items[0].FailureReason != state {
			t.Fatalf("%s: reason = %q, want the exact state", state, jobs[0].Items[0].FailureReason)
		}
		if pending, _ := store.Pending(); len(pending) != 0 {
			t.Fatalf("%s: job still pending after a terminal failure", state)
		}
	}
}

// TestPollMarksCompletedPendingImport proves a successful transfer stops at the
// import boundary rather than claiming the file was ingested.
func TestPollMarksCompletedPendingImport(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.transfers = []Transfer{{ID: "t1", BatchID: res.Job.BatchID, Username: "peer1", Filename: `music\a.flac`, State: "Completed, Succeeded", Size: 10}}
	ts.mu.Unlock()

	jobs, err := r.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != JobCompletedPendingImport {
		t.Fatalf("status = %+v, want COMPLETED_PENDING_IMPORT", jobs)
	}
}

// TestPollKeepsListeningWhileQueued proves a queued transfer stays pending.
func TestPollKeepsListeningWhileQueued(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.transfers = []Transfer{{ID: "t1", BatchID: res.Job.BatchID, Username: "peer1", Filename: `music\a.flac`, State: "Queued, Remotely", PlaceInQueue: intPtr(12)}}
	ts.mu.Unlock()
	jobs, err := r.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Status != JobInProgress {
		t.Fatalf("status = %s", jobs[0].Status)
	}
	if jobs[0].FailureReason != "" {
		t.Fatalf("queued job has a failure reason %q", jobs[0].FailureReason)
	}
}

// TestPollFallsBackToPeerFilename proves correlation survives a lost batch id
// (a slskd retention cleanup), which is what makes recovery possible.
func TestPollFallsBackToPeerFilename(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	if _, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}}); err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.transfers = []Transfer{{ID: "t9", Username: "peer1", Filename: `music\a.flac`, State: "Completed, Succeeded"}}
	ts.mu.Unlock()
	jobs, err := r.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Items[0].TransferID != "t9" {
		t.Fatalf("transfer id = %q", jobs[0].Items[0].TransferID)
	}
	if jobs[0].Status != JobCompletedPendingImport {
		t.Fatalf("status = %s", jobs[0].Status)
	}
}

// TestSnapshotCacheProvesReadsAreServedLocallyAndInvalidatedOnMutation pins the
// 2-3s snapshot contract and that a cancel is visible immediately.
func TestSnapshotCacheProvesReadsAreServedLocallyAndInvalidatedOnMutation(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	ctx := context.Background()
	if _, err := r.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if ts.listCalls != 1 {
		t.Fatalf("%d list calls within the TTL, want 1", ts.listCalls)
	}
	r.invalidateSnapshot()
	if _, err := r.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if ts.listCalls != 2 {
		t.Fatalf("%d list calls after invalidation, want 2", ts.listCalls)
	}
}

// TestCancelMarksJobCancelled proves cancel both talks to slskd and records the
// user's decision durably.
func TestCancelMarksJobCancelled(t *testing.T) {
	r, store, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.Cancel(context.Background(), res.Job.ID, true)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if job.Status != JobCancelled {
		t.Fatalf("status = %s", job.Status)
	}
	if len(ts.cancels) != 1 {
		t.Fatalf("%d cancel calls", len(ts.cancels))
	}
	if pending, _ := store.Pending(); len(pending) != 0 {
		t.Fatal("cancelled job still pending")
	}
}

// TestRetryAnotherSourceUsesAlternates proves the fallback is user-initiated and
// picks a different peer.
func TestRetryAnotherSourceUsesAlternates(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{
		Peer:  "peer1",
		Files: []File{flacFile(`music\a.flac`, 10)},
		Alternates: []Candidate{
			{Username: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}},
			{Username: "peer2", Files: []File{flacFile(`music\a.flac`, 10)}, HasFreeUploadSlot: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Update(res.Job.ID, func(j *Job) error { j.Status = JobFailed; return nil }); err != nil {
		t.Fatal(err)
	}
	retry, err := r.Retry(context.Background(), res.Job.ID, true)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retry.Job.Peer != "peer2" {
		t.Fatalf("retry peer = %q, want peer2", retry.Job.Peer)
	}
	if retry.Job.ID == res.Job.ID {
		t.Fatal("retry reused the old job id; history would be overwritten")
	}
	if len(ts.batches) != 2 {
		t.Fatalf("%d batches, want 2", len(ts.batches))
	}
}

// TestRetryWithoutAlternateFails proves a missing fallback is reported, not
// invented.
func TestRetryWithoutAlternateFails(t *testing.T) {
	r, _, _ := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Update(res.Job.ID, func(j *Job) error { j.Status = JobFailed; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Retry(context.Background(), res.Job.ID, true); err == nil {
		t.Fatal("retry with no alternate succeeded")
	}
}

// TestQueuePositionThrottling proves the probe cannot be spammed at one peer.
func TestQueuePositionThrottling(t *testing.T) {
	r, _, ts := newReconciler(t, &transferServer{})
	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.transfers = []Transfer{{ID: "t1", BatchID: res.Job.BatchID, Username: "peer1", Filename: `music\a.flac`, State: "Queued, Remotely"}}
	ts.mu.Unlock()
	r.posInterval = time.Minute
	if _, err := r.QueuePosition(context.Background(), res.Job.ID); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if _, err := r.QueuePosition(context.Background(), res.Job.ID); err != ErrPositionThrottled {
		t.Fatalf("second probe = %v, want ErrPositionThrottled", err)
	}
}

func intPtr(n int) *int { return &n }
