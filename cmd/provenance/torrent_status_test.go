package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// statusFixture writes a v2 job record the way the backend does, so the CLI
// tests exercise the real read path rather than a struct.
func statusFixture(t *testing.T, root, infohash string, body map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, ".provenance", "torrent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record := map[string]any{
		"format":      "provenance/torrent-job+2",
		"job_id":      "bt-" + infohash,
		"backend":     "bittorrent",
		"info_hash":   infohash,
		"source":      "magnet:?xt=urn:btih:" + infohash,
		"staging_ref": "fixture",
		"selection":   []string{"fixture/a.bin"},
		"resume": map[string]any{
			"metainfo_path": ".provenance/torrent/" + infohash + ".torrent",
			"data_dir":      ".",
			"part_files":    true,
		},
		"state":      map[string]any{"payload": "ready", "capture": "committed", "seeding": "stopped"},
		"created_at": "2026-02-01T09:00:00Z",
		"updated_at": "2026-02-01T10:01:00Z",
	}
	for k, v := range body {
		record[k] = v
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "bt-"+infohash+".job.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func infohashHex(c byte) string { return strings.Repeat(string(c), 40) }

// TestTorrentStatusListJSON: the list is a JSON document on stdout, sorted, with
// a recorded_state object that carries no liveness claim.
func TestTorrentStatusListJSON(t *testing.T) {
	saveTorrentFlags(t)
	root := t.TempDir()
	statusFixture(t, root, infohashHex('b'), nil)
	statusFixture(t, root, infohashHex('a'), map[string]any{
		"capture": map[string]any{
			"key":          strings.Repeat("f", 64),
			"manifest":     ".provenance/runs/2026-02-01T100000Z.json",
			"committed_at": "2026-02-01T10:01:00Z",
		},
		"payload": map[string]any{
			"verified_at": "2026-02-01T10:00:00Z",
			"assets":      []map[string]any{{"path": "fixture/a.bin", "length": 10, "sha256": strings.Repeat("a", 64)}},
		},
	})
	flagTorrentOut = root
	flagTorrentJSON = true

	var out bytes.Buffer
	if err := runTorrentStatus(nil, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	var doc struct {
		StagingRoot string `json:"staging_root"`
		Complete    bool   `json:"complete"`
		Jobs        []struct {
			JobID         string   `json:"job_id"`
			InfoHash      string   `json:"info_hash"`
			Selection     []string `json:"selection"`
			UpdatedAt     string   `json:"updated_at"`
			RecordedState struct {
				Payload string `json:"payload"`
				Capture string `json:"capture"`
				Seeding string `json:"seeding"`
			} `json:"recorded_state"`
			Payload *struct {
				Files  int `json:"files"`
				Assets []struct {
					Path   string `json:"path"`
					Length int64  `json:"length"`
					SHA256 string `json:"sha256"`
				} `json:"assets"`
			} `json:"payload"`
			Capture *struct {
				Manifest string `json:"manifest"`
				Key      string `json:"capture_key"`
			} `json:"capture"`
		} `json:"jobs"`
		Unreadable []any `json:"unreadable"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	if doc.StagingRoot == "" {
		t.Error("staging_root missing")
	}
	if len(doc.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(doc.Jobs))
	}
	if doc.Jobs[0].InfoHash != infohashHex('a') || doc.Jobs[1].InfoHash != infohashHex('b') {
		t.Errorf("jobs are not sorted by job id: %v", doc.Jobs)
	}
	first := doc.Jobs[0]
	if first.RecordedState.Payload != "ready" || first.RecordedState.Capture != "committed" {
		t.Errorf("recorded_state = %+v", first.RecordedState)
	}
	if !doc.Complete {
		t.Error("a listing with no unreadable records reported complete=false")
	}
	if first.Payload == nil || first.Payload.Files != 1 {
		t.Errorf("payload projection = %+v", first.Payload)
	}
	if first.Payload != nil {
		if len(first.Payload.Assets) != 1 {
			t.Fatalf("payload assets = %+v", first.Payload.Assets)
		}
		asset := first.Payload.Assets[0]
		if asset.Path != "fixture/a.bin" || asset.Length != 10 || asset.SHA256 != strings.Repeat("a", 64) {
			t.Errorf("asset = %+v", asset)
		}
	}
	if first.Capture == nil || first.Capture.Manifest == "" || first.Capture.Key == "" {
		t.Errorf("capture projection = %+v", first.Capture)
	}
	if first.UpdatedAt != "2026-02-01T10:01:00Z" {
		t.Errorf("updated_at = %q", first.UpdatedAt)
	}
	for _, forbidden := range []string{"live", "running", "alive"} {
		if strings.Contains(strings.ToLower(out.String()), `"`+forbidden+`"`) {
			t.Errorf("JSON claims liveness with key %q", forbidden)
		}
	}
	if len(doc.Unreadable) != 0 {
		t.Errorf("unreadable = %v, want none", doc.Unreadable)
	}
}

// TestTorrentStatusSingleJSON: one job by id is a single object.
func TestTorrentStatusSingleJSON(t *testing.T) {
	saveTorrentFlags(t)
	root := t.TempDir()
	statusFixture(t, root, infohashHex('c'), nil)
	flagTorrentOut = root
	flagTorrentJSON = true

	var out bytes.Buffer
	if err := runTorrentStatus([]string{"bt-" + infohashHex('c')}, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	var job struct {
		JobID         string `json:"job_id"`
		InfoHash      string `json:"info_hash"`
		RecordFormat  string `json:"record_format"`
		RecordedState struct {
			Seeding string `json:"seeding"`
		} `json:"recorded_state"`
		Capture *struct {
			Manifest string `json:"manifest"`
		} `json:"capture"`
	}
	if err := json.Unmarshal(out.Bytes(), &job); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out.String())
	}
	if job.JobID != "bt-"+infohashHex('c') || job.InfoHash != infohashHex('c') {
		t.Errorf("job = %+v", job)
	}
	if job.RecordFormat != "provenance/torrent-job+2" {
		t.Errorf("record_format = %q", job.RecordFormat)
	}
	if job.Capture != nil {
		t.Errorf("record without a capture projected one: %+v", job.Capture)
	}
	// The bare infohash is the same lookup.
	out.Reset()
	if err := runTorrentStatus([]string{infohashHex('c')}, &out); err != nil {
		t.Fatalf("status by bare infohash: %v", err)
	}
}

// TestTorrentStatusHumanLabelsRecorded: the human output must say the state is
// recorded and must not imply the job is running.
func TestTorrentStatusHumanLabelsRecorded(t *testing.T) {
	saveTorrentFlags(t)
	root := t.TempDir()
	statusFixture(t, root, infohashHex('d'), map[string]any{
		"state": map[string]any{"payload": "ready", "capture": "committed", "seeding": "seeding"},
		"capture": map[string]any{
			"key":          strings.Repeat("f", 64),
			"manifest":     ".provenance/runs/2026-02-01T100000Z.json",
			"committed_at": "2026-02-01T10:01:00Z",
		},
	})
	flagTorrentOut = root
	flagTorrentJSON = false

	var list bytes.Buffer
	if err := runTorrentStatus(nil, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	text := list.String()
	for _, want := range []string{
		"payload=ready", "capture=committed", "seeding=seeding",
		"not a liveness check", "recorded", ".provenance/runs/2026-02-01T100000Z.json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("list output missing %q:\n%s", want, text)
		}
	}

	var one bytes.Buffer
	if err := runTorrentStatus([]string{"bt-" + infohashHex('d')}, &one); err != nil {
		t.Fatalf("single: %v", err)
	}
	detail := one.String()
	for _, want := range []string{"recorded state", "this is not a liveness check", "manifest", "capture key"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail output missing %q:\n%s", want, detail)
		}
	}
}

// TestTorrentStatusEmptyRoot: an untouched root is not an error.
func TestTorrentStatusEmptyRoot(t *testing.T) {
	saveTorrentFlags(t)
	flagTorrentOut = t.TempDir()
	flagTorrentJSON = false

	var out bytes.Buffer
	if err := runTorrentStatus(nil, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "no torrent jobs recorded") {
		t.Errorf("output = %q", out.String())
	}
	if entries, err := os.ReadDir(flagTorrentOut); err != nil || len(entries) != 0 {
		t.Errorf("status created files: %v %v", entries, err)
	}
}

// TestTorrentStatusMissingAndInvalidID: both are ordinary command errors, which
// the CLI turns into exit code 1.
func TestTorrentStatusMissingAndInvalidID(t *testing.T) {
	saveTorrentFlags(t)
	flagTorrentOut = t.TempDir()

	var out bytes.Buffer
	if err := runTorrentStatus([]string{"bt-" + infohashHex('e')}, &out); !errors.Is(err, bittorrent.ErrJobNotFound) {
		t.Errorf("missing id error = %v, want ErrJobNotFound", err)
	}
	for _, bad := range []string{"../etc/passwd", "/abs", "not-a-job", ""} {
		if err := runTorrentStatus([]string{bad}, &out); !errors.Is(err, bittorrent.ErrInvalidJobID) {
			t.Errorf("id %q error = %v, want ErrInvalidJobID", bad, err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("failed lookups wrote to stdout: %q", out.String())
	}
}

// TestTorrentStatusUnreadableKeepsJSONValid: a bad record fails the command but
// must not corrupt the JSON document, and the diagnostic stays off stdout.
func TestTorrentStatusUnreadableKeepsJSONValid(t *testing.T) {
	saveTorrentFlags(t)
	root := t.TempDir()
	statusFixture(t, root, infohashHex('a'), nil)
	dir := filepath.Join(root, ".provenance", "torrent")
	if err := os.WriteFile(filepath.Join(dir, "broken.job.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	flagTorrentOut = root
	flagTorrentJSON = true

	var out bytes.Buffer
	err := runTorrentStatus(nil, &out)
	if err == nil {
		t.Fatal("listing with an unreadable record reported success")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error = %v, want it to say the listing is incomplete", err)
	}
	var doc struct {
		Complete   bool             `json:"complete"`
		Jobs       []map[string]any `json:"jobs"`
		Unreadable []struct {
			File   string `json:"file"`
			Reason string `json:"reason"`
		} `json:"unreadable"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil {
		t.Fatalf("stdout is not valid JSON despite an unreadable record: %v\n%s", jerr, out.String())
	}
	if len(doc.Jobs) != 1 || len(doc.Unreadable) != 1 {
		t.Fatalf("document = %+v", doc)
	}
	if doc.Complete {
		t.Error("document with an unreadable record reported complete=true")
	}
	if doc.Unreadable[0].File != "broken.job.json" || doc.Unreadable[0].Reason == "" {
		t.Errorf("unreadable entry = %+v", doc.Unreadable[0])
	}
	if strings.Contains(out.String(), "[provenance] warning") {
		t.Error("diagnostic leaked into JSON stdout")
	}
}

// TestTorrentStatusCommandWiring: the subcommand exists, shares the staging-root
// flag, and propagates errors so main can exit non-zero.
func TestTorrentStatusCommandWiring(t *testing.T) {
	saveTorrentFlags(t)
	cmd := torrentCmd()
	var status *cobra.Command
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			status = sub
		}
	}
	if status == nil {
		t.Fatal("torrent status subcommand is missing")
	}
	if status.Flags().Lookup("json") == nil {
		t.Error("status has no --json flag")
	}
	if cmd.PersistentFlags().Lookup("output") == nil {
		t.Error("torrent group has no --output flag")
	}
	cmd.SetArgs([]string{"-o", t.TempDir(), "status", "not-an-id"})
	if err := cmd.Execute(); !errors.Is(err, bittorrent.ErrInvalidJobID) {
		t.Fatalf("Execute error = %v, want ErrInvalidJobID", err)
	}
}
