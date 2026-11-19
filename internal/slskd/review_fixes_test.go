package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// TestCheckSettingsHealthSurvivesAnInvalidProfile guards a nil-client panic: the
// profile validator accepts a base URL string that the client constructor later
// rejects, and the error path used the nil client to redact its own message.
func TestCheckSettingsHealthSurvivesAnInvalidProfile(t *testing.T) {
	const key = "distinctive-api-key-value"
	s := DefaultSettings()
	s.Enabled = true
	s.BaseURL = "127.0.0.1:5030" // no scheme: Validate accepts, New rejects
	s.APIKey = key

	h := CheckSettingsHealth(context.Background(), s)
	if h.State != HealthAuthOrConfig {
		t.Fatalf("state = %s, want AUTH_OR_CONFIG_ERROR", h.State)
	}
	if len(h.Diagnostics) == 0 {
		t.Fatal("no diagnostic explaining the invalid profile")
	}
	// This path has no client to redact with, so it must redact from the profile.
	for _, d := range h.Diagnostics {
		if strings.Contains(d, key) {
			t.Fatalf("diagnostic leaked the API key: %q", d)
		}
	}
}

// TestEnqueuePartialBatchSettlesInsteadOfStalling covers a 207: some files are
// accepted, some are refused. A refused file never gets a transfer record, so
// without an explicit marker the job stays IN_PROGRESS forever and is polled for
// the rest of its life.
func TestEnqueuePartialBatchSettlesInsteadOfStalling(t *testing.T) {
	ts := &transferServer{
		batchStatus: http.StatusMultiStatus,
		batchBody: `{"batch":{"id":"b-partial","username":"peer1","transfers":[
			{"id":"t-ok","batchId":"b-partial","username":"peer1","filename":"music\\ok.flac","state":"Completed, Succeeded"}]},
			"failures":[{"filename":"music\\refused.flac","message":"file is locked"}]}`,
	}
	r, store, _ := newReconciler(t, ts)

	res, err := r.Enqueue(context.Background(), EnqueueRequest{
		Peer: "peer1",
		Files: []File{
			flacFile(`music\ok.flac`, 10),
			flacFile(`music\refused.flac`, 20),
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(res.Job.Items) != 2 {
		t.Fatalf("%d items", len(res.Job.Items))
	}
	if res.Job.Items[0].Rejected || !res.Job.Items[1].Rejected {
		t.Fatalf("rejected markers = %v/%v", res.Job.Items[0].Rejected, res.Job.Items[1].Rejected)
	}
	if !strings.Contains(res.Job.Items[1].FailureReason, "locked") {
		t.Fatalf("refused item reason = %q", res.Job.Items[1].FailureReason)
	}

	// The batch response already settles both files: one succeeded, one was
	// refused. The job must be terminal immediately rather than polling for a
	// transfer that will never exist.
	if res.Job.Status != JobFailed {
		t.Fatalf("status after a partial batch = %s, want FAILED", res.Job.Status)
	}
	if !strings.Contains(res.Job.FailureReason, "refused.flac") {
		t.Fatalf("job reason = %q, want the refused file named", res.Job.FailureReason)
	}

	// A later poll must not resurrect it.
	ts.mu.Lock()
	ts.transfers = []Transfer{{
		ID: "t-ok", BatchID: res.Job.BatchID, Username: "peer1",
		Filename: `music\ok.flac`, State: "Completed, Succeeded", Size: 10,
	}}
	ts.mu.Unlock()
	if _, err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	stored, err := store.Get(res.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != JobFailed {
		t.Fatalf("stored status = %s", stored.Status)
	}
	if pending, _ := store.Pending(); len(pending) != 0 {
		t.Fatalf("%d job(s) still pending after a partial batch", len(pending))
	}
}

// TestSearchStopKeepsFinalResponses proves a wall-clock stop does not discard the
// responses slskd accumulated: the last poll happens before the stop, so without a
// final fetch the user loses everything the peers sent in the final window.
func TestSearchStopKeepsFinalResponses(t *testing.T) {
	var stopped int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v0/searches"):
			_, _ = w.Write([]byte(`{"id":"s1","isComplete":false}`))
		case r.Method == http.MethodPut:
			atomic.StoreInt32(&stopped, 1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet:
			// Responses only become visible after the stop, which is what a slow
			// set of peers looks like: the poll that precedes the stop is empty.
			body := map[string]any{"id": "s1", "isComplete": false}
			if atomic.LoadInt32(&stopped) == 1 {
				body["responses"] = []any{map[string]any{
					"username": "latepeer", "uploadSpeed": 1000, "hasFreeUploadSlot": true,
					"files": []any{map[string]any{"filename": `music\late.flac`, "size": 5}},
				}}
			}
			writeJSON(w, 200, body)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	sc := NewSearchCoordinator(c)
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", WallClock: 80 * time.Millisecond,
		PollInitial: 20 * time.Millisecond, PollMax: 20 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.Stopped {
		t.Fatal("search was not stopped at the wall clock")
	}
	if len(res.Groups) != 1 || res.Groups[0].Username != "latepeer" {
		t.Fatalf("responses from the final window were lost: %+v", res.Groups)
	}
}

// TestIngestKeepsFilesWithTheSameBasenameApart proves two items that share a file
// name in different folders are not collapsed into one: the staged-file map was
// keyed by basename, so the second file overwrote the first and both items pointed
// at the same path.
func TestIngestKeepsFilesWithTheSameBasenameApart(t *testing.T) {
	mount := t.TempDir()
	dest := filepath.Join(mount, "provenance", "prov-0123456789abcdef")
	for _, sub := range []string{"AlbumA", "AlbumB"} {
		if err := os.MkdirAll(filepath.Join(dest, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, sub, "track.flac"), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ing := &Ingestor{Mount: mount}
	job := Job{
		ID: "prov-0123456789abcdef", Peer: "peer1", SearchText: "q",
		DestinationSubdir: "provenance/prov-0123456789abcdef",
		Status:            JobCompletedPendingImport,
		Items: []Item{
			{RemoteFilename: `music\AlbumA\track.flac`, Size: 10},
			{RemoteFilename: `music\AlbumB\track.flac`, Size: 10},
		},
	}
	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got.Status != JobImported {
		t.Fatalf("status = %s (%s)", got.Status, got.FailureReason)
	}
	if got.Items[0].LocalPath == got.Items[1].LocalPath {
		t.Fatalf("both items resolved to the same file: %q", got.Items[0].LocalPath)
	}
	if !strings.Contains(got.Items[0].LocalPath, "AlbumA") || !strings.Contains(got.Items[1].LocalPath, "AlbumB") {
		t.Fatalf("items were matched to the wrong folders: %q / %q", got.Items[0].LocalPath, got.Items[1].LocalPath)
	}
	if got.Items[0].CaptureItem == got.Items[1].CaptureItem {
		t.Fatalf("capture item identity collided: %q", got.Items[0].CaptureItem)
	}

	runs, err := manifest.ListRunManifests(mount)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	var cm struct {
		Items []struct {
			ExternalID     string `json:"external_id"`
			DownloadedPath string `json:"downloaded_path"`
		} `json:"items"`
	}
	data, err := os.ReadFile(runs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Items) != 2 {
		t.Fatalf("%d capture items, want 2", len(cm.Items))
	}
	if cm.Items[0].ExternalID == cm.Items[1].ExternalID {
		t.Fatalf("capture external ids collided: %q", cm.Items[0].ExternalID)
	}
	if cm.Items[0].DownloadedPath == cm.Items[1].DownloadedPath {
		t.Fatalf("both capture items point at %q", cm.Items[0].DownloadedPath)
	}
}

// TestIngestRefusesAPartialImport proves a job whose bytes are only partly on disk
// is not reported as IMPORTED: the files that are there would otherwise be
// archived while the missing ones vanish from the record.
func TestIngestRefusesAPartialImport(t *testing.T) {
	ing, _, dest := stagingRoot(t, map[string]string{"a.flac": "0123456789"})
	job := completedJob(t, dest,
		Item{RemoteFilename: `music\a.flac`, Size: 10},
		Item{RemoteFilename: `music\b.flac`, Size: 10},
	)
	got, err := ing.Ingest(context.Background(), job)
	if err == nil {
		t.Fatal("partial import reported as success")
	}
	if got.Status != JobImportFailed {
		t.Fatalf("status = %s", got.Status)
	}
	if !strings.Contains(got.FailureReason, "b.flac") {
		t.Fatalf("reason does not name the missing file: %q", got.FailureReason)
	}
}

// TestShareAddPrunesLinksForRemovedFiles proves re-adding a shrunken folder stops
// sharing what was deleted: the materialiser only ever added links, so a removed
// file stayed advertised by slskd indefinitely.
func TestShareAddPrunesLinksForRemovedFiles(t *testing.T) {
	m, src, root := shareFixture(t, map[string]string{
		"Album/keep.flac": "aaaa",
		"Album/drop.flac": "bbbb",
	})
	if _, _, err := m.Add(context.Background(), src, "Music"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Music", "Album", "drop.flac")); err != nil {
		t.Fatalf("link not created: %v", err)
	}
	if err := os.Remove(filepath.Join(src, "Album", "drop.flac")); err != nil {
		t.Fatal(err)
	}
	rule, _, err := m.Add(context.Background(), src, "Music")
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Music", "Album", "drop.flac")); !os.IsNotExist(err) {
		t.Fatal("a link for a deleted source file survived the re-add and is still shared")
	}
	if _, err := os.Stat(filepath.Join(root, "Music", "Album", "keep.flac")); err != nil {
		t.Fatal("a still-present file lost its link")
	}
	if rule.FileCount != 1 {
		t.Fatalf("file count = %d, want 1 after pruning", rule.FileCount)
	}
}

// TestHealthPermissionsWarning pins the #1760 check: an execute-less mode makes
// album directories untraversable, while 755/775 are exactly right.
func TestHealthPermissionsWarning(t *testing.T) {
	cases := map[string]bool{
		"755": false,
		"775": false,
		"644": true,
		"666": true,
		"777": true,
		"600": true,
	}
	for mode, wantWarning := range cases {
		opts := `{"shares":{"directories":[]},"flags":{},
		          "transfers":{"download":{"destination":{"permissions":{"mode":"` + mode + `"}}}}}`
		h := probe(t, &fakeSlskd{options: opts}, healthPaths(t))
		got := hasWarning(h, "#1760")
		if got != wantWarning {
			t.Errorf("mode %s: warning = %v, want %v (%+v)", mode, got, wantWarning, h.Warnings)
		}
	}
}

// TestWebhookRejectsOversizedBody keeps a sender from streaming an unbounded body
// into the receiver; truncation used to look like malformed JSON instead.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	h := newWebhookHandler(nil, nil)
	big := strings.Repeat("x", 80<<10)
	rec := postWebhook(t, h, "s3cret-value", `{"id":"big","type":"DownloadFileComplete","pad":"`+big+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}
}

// TestQueuePositionThrottleOnlyAfterSuccess proves a failed probe does not cost the
// user their next attempt: the rate limit exists to spare the peer, not to punish a
// transient error.
func TestQueuePositionThrottleOnlyAfterSuccess(t *testing.T) {
	var fail int32 = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"batch":{"id":"b1","username":"peer1","transfers":[{"id":"t1","batchId":"b1","filename":"music\\a.flac","state":"Queued, Remotely"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/position"):
			if atomic.LoadInt32(&fail) == 1 {
				http.Error(w, "peer timed out", http.StatusInternalServerError)
				return
			}
			writeJSON(w, 200, map[string]any{"id": "t1", "placeInQueue": 4, "state": "Queued, Remotely"})
		case strings.HasSuffix(r.URL.Path, "/transfers/downloads"):
			writeJSON(w, 200, []any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})
	store := mustOpenStore(t, t.TempDir())
	r := NewReconciler(c, store)
	r.posInterval = time.Minute

	res, err := r.Enqueue(context.Background(), EnqueueRequest{Peer: "peer1", Files: []File{flacFile(`music\a.flac`, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.QueuePosition(context.Background(), res.Job.ID); err == nil {
		t.Fatal("a failing probe reported success")
	}
	atomic.StoreInt32(&fail, 0)
	if _, err := r.QueuePosition(context.Background(), res.Job.ID); err != nil {
		t.Fatalf("the retry after a failed probe was throttled: %v", err)
	}
}

// TestQueuePositionUsesTheTransferShape pins the endpoint's real response. The
// Swagger says the position endpoint returns a Transfer, so the place arrives in
// placeInQueue; decoding a "position" field would succeed and always read zero.
func TestQueuePositionUsesTheTransferShape(t *testing.T) {
	withPlace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": "t1", "placeInQueue": 12, "state": "Queued, Remotely"})
	}))
	defer withPlace.Close()
	c, _ := New(Config{BaseURL: withPlace.URL, APIKey: "k"})
	p, err := c.QueuePosition(context.Background(), "peer1", "t1")
	if err != nil {
		t.Fatalf("QueuePosition: %v", err)
	}
	if !p.Known || p.Position != 12 {
		t.Fatalf("position = %+v, want known 12", p)
	}
	if p.State != "Queued, Remotely" {
		t.Fatalf("state = %q", p.State)
	}

	// A peer that has not answered yet must not read as position zero.
	noPlace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": "t1", "state": "Queued, Remotely"})
	}))
	defer noPlace.Close()
	c2, _ := New(Config{BaseURL: noPlace.URL, APIKey: "k"})
	p2, err := c2.QueuePosition(context.Background(), "peer1", "t1")
	if err != nil {
		t.Fatalf("QueuePosition: %v", err)
	}
	if p2.Known {
		t.Fatalf("an absent placeInQueue reported as known: %+v", p2)
	}
}

// TestIngestTwoJobsInOneSecondKeepBothCaptures covers manifest.RunPath's
// one-second granularity: two jobs finishing in the same second used to write the
// same run manifest, so archive import saw only one of them.
func TestIngestTwoJobsInOneSecondKeepBothCaptures(t *testing.T) {
	mount := t.TempDir()
	fixed := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ing := &Ingestor{Mount: mount, Now: func() time.Time { return fixed }}

	for _, id := range []string{"prov-1111111111111111", "prov-2222222222222222"} {
		dest := filepath.Join(mount, "provenance", id)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "track.flac"), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		job := Job{
			ID: id, Peer: "peer1", DestinationSubdir: "provenance/" + id,
			Status: JobCompletedPendingImport,
			Items:  []Item{{RemoteFilename: `music\track.flac`, Size: 10}},
		}
		if _, err := ing.Ingest(context.Background(), job); err != nil {
			t.Fatalf("ingest %s: %v", id, err)
		}
	}

	runs, err := manifest.ListRunManifests(mount)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("%d run manifests for two jobs in the same second, want 2: %v", len(runs), runs)
	}
	keys := map[string]bool{}
	for _, p := range runs {
		cm, err := manifest.ReadCaptureManifest(p)
		if err != nil {
			t.Fatal(err)
		}
		keys[cm.CaptureKey] = true
	}
	if len(keys) != 2 {
		t.Fatalf("capture keys = %v, want one per job", keys)
	}
}

// TestIngestCleanupTouchesOnlyThisJobsFiles proves --cleanup cannot delete
// anything it did not import: a custom destination can be shared, and a blanket
// RemoveAll would take other jobs' bytes with it.
func TestIngestCleanupTouchesOnlyThisJobsFiles(t *testing.T) {
	mount := t.TempDir()
	shared := filepath.Join(mount, "custom-dest")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "track.flac"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "other-job.flac"), []byte("someone else"), 0o644); err != nil {
		t.Fatal(err)
	}
	ing := &Ingestor{Mount: mount, Cleanup: true}
	job := Job{
		ID: "prov-3333333333333333", Peer: "peer1",
		DestinationSubdir: "custom-dest", Status: JobCompletedPendingImport,
		Items: []Item{{RemoteFilename: `music\track.flac`, Size: 10}},
	}
	got, err := ing.Ingest(context.Background(), job)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !got.CleanedUp {
		t.Fatal("cleanup not recorded")
	}
	if _, err := os.Stat(filepath.Join(shared, "track.flac")); !os.IsNotExist(err) {
		t.Fatal("the imported file was not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(shared, "other-job.flac")); err != nil {
		t.Fatalf("cleanup deleted a file it did not import: %v", err)
	}
}
