package slskd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func upload(id, user, file, state string, at time.Time) Transfer {
	return Transfer{ID: id, Username: user, Filename: file, State: state, RequestedAt: at, Size: 100}
}

// TestUploadsDedupeQueuedLocally covers #1609: slskd can report the same
// queued-locally upload twice, and a UI that shows both looks broken.
func TestUploadsDedupeQueuedLocally(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := []Transfer{
		upload("a1", "peer1", `music\track.flac`, "Queued, Locally", base),
		upload("a2", "peer1", `music\track.flac`, "Queued, Locally", base.Add(time.Second)),
		upload("a3", "peer2", `music\track.flac`, "Queued, Locally", base),
		upload("b1", "peer1", `music\other.flac`, "InProgress", base),
		upload("c1", "peer1", `music\done.flac`, "Completed, Succeeded", base),
	}
	out := DedupeUploads(in)
	if len(out) != 4 {
		t.Fatalf("%d uploads after dedupe, want 4: %+v", len(out), out)
	}
	// The earliest of the duplicate pair survives.
	for _, tr := range out {
		if tr.ID == "a2" {
			t.Fatal("the later duplicate was kept instead of the earlier one")
		}
	}
}

// TestUploadsSummarize pins the classification a status line depends on.
func TestUploadsSummarize(t *testing.T) {
	base := time.Now().UTC()
	sum := Summarize([]Transfer{
		upload("1", "p", "a", "InProgress", base),
		upload("2", "p", "b", "Queued, Remotely", base),
		upload("3", "p", "c", "Completed, Succeeded", base),
		upload("4", "p", "d", "Completed, Errored", base),
	})
	if sum.Active != 1 || sum.Queued != 1 || sum.Completed != 1 || sum.Failed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

// TestUploadsListAndCancel drives the HTTP side.
func TestUploadsListAndCancel(t *testing.T) {
	var canceled string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transfers/uploads"):
			writeJSON(w, 200, []any{
				map[string]any{"id": "u1", "username": "peer1", "filename": `music\a.flac`, "state": "InProgress", "size": 10},
				map[string]any{"id": "u2", "username": "peer1", "filename": `music\a.flac`, "state": "Queued, Locally", "size": 10},
				map[string]any{"id": "u3", "username": "peer1", "filename": `music\a.flac`, "state": "Queued, Locally", "size": 10},
			})
		case r.Method == http.MethodDelete:
			canceled = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})
	u := &Uploads{Client: client}

	list, err := u.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("%d uploads, want 2 after dedupe", len(list))
	}
	if err := u.Cancel(context.Background(), "peer1", "u1", true); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !strings.Contains(canceled, "/transfers/uploads/peer1/u1") {
		t.Fatalf("cancel path = %q", canceled)
	}
	if err := u.ClearCompleted(context.Background()); err != nil {
		t.Fatalf("ClearCompleted: %v", err)
	}
}

// TestUploadsRequireClient proves an unconfigured service fails loudly.
func TestUploadsRequireClient(t *testing.T) {
	u := &Uploads{}
	if _, err := u.List(context.Background()); err == nil {
		t.Fatal("List without a client succeeded")
	}
	if err := u.Cancel(context.Background(), "p", "i", true); err == nil {
		t.Fatal("Cancel without a client succeeded")
	}
	if err := u.ClearCompleted(context.Background()); err == nil {
		t.Fatal("ClearCompleted without a client succeeded")
	}
}
