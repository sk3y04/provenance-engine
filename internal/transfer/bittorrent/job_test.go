package bittorrent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCaptureKeyIsStableAndContentBound: the idempotency key must be a function
// of job identity and selected output identity only. Anything time- or
// run-derived would make a restart unable to recognise a committed capture.
func TestCaptureKeyIsStableAndContentBound(t *testing.T) {
	a := Asset{Path: "fixture/a.bin", Length: 10, SHA256: strings.Repeat("a", 64)}
	b := Asset{Path: "fixture/b.bin", Length: 20, SHA256: strings.Repeat("b", 64)}

	base := captureKey("bt-abc", []Asset{a, b})
	if len(base) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars", len(base))
	}
	if got := captureKey("bt-abc", []Asset{b, a}); got != base {
		t.Error("key depends on asset order")
	}
	if got := captureKey("bt-other", []Asset{a, b}); got == base {
		t.Error("key ignores job identity")
	}
	if got := captureKey("bt-abc", []Asset{a}); got == base {
		t.Error("key ignores the selection")
	}

	digestChanged := a
	digestChanged.SHA256 = strings.Repeat("c", 64)
	if got := captureKey("bt-abc", []Asset{digestChanged, b}); got == base {
		t.Error("key ignores the artifact digest")
	}
	sizeChanged := a
	sizeChanged.Length = 9
	if got := captureKey("bt-abc", []Asset{sizeChanged, b}); got == base {
		t.Error("key ignores the artifact length")
	}
}

func magnetSource(t *testing.T, infohash string) source {
	t.Helper()
	src, err := parseSource("magnet:?xt=urn:btih:" + infohash)
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}
	return src
}

// TestJobRecordUpgradesFromV1: a job started by the previous build must resume
// under the new model, with nothing claimed as verified or captured.
func TestJobRecordUpgradesFromV1(t *testing.T) {
	dir := t.TempDir()
	ih := strings.Repeat("ab", 20)
	stage := time.Now().UTC().Add(-time.Hour)

	v1 := map[string]any{
		"format":      jobFormatV1,
		"job_id":      jobID(ih),
		"backend":     Backend,
		"info_hash":   ih,
		"source":      "magnet:?xt=urn:btih:" + ih,
		"staging_ref": "fixture",
		"selection":   []string{"fixture/a.bin"},
		"created_at":  stage,
		"updated_at":  stage,
	}
	data, err := json.MarshalIndent(v1, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.FromSlash(torrentStateDir), jobID(ih)+".job.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	job, err := readJob(path)
	if err != nil {
		t.Fatalf("readJob: %v", err)
	}
	if job.Format != JobFormatVersion {
		t.Errorf("format = %q, want %q", job.Format, JobFormatVersion)
	}
	want := JobState{Payload: PayloadPending, Capture: CapturePending, Seeding: SeedingStopped}
	if job.State != want {
		t.Errorf("state = %+v, want %+v", job.State, want)
	}
	if job.Payload != nil || job.Capture != nil {
		t.Error("an upgraded v1 record must not claim verified payload or a committed capture")
	}
	if job.Resume.MetainfoPath != metainfoRel(ih) || job.Resume.DataDir != "." || !job.Resume.PartFiles {
		t.Errorf("resume ref = %+v, want the backend's derived paths", job.Resume)
	}
	if !equalSelection(job.Selection, []string{"fixture/a.bin"}) {
		t.Errorf("selection = %v, want the v1 selection", job.Selection)
	}
}

// TestSelectionLockedAcrossVersions: the immutable selection is enforced on an
// upgraded record too, and a matching call rewrites it in the current format.
func TestSelectionLockedAcrossVersions(t *testing.T) {
	dir := t.TempDir()
	ih := strings.Repeat("cd", 20)
	s := &Session{staging: dir}
	src := magnetSource(t, ih)

	if _, err := s.loadOrCreateJob(src, []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := s.loadOrCreateJob(src, []string{"fixture/b.bin"}); !errors.Is(err, ErrSelectionLocked) {
		t.Fatalf("changed selection error = %v, want ErrSelectionLocked", err)
	}
	job, err := s.loadOrCreateJob(src, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("same selection: %v", err)
	}
	if job.Format != JobFormatVersion {
		t.Errorf("format = %q", job.Format)
	}
	onDisk, err := readJob(filepath.Join(dir, filepath.FromSlash(torrentStateDir), jobID(ih)+".job.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if onDisk.State != job.State || !equalSelection(onDisk.Selection, job.Selection) {
		t.Errorf("persisted record = %+v, want %+v", onDisk, job)
	}
}

// TestJobRecordRejectsForeignPaths: record paths are untrusted input and must
// stay confined to the job root, and the record must describe this torrent.
func TestJobRecordRejectsForeignPaths(t *testing.T) {
	dir := t.TempDir()
	ih := strings.Repeat("ef", 20)
	s := &Session{staging: dir}
	src := magnetSource(t, ih)
	job, err := s.loadOrCreateJob(src, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.validateJob(job, src.id); err != nil {
		t.Fatalf("healthy job rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*jobRecord)
		unsafe bool
	}{
		{"traversal metainfo", func(j *jobRecord) { j.Resume.MetainfoPath = "../evil.torrent" }, true},
		{"absolute metainfo", func(j *jobRecord) { j.Resume.MetainfoPath = "/etc/passwd" }, true},
		{"unexpected metainfo", func(j *jobRecord) { j.Resume.MetainfoPath = torrentStateDir + "/other.torrent" }, true},
		{"traversal data dir", func(j *jobRecord) { j.Resume.DataDir = "../../etc" }, true},
		{"foreign infohash", func(j *jobRecord) { j.InfoHash = strings.Repeat("11", 20) }, false},
		{"forged job id", func(j *jobRecord) { j.JobID = "bt-forged" }, false},
	}
	for _, tc := range cases {
		broken := *job
		broken.Resume = job.Resume
		tc.mutate(&broken)
		err := s.validateJob(&broken, src.id)
		if err == nil {
			t.Errorf("%s: accepted, want an error", tc.name)
			continue
		}
		if tc.unsafe && !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%s: error = %v, want ErrUnsafePath", tc.name, err)
		}
	}
}

// TestJobRecordStoresOnlySanitisedSource: a magnet can carry tracker URLs with
// passkeys. Neither the record nor anything it is logged or exported with may
// contain them.
func TestJobRecordStoresOnlySanitisedSource(t *testing.T) {
	dir := t.TempDir()
	ih := strings.Repeat("9a", 20)
	s := &Session{staging: dir}
	src, err := parseSource("magnet:?xt=urn:btih:" + ih + "&dn=Something&tr=" +
		"http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3DSECRET")
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}
	job, err := s.loadOrCreateJob(src, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if want := "magnet:?xt=urn:btih:" + ih; job.Source != want {
		t.Errorf("source = %q, want %q", job.Source, want)
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(torrentStateDir), jobID(ih)+".job.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET", "tracker.example", "passkey"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("job record contains %q", secret)
		}
	}
}

// TestMarkPayloadReadySortsAndPersists: the attestation is written sorted so the
// key derived from it is stable, and it survives a reload.
func TestMarkPayloadReadySortsAndPersists(t *testing.T) {
	dir := t.TempDir()
	ih := strings.Repeat("7b", 20)
	s := &Session{staging: dir}
	src := magnetSource(t, ih)
	job, err := s.loadOrCreateJob(src, []string{"fixture/b.bin", "fixture/a.bin"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if !equalSelection(job.Selection, []string{"fixture/a.bin", "fixture/b.bin"}) {
		t.Fatalf("selection = %v, want it sorted", job.Selection)
	}
	assets := []Asset{
		{Path: "fixture/b.bin", Length: 20, SHA256: strings.Repeat("b", 64)},
		{Path: "fixture/a.bin", Length: 10, SHA256: strings.Repeat("a", 64)},
	}
	if err := s.markPayloadReady(job, assets); err != nil {
		t.Fatalf("markPayloadReady: %v", err)
	}
	reloaded := jobRecordFor(t, dir, ih)
	if reloaded.State.Payload != PayloadReady {
		t.Errorf("payload state = %q, want ready", reloaded.State.Payload)
	}
	if reloaded.Payload == nil || len(reloaded.Payload.Assets) != 2 {
		t.Fatalf("payload = %+v", reloaded.Payload)
	}
	if reloaded.Payload.Assets[0].Path != "fixture/a.bin" {
		t.Errorf("assets not sorted: %+v", reloaded.Payload.Assets)
	}
	if key := captureKey(reloaded.JobID, reloaded.Payload.Assets); key != captureKey(job.JobID, assets) {
		t.Error("persisted attestation produces a different key")
	}
}
