package slskd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// fakePeer is a slskd instance good enough for the whole flow: it answers
// searches, accepts a batch, "downloads" by writing the file into the pinned
// destination, and then reports the transfer as succeeded.
type fakePeer struct {
	mu        sync.Mutex
	mount     string
	filename  string
	payload   []byte
	searches  int
	batches   []map[string]any
	transfers []Transfer
	searched  bool
}

func (f *fakePeer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v0/searches"):
			f.mu.Lock()
			f.searches++
			f.searched = true
			f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"id": "s-e2e", "state": "InProgress", "isComplete": false})

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v0/searches/"):
			f.mu.Lock()
			done := f.searched
			f.mu.Unlock()
			body := map[string]any{"id": "s-e2e", "isComplete": done, "state": "InProgress"}
			if done {
				body["state"] = "Completed, ResponseLimitReached"
				body["responses"] = []any{map[string]any{
					"username": "peer1", "uploadSpeed": 900000, "hasFreeUploadSlot": true, "queueLength": 0,
					"files": []any{map[string]any{"filename": f.filename, "size": len(f.payload)}},
				}}
			}
			writeJSON(w, 200, body)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transfers/downloads/batches"):
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			opts, _ := req["options"].(map[string]any)
			dest, _ := opts["destination"].(string)
			externalID, _ := opts["externalId"].(string)
			batchID, _ := req["id"].(string)

			// Simulate slskd writing the completed file into the pinned
			// destination on the shared volume.
			dir := filepath.Join(f.mount, filepath.FromSlash(dest), "music", "Album")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t := err.Error()
				http.Error(w, t, http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(filepath.Join(dir, "track.flac"), f.payload, 0o644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			f.mu.Lock()
			f.batches = append(f.batches, req)
			f.transfers = []Transfer{{
				ID: "t-e2e", BatchID: batchID, Username: "peer1",
				Filename: f.filename, Size: int64(len(f.payload)), State: "Completed, Succeeded",
			}}
			f.mu.Unlock()
			writeJSON(w, http.StatusCreated, map[string]any{
				"batch": map[string]any{"id": batchID, "username": "peer1", "createdAt": time.Now().UTC(),
					"transfers": []any{map[string]any{"id": "t-e2e", "batchId": batchID, "filename": f.filename, "state": "Queued, Remotely"}}},
			})
			_ = externalID

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transfers/downloads"):
			f.mu.Lock()
			list := append([]Transfer(nil), f.transfers...)
			f.mu.Unlock()
			writeJSON(w, 200, list)

		default:
			http.NotFound(w, r)
		}
	})
}

// TestEndToEndSearchEnqueuePollIngest is the M2 exit gate: one flow from a search
// to a provenance-stamped capture on disk, driven only through the public API.
func TestEndToEndSearchEnqueuePollIngest(t *testing.T) {
	mount := t.TempDir()
	peer := &fakePeer{mount: mount, filename: `music\Album\track.flac`, payload: []byte("flac-bytes-here")}
	srv := httptest.NewServer(peer.handler())
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewSearchCoordinator(client)
	store := mustOpenStore(t, mount)
	reconciler := NewReconciler(client, store)
	ingestor := &Ingestor{Mount: mount}
	ingestor.AttachStore(store)

	ctx := context.Background()

	// 1. Search.
	res, err := coordinator.Search(ctx, SearchOptions{
		Query: "beethoven", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("%d groups", len(res.Groups))
	}
	group := res.Groups[0]
	if group.Username != "peer1" || len(group.Files) != 1 {
		t.Fatalf("group = %+v", group)
	}

	// 2. Enqueue.
	enq, err := reconciler.Enqueue(ctx, EnqueueRequest{
		SearchID:   res.SearchID,
		SearchText: res.Query,
		Peer:       group.Username,
		Files:      group.Files,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if enq.Job.Status != JobInProgress {
		t.Fatalf("status after enqueue = %s", enq.Job.Status)
	}

	// 3. Poll until the transfer is complete.
	jobs, err := reconciler.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != JobCompletedPendingImport {
		t.Fatalf("poll status = %+v", jobs)
	}

	// 4. Ingest.
	imported, err := ingestor.Ingest(ctx, jobs[0])
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if imported.Status != JobImported {
		t.Fatalf("status = %s (%s)", imported.Status, imported.FailureReason)
	}
	if imported.Items[0].Sha256 == "" {
		t.Fatal("no hash recorded")
	}

	// 5. The bytes and the provenance record are both on disk.
	runs, err := manifest.ListRunManifests(mount)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, err = %v", runs, err)
	}
	cm, err := manifest.ReadCaptureManifest(runs[0])
	if err != nil {
		t.Fatal(err)
	}
	if cm.Site != "soulseek" || len(cm.Items) != 1 {
		t.Fatalf("manifest = %+v", cm)
	}
	if cm.Items[0].Extractor != "slskd" || cm.Items[0].Author != "peer1" {
		t.Fatalf("capture item = %+v", cm.Items[0])
	}

	results, err := manifest.VerifyCaptureDir(mount)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("verification = %+v", results)
	}

	// 6. The durable job agrees, and nothing is left pending.
	stored, err := store.Get(imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != JobImported {
		t.Fatalf("stored status = %s", stored.Status)
	}
	if pending, _ := store.Pending(); len(pending) != 0 {
		t.Fatalf("pending = %d", len(pending))
	}
}
