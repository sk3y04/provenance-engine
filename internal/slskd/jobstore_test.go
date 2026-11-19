package slskd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestJob(t *testing.T, id string) Job {
	t.Helper()
	return Job{
		ID:         id,
		Peer:       "peer1",
		SearchID:   "s1",
		SearchText: "beethoven",
		Items:      []Item{{RemoteFilename: `music\Album\track.flac`, Size: 1234}},
	}
}

func mustOpenStore(t *testing.T, root string) *JobStore {
	t.Helper()
	s, err := OpenJobStore(root)
	if err != nil {
		t.Fatalf("OpenJobStore: %v", err)
	}
	return s
}

// TestJobStoreRoundTrip proves a created job reads back byte-identical and that
// the record is 0600: it names peers and remote paths, which is personal data.
func TestJobStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := mustOpenStore(t, root)
	id, err := NewJobID()
	if err != nil {
		t.Fatal(err)
	}
	job := newTestJob(t, id)
	created, err := s.Create(job)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.DestinationSubdir != "provenance/"+id {
		t.Fatalf("destination = %q, want the pinned provenance/<id>", created.DestinationSubdir)
	}
	if created.Status != JobQueued {
		t.Fatalf("status = %q", created.Status)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Peer != job.Peer || len(got.Items) != 1 || got.Items[0].Size != 1234 {
		t.Fatalf("round trip changed the job: %+v", got)
	}
	fi, err := os.Stat(filepath.Join(root, ".provenance", "slskd", "jobs", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("job record mode = %o, want 600", fi.Mode().Perm())
	}
}

// TestJobStoreRefusesDuplicateID proves a retry cannot overwrite history.
func TestJobStoreRefusesDuplicateID(t *testing.T) {
	s := mustOpenStore(t, t.TempDir())
	id, _ := NewJobID()
	if _, err := s.Create(newTestJob(t, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(newTestJob(t, id)); err == nil {
		t.Fatal("duplicate id accepted")
	}
}

// TestJobStoreRejectsUnsafeIDs keeps a record path inside the jobs directory.
func TestJobStoreRejectsUnsafeIDs(t *testing.T) {
	s := mustOpenStore(t, t.TempDir())
	for _, id := range []string{"", "prov-", "../../etc/passwd", "prov-XYZ", "other-0123456789abcdef"} {
		if _, err := s.Create(newTestJob(t, id)); err == nil {
			t.Fatalf("Create(%q) accepted", id)
		}
		if _, err := s.Get(id); err == nil {
			t.Fatalf("Get(%q) accepted", id)
		}
	}
}

// TestJobStoreUpdateAndPending proves the store is the durable lifecycle record:
// an update lands, and only non-terminal jobs are handed to the reconciler.
func TestJobStoreUpdateAndPending(t *testing.T) {
	s := mustOpenStore(t, t.TempDir())
	a, _ := NewJobID()
	b, _ := NewJobID()
	if _, err := s.Create(newTestJob(t, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(newTestJob(t, b)); err != nil {
		t.Fatal(err)
	}

	updated, err := s.Update(a, func(j *Job) error {
		j.Status = JobImported
		j.RevisionID = "rev1"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != JobImported || updated.RevisionID != "rev1" {
		t.Fatalf("update not applied: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) && !updated.UpdatedAt.Equal(updated.CreatedAt) {
		t.Fatalf("UpdatedAt %v is before CreatedAt %v", updated.UpdatedAt, updated.CreatedAt)
	}

	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != b {
		t.Fatalf("pending = %+v, want only %s", pending, b)
	}
}

// TestJobStoreDuplicateDetection is what stops a second grab of the same file
// from double-enqueueing it.
func TestJobStoreDuplicateDetection(t *testing.T) {
	s := mustOpenStore(t, t.TempDir())
	id, _ := NewJobID()
	if _, err := s.Create(newTestJob(t, id)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.FindActiveItem("PEER1", strings.ToLower(`MUSIC\album\TRACK.flac`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.ID != id {
		t.Fatalf("duplicate not found: ok=%v job=%+v", ok, got)
	}
	if _, ok, _ := s.FindActiveItem("peer2", `music\Album\track.flac`); ok {
		t.Fatal("matched a different peer")
	}

	// A finished job no longer blocks a re-grab.
	if _, err := s.Update(id, func(j *Job) error { j.Status = JobFailed; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.FindActiveItem("peer1", `music\Album\track.flac`); ok {
		t.Fatal("terminal job still blocks a re-grab")
	}
}

// TestJobStoreRefusesFutureFormat proves a newer record is reported rather than
// silently reinterpreted.
func TestJobStoreRefusesFutureFormat(t *testing.T) {
	root := t.TempDir()
	s := mustOpenStore(t, root)
	id, _ := NewJobID()
	if _, err := s.Create(newTestJob(t, id)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".provenance", "slskd", "jobs", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	future := strings.Replace(string(data), JobFormat, "provenance/slskd-job+99", 1)
	if err := os.WriteFile(path, []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); err == nil {
		t.Fatal("future format accepted")
	}
	if _, err := s.List(); err == nil {
		t.Fatal("List silently ignored an unreadable record")
	}
}

// TestJobStoreConcurrentWritesSurvive proves two writers serialise instead of
// clobbering each other's record.
func TestJobStoreConcurrentWritesSurvive(t *testing.T) {
	s := mustOpenStore(t, t.TempDir())
	id, _ := NewJobID()
	if _, err := s.Create(newTestJob(t, id)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(n int) {
			_, err := s.Update(id, func(j *Job) error {
				j.Items[0].State = "InProgress"
				j.FailureReason = strings.Repeat("x", n)
				return nil
			})
			done <- err
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("record unreadable after concurrent writes: %v", err)
	}
	if got.Items[0].State != "InProgress" {
		t.Fatalf("state = %q", got.Items[0].State)
	}
}

// TestJobPendingStatuses pins which statuses the reconciler must still see.
func TestJobPendingStatuses(t *testing.T) {
	pending := []JobStatus{JobQueued, JobInProgress, JobCompletedPendingImport, JobInterrupted}
	terminal := []JobStatus{JobImported, JobFailed, JobImportFailed, JobCancelled}
	for _, s := range pending {
		if s.Terminal() {
			t.Errorf("%s reported terminal", s)
		}
	}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s reported pending", s)
		}
	}
}

// TestJobActiveItem proves a job with an unobserved or moving item is not
// considered finished, which is what keeps the poller awake.
func TestJobActiveItem(t *testing.T) {
	cases := []struct {
		states []string
		want   bool
	}{
		{[]string{""}, true},
		{[]string{"Queued, Remotely"}, true},
		{[]string{"InProgress"}, true},
		{[]string{"Completed, Succeeded"}, false},
		{[]string{"Completed, Errored"}, false},
		{[]string{"Completed, Succeeded", "InProgress"}, true},
	}
	for _, tc := range cases {
		j := Job{}
		for _, st := range tc.states {
			j.Items = append(j.Items, Item{State: st})
		}
		if got := j.ActiveItem(); got != tc.want {
			t.Errorf("ActiveItem(%v) = %v, want %v", tc.states, got, tc.want)
		}
	}
}

// TestNewJobIDShape pins the id namespace used for externalId correlation.
func TestNewJobIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := NewJobID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidJobID(id) {
			t.Fatalf("NewJobID produced invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
