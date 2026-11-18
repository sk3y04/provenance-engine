package bittorrent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
)

// jobFixture builds a valid v2 record for tests. Callers mutate one field to
// describe the case they care about.
func jobFixture(ih string, mutate func(*jobRecord)) *jobRecord {
	job := &jobRecord{
		Format:     JobFormatVersion,
		JobID:      jobID(ih),
		Backend:    Backend,
		InfoHash:   ih,
		Source:     "magnet:?xt=urn:btih:" + ih,
		StagingRef: "fixture",
		Selection:  []string{"fixture/a.bin"},
		Resume:     resumeRef{MetainfoPath: metainfoRel(ih), DataDir: ".", PartFiles: true},
		State:      JobState{Payload: PayloadReady, Capture: CaptureCommitted, Seeding: SeedingStopped},
		Payload: &payloadRecord{
			VerifiedAt: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC),
			Assets:     []Asset{{Path: "fixture/a.bin", Length: 10, SHA256: strings.Repeat("a", 64)}},
		},
		Capture: &captureRecord{
			Key:         strings.Repeat("f", 64),
			Manifest:    ".provenance/runs/2026-02-01T100000Z.json",
			CommittedAt: time.Date(2026, 2, 1, 10, 1, 0, 0, time.UTC),
		},
		CreatedAt: time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 2, 1, 10, 1, 0, 0, time.UTC),
	}
	if mutate != nil {
		mutate(job)
	}
	return job
}

// writeJobFixture persists a record under the canonical name for its id and
// returns the path.
func writeJobFixture(t *testing.T, root string, job *jobRecord) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	path := filepath.Join(dir, job.JobID+".job.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func hashOf(prefix byte) string { return strings.Repeat(string(prefix), 40) }

// TestListJobsDeterministicOrderAndProjection: listing is sorted by job id
// regardless of write order, and the projection carries the recorded facts.
func TestListJobsDeterministicOrderAndProjection(t *testing.T) {
	root := t.TempDir()
	ihC, ihA, ihB := hashOf('c'), hashOf('a'), hashOf('b')
	writeJobFixture(t, root, jobFixture(ihC, nil))
	writeJobFixture(t, root, jobFixture(ihA, func(j *jobRecord) {
		j.Selection = []string{"fixture/a.bin", "fixture/b.bin"}
		j.State = JobState{Payload: PayloadReady, Capture: CaptureCommitted, Seeding: SeedingActive}
	}))
	writeJobFixture(t, root, jobFixture(ihB, func(j *jobRecord) {
		j.Payload = nil
		j.Capture = nil
		j.State = JobState{Payload: PayloadPending, Capture: CapturePending, Seeding: SeedingStopped}
	}))

	listing, err := ListJobs(root)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if listing.StagingRoot == "" || !filepath.IsAbs(listing.StagingRoot) {
		t.Errorf("staging root = %q, want an absolute path", listing.StagingRoot)
	}
	if len(listing.Jobs) != 3 || len(listing.Unreadable) != 0 {
		t.Fatalf("jobs = %d, unreadable = %d", len(listing.Jobs), len(listing.Unreadable))
	}
	wantOrder := []string{jobID(ihA), jobID(ihB), jobID(ihC)}
	for i, want := range wantOrder {
		if listing.Jobs[i].JobID != want {
			t.Fatalf("job %d = %s, want %s (order %v)", i, listing.Jobs[i].JobID, want, listing.Jobs)
		}
	}

	first := listing.Jobs[0]
	if first.InfoHash != ihA {
		t.Errorf("infohash = %s, want %s", first.InfoHash, ihA)
	}
	if len(first.Selection) != 2 || first.Selection[0] != "fixture/a.bin" {
		t.Errorf("selection = %v", first.Selection)
	}
	if first.RecordedState.Seeding != SeedingActive {
		t.Errorf("recorded seeding = %q", first.RecordedState.Seeding)
	}
	if first.Payload == nil || first.Payload.Files != 1 {
		t.Errorf("payload projection = %+v", first.Payload)
	}
	if first.Capture == nil || first.Capture.Manifest != ".provenance/runs/2026-02-01T100000Z.json" {
		t.Errorf("capture projection = %+v", first.Capture)
	}
	if first.RecordFormat != JobFormatVersion {
		t.Errorf("record format = %q", first.RecordFormat)
	}
	if !first.UpdatedAt.Equal(time.Date(2026, 2, 1, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("updated_at = %s", first.UpdatedAt)
	}

	// A second listing must produce the same order.
	again, err := ListJobs(root)
	if err != nil {
		t.Fatalf("ListJobs again: %v", err)
	}
	for i := range again.Jobs {
		if again.Jobs[i].JobID != listing.Jobs[i].JobID {
			t.Fatalf("order changed between listings: %v vs %v", again.Jobs[i].JobID, listing.Jobs[i].JobID)
		}
	}
}

// TestListJobsEmptyRootCreatesNothing: status is read-only, so an untouched
// staging root lists nothing and gains no files.
func TestListJobsEmptyRootCreatesNothing(t *testing.T) {
	root := t.TempDir()
	listing, err := ListJobs(root)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(listing.Jobs) != 0 || len(listing.Unreadable) != 0 {
		t.Fatalf("listing = %+v, want empty", listing)
	}
	if listing.Jobs == nil {
		t.Error("Jobs must be non-nil so JSON renders [] rather than null")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("status created entries in the root: %v (err %v)", entries, err)
	}
}

// TestListJobsReportsUnreadable: a list that skips a bad record must say so,
// with a reason that does not echo the record's contents.
func TestListJobsReportsUnreadable(t *testing.T) {
	root := t.TempDir()
	good := hashOf('1')
	writeJobFixture(t, root, jobFixture(good, nil))

	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	if err := os.WriteFile(filepath.Join(dir, "broken.job.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsupported := jobFixture(hashOf('2'), func(j *jobRecord) { j.Format = "provenance/torrent-job+9" })
	data, _ := json.Marshal(unsupported)
	if err := os.WriteFile(filepath.Join(dir, unsupported.JobID+".job.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid record filed under the wrong name must not be silently trusted.
	misnamed := jobFixture(hashOf('3'), nil)
	data, _ = json.Marshal(misnamed)
	if err := os.WriteFile(filepath.Join(dir, "planted.job.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	listing, err := ListJobs(root)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(listing.Jobs) != 1 || listing.Jobs[0].JobID != jobID(good) {
		t.Fatalf("jobs = %+v, want only the good record", listing.Jobs)
	}
	if len(listing.Unreadable) != 3 {
		t.Fatalf("unreadable = %+v, want 3", listing.Unreadable)
	}
	wantFiles := []string{"broken.job.json", unsupported.JobID + ".job.json", "planted.job.json"}
	for i, want := range wantFiles {
		if listing.Unreadable[i].File != want {
			t.Errorf("unreadable[%d] = %s, want %s (sorted by file)", i, listing.Unreadable[i].File, want)
		}
		if strings.TrimSpace(listing.Unreadable[i].Reason) == "" {
			t.Errorf("unreadable[%d] has no reason", i)
		}
	}
	for _, bad := range listing.Unreadable {
		if strings.Contains(bad.Reason, "not json") {
			t.Errorf("reason echoes record content: %q", bad.Reason)
		}
	}
}

// TestReadJobStatusByValidatedID: lookup accepts the canonical id and the bare
// infohash, and rejects anything that is not an identity before touching the
// filesystem.
func TestReadJobStatusByValidatedID(t *testing.T) {
	root := t.TempDir()
	ih := hashOf('a')
	writeJobFixture(t, root, jobFixture(ih, nil))

	for _, in := range []string{jobID(ih), ih, strings.ToUpper(jobID(ih)), "  " + ih + "  "} {
		status, err := ReadJobStatus(root, in)
		if err != nil {
			t.Fatalf("ReadJobStatus(%q): %v", in, err)
		}
		if status.JobID != jobID(ih) || status.InfoHash != ih {
			t.Errorf("ReadJobStatus(%q) = %s / %s", in, status.JobID, status.InfoHash)
		}
	}

	if _, err := ReadJobStatus(root, hashOf('b')); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("missing id error = %v, want ErrJobNotFound", err)
	}

	bad := []string{
		"", "..", "../x", "/etc/passwd", "bt-", "bt-abc",
		"bt-" + strings.Repeat("g", 40),
		"bt-" + strings.Repeat("a", 39),
		"bt-" + strings.Repeat("a", 40) + "/../x",
		filepath.Join(root, jobID(ih)+".job.json"),
	}
	for _, in := range bad {
		if _, err := ReadJobStatus(root, in); !errors.Is(err, ErrInvalidJobID) {
			t.Errorf("ReadJobStatus(%q) error = %v, want ErrInvalidJobID", in, err)
		}
	}
}

// TestStatusRootMustExist: a missing root is an error, and status must not
// create it.
func TestStatusRootMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := ListJobs(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("ListJobs(missing) = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("status created the missing root: %v", err)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListJobs(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("ListJobs(file) = %v", err)
	}
	if _, err := ReadJobStatus(file, hashOf('a')); err == nil {
		t.Error("ReadJobStatus accepted a file as a staging root")
	}
}

// TestStatusReadsV1WithoutRewriting: an old record is readable, reports the
// format it actually has on disk, and is left byte-identical.
func TestStatusReadsV1WithoutRewriting(t *testing.T) {
	root := t.TempDir()
	ih := hashOf('d')
	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := map[string]any{
		"format":      jobFormatV1,
		"job_id":      jobID(ih),
		"backend":     Backend,
		"info_hash":   ih,
		"source":      "magnet:?xt=urn:btih:" + ih,
		"staging_ref": "fixture",
		"selection":   []string{"fixture/a.bin"},
		"created_at":  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		"updated_at":  time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	data, err := json.MarshalIndent(v1, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, jobID(ih)+".job.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	status, err := ReadJobStatus(root, jobID(ih))
	if err != nil {
		t.Fatalf("ReadJobStatus(v1): %v", err)
	}
	if status.RecordFormat != jobFormatV1 {
		t.Errorf("record format = %q, want the on-disk v1 label", status.RecordFormat)
	}
	want := JobState{Payload: PayloadPending, Capture: CapturePending, Seeding: SeedingStopped}
	if status.RecordedState != want {
		t.Errorf("recorded state = %+v, want %+v", status.RecordedState, want)
	}
	if status.Payload != nil || status.Capture != nil {
		t.Errorf("v1 record projected payload/capture: %+v / %+v", status.Payload, status.Capture)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("status rewrote the v1 record")
	}
}

// TestStatusOmitsSecrets: records are not dumped, and nothing that reaches the
// projection or an error message can carry a tracker URL, passkey, magnet, or
// metainfo bytes.
func TestStatusOmitsSecrets(t *testing.T) {
	root := t.TempDir()
	ih := hashOf('e')
	secret := "http://tracker.example/announce?passkey=SECRET"
	writeJobFixture(t, root, jobFixture(ih, func(j *jobRecord) {
		// A hand-edited record could carry a full magnet; the projection must
		// not echo it.
		j.Source = "magnet:?xt=urn:btih:" + ih + "&tr=" + secret
	}))

	status, err := ReadJobStatus(root, jobID(ih))
	if err != nil {
		t.Fatalf("ReadJobStatus: %v", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"SECRET", "tracker.example", "passkey", "magnet:", "&tr="} {
		if strings.Contains(string(encoded), needle) {
			t.Errorf("projection contains %q: %s", needle, encoded)
		}
	}

	// A record whose raw bytes contain the secret must not leak it through the
	// decoder error either.
	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	if err := os.WriteFile(filepath.Join(dir, "leaky.job.json"),
		[]byte(`{"format":"`+JobFormatVersion+`","info_hash":"`+ih+`","source":"`+secret+`",`), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := ListJobs(root)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, bad := range listing.Unreadable {
		if strings.Contains(bad.Reason, "SECRET") || strings.Contains(bad.Reason, "passkey") {
			t.Errorf("unreadable reason leaks a secret: %q", bad.Reason)
		}
	}
	if len(listing.Unreadable) != 1 {
		t.Fatalf("unreadable = %+v, want the malformed record", listing.Unreadable)
	}
}

// TestStatusRejectsUnsafePathsInRecords: record paths are untrusted on read too.
func TestStatusRejectsUnsafePathsInRecords(t *testing.T) {
	root := t.TempDir()
	ih := hashOf('f')

	writeJobFixture(t, root, jobFixture(ih, func(j *jobRecord) {
		j.Resume.MetainfoPath = "../evil.torrent"
	}))
	if _, err := ReadJobStatus(root, jobID(ih)); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("traversal metainfo path error = %v, want ErrUnsafePath", err)
	}

	other := hashOf('9')
	writeJobFixture(t, root, jobFixture(other, func(j *jobRecord) {
		j.StagingRef = "../escape"
	}))
	_, err := ReadJobStatus(root, jobID(other))
	if !errors.Is(err, ErrUnsafePath) {
		t.Errorf("traversal staging ref error = %v, want ErrUnsafePath", err)
	}
	if err != nil && strings.Contains(err.Error(), "escape") {
		t.Errorf("error echoes the unsafe value: %v", err)
	}
}

// TestReadDuringAtomicReplacement: records are published by rename, so a reader
// must always observe one complete version. The writer alternates two records
// that differ in every field a reader projects.
func TestReadDuringAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	ih := hashOf('7')
	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, jobID(ih)+".job.json")

	variantA := jobFixture(ih, func(j *jobRecord) {
		j.UpdatedAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		j.Selection = []string{"fixture/a.bin"}
	})
	variantB := jobFixture(ih, func(j *jobRecord) {
		j.UpdatedAt = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
		j.Selection = []string{"fixture/a.bin", "fixture/b.bin"}
		j.State.Seeding = SeedingActive
	})
	dataA, _ := json.MarshalIndent(variantA, "", "  ")
	dataB, _ := json.MarshalIndent(variantB, "", "  ")
	if err := atomicfile.WriteFile(path, dataA, 0o600); err != nil {
		t.Fatal(err)
	}

	const iterations = 300
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			body := dataA
			if i%2 == 1 {
				body = dataB
			}
			if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
		close(stop)
	}()

	reads := 0
	for {
		select {
		case <-stop:
			wg.Wait()
			if reads == 0 {
				t.Fatal("no reads completed")
			}
			return
		default:
		}
		status, err := ReadJobStatus(root, jobID(ih))
		if err != nil {
			t.Fatalf("read during replacement: %v", err)
		}
		reads++
		switch {
		case status.UpdatedAt.Equal(variantA.UpdatedAt) && len(status.Selection) == 1 && status.RecordedState.Seeding == SeedingStopped:
		case status.UpdatedAt.Equal(variantB.UpdatedAt) && len(status.Selection) == 2 && status.RecordedState.Seeding == SeedingActive:
		default:
			t.Fatalf("observed a mixed record: updated=%s selection=%v seeding=%s",
				status.UpdatedAt, status.Selection, status.RecordedState.Seeding)
		}
	}
}
