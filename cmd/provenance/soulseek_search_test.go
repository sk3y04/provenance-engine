package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/slskd"
)

// TestSelectGroupsPicksOnePeerFolder proves the selection rule: only the named
// peer's folders are enqueued, --dir narrows to one folder, and every other peer
// is kept as a ranked fallback.
func TestSelectGroupsPicksOnePeerFolder(t *testing.T) {
	groups := []slskd.ResultGroup{
		{Username: "fastpeer", Directory: "music/A", Files: []slskd.File{{Filename: `music\A\1.flac`, Size: 1}}, Score: 900},
		{Username: "slowpeer", Directory: "music/A", Files: []slskd.File{{Filename: `music\A\1.flac`, Size: 1}}, Score: 10},
		{Username: "slowpeer", Directory: "music/B", Files: []slskd.File{{Filename: `music\B\2.flac`, Size: 2}}, Score: 5},
	}
	selected, alternates, err := selectGroups(groups, "slowpeer", "", nil)
	if err != nil {
		t.Fatalf("selectGroups: %v", err)
	}
	if len(selected) != 2 {
		t.Fatalf("%d folders selected, want both of slowpeer's", len(selected))
	}
	if len(alternates) != 1 || alternates[0].Username != "fastpeer" {
		t.Fatalf("alternates = %+v", alternates)
	}

	selected, _, err = selectGroups(groups, "slowpeer", "music/B", nil)
	if err != nil {
		t.Fatalf("selectGroups with dir: %v", err)
	}
	if len(selected) != 1 || selected[0].Directory != "music/B" {
		t.Fatalf("dir selection = %+v", selected)
	}

	// An unknown peer is an actionable error, not an empty enqueue.
	if _, _, err := selectGroups(groups, "nobody", "", nil); err == nil {
		t.Fatal("unknown peer accepted")
	}
	// A peer with several folders and no --dir must say which ones exist.
	if _, _, err := selectGroups(groups, "slowpeer", "music/C", nil); err == nil ||
		!strings.Contains(err.Error(), "music/A") {
		t.Fatalf("ambiguous folder error = %v", err)
	}
}

// TestSelectGroupsFiltersFiles proves --file narrows to named files and that a
// file outside the peer's folder is not silently dropped into the wrong one.
func TestSelectGroupsFiltersFiles(t *testing.T) {
	groups := []slskd.ResultGroup{{
		Username:  "peer",
		Directory: "music/A",
		Files: []slskd.File{
			{Filename: `music\A\1.flac`, Size: 1},
			{Filename: `music\A\2.flac`, Size: 2},
		},
	}}
	selected, _, err := selectGroups(groups, "peer", "", []string{`music\A\2.flac`})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || len(selected[0].Files) != 1 || selected[0].Files[0].Size != 2 {
		t.Fatalf("file filter = %+v", selected)
	}
	if _, _, err := selectGroups(groups, "peer", "", []string{`music\A\missing.flac`}); err == nil {
		t.Fatal("missing file accepted")
	}
}

// TestSoulseekDownloadAndJobsCLI drives the real commands against a fake slskd:
// download must create a durable job, and jobs must show it.
func TestSoulseekDownloadAndJobsCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	mount := filepath.Join(dir, "downloads")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := fakeSlskdServer(t, mount)
	defer srv.Close()

	if _, err := runSoulseek(t, "settings", "set",
		"--base-url", srv.URL, "--api-key", "k", "--enable", "--downloads-mount", mount); err != nil {
		t.Fatalf("settings: %v", err)
	}

	out, err := runSoulseek(t, "download", "beethoven", "--peer", "peer1", "--timeout-ms", "5000", "--settle", "0s")
	if err != nil {
		t.Fatalf("download: %v\n%s", err, out)
	}
	if !strings.Contains(out, "queued prov-") {
		t.Fatalf("download output = %q", out)
	}

	out, err = runSoulseek(t, "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !strings.Contains(out, "IN_PROGRESS") && !strings.Contains(out, "COMPLETED_PENDING_IMPORT") {
		t.Fatalf("jobs output = %q", out)
	}
	if !strings.Contains(out, "peer1") {
		t.Fatalf("jobs output lost the peer: %q", out)
	}

	// A second identical grab attaches instead of double-enqueueing.
	out, err = runSoulseek(t, "download", "beethoven", "--peer", "peer1", "--timeout-ms", "5000", "--settle", "0s")
	if err != nil {
		t.Fatalf("second download: %v", err)
	}
	if !strings.Contains(out, "already queued") {
		t.Fatalf("duplicate grab output = %q", out)
	}
}

// TestSoulseekJobsWithoutMountFails proves the CLI refuses to guess where job
// state lives.
func TestSoulseekJobsWithoutMountFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if _, err := runSoulseek(t, "settings", "set", "--base-url", "http://127.0.0.1:5030", "--api-key", "k", "--enable"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSoulseek(t, "jobs"); err == nil {
		t.Fatal("jobs succeeded without a configured download mount")
	}
}

// fakeSlskdServer serves the three endpoints the download path needs, and
// simulates the download by writing the file into the pinned destination.
func fakeSlskdServer(t *testing.T, mount string) *httptest.Server {
	t.Helper()
	const remote = `music\Album\track.flac`
	const payload = "flac-bytes"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v0/searches"):
			_, _ = w.Write([]byte(`{"id":"s1","state":"InProgress","isComplete":false}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v0/searches/"):
			_, _ = w.Write([]byte(`{"id":"s1","isComplete":true,"state":"Completed, ResponseLimitReached","responses":[{"username":"peer1","uploadSpeed":900000,"hasFreeUploadSlot":true,"files":[{"filename":"music\\\\Album\\\\track.flac","size":10}]}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transfers/downloads/batches"):
			var req struct {
				ID      string `json:"id"`
				Options struct {
					Destination string `json:"destination"`
				} `json:"options"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			dest := filepath.Join(mount, filepath.FromSlash(req.Options.Destination), "track.flac")
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err := os.WriteFile(dest, []byte(payload), 0o644); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			_, _ = w.Write([]byte(`{"batch":{"id":"` + req.ID + `","username":"peer1","createdAt":"2026-01-01T00:00:00Z","transfers":[{"id":"t1","batchId":"` + req.ID + `","filename":"music\\\\Album\\\\track.flac","state":"Queued, Remotely"}]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transfers/downloads"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
}
