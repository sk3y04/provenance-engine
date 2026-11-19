package slskd

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, transfers []Transfer) (*Recovery, *JobStore, string) {
	t.Helper()
	mount := t.TempDir()
	ts := &transferServer{transfers: transfers}
	srv := httptest.NewServer(ts.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	store := mustOpenStore(t, t.TempDir())
	return &Recovery{Client: c, Store: store, Mount: mount, Grace: time.Minute, Now: func() time.Time { return time.Now().UTC() }}, store, mount
}

func age(j *Job, d time.Duration) {
	j.CreatedAt = j.CreatedAt.Add(-d)
	j.UpdatedAt = j.UpdatedAt.Add(-d)
}

// TestRecoveryUpdatesJobsSlskdStillKnows proves a normal restart keeps a live job
// live and folds in the state slskd reports.
func TestRecoveryUpdatesJobsSlskdStillKnows(t *testing.T) {
	rec, store, _ := recoveryFixture(t, []Transfer{{
		ID: "t1", Username: "peer1", Filename: `music\a.flac`, State: "InProgress", Size: 10,
	}})
	job := Job{ID: "prov-aaaaaaaaaaaaaaaa", Peer: "peer1", Status: JobInProgress,
		Items: []Item{{RemoteFilename: `music\a.flac`, Size: 10}}}
	if _, err := store.Create(job); err != nil {
		t.Fatal(err)
	}
	report, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Active != 1 || len(report.Interrupted) != 0 {
		t.Fatalf("report = %+v", report)
	}
	stored, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != JobInProgress || stored.Items[0].TransferID != "t1" {
		t.Fatalf("job = %+v", stored)
	}
}

// TestRecoveryAdoptsFilesOnDisk covers the retention-eviction case: the transfer
// record is gone, but the bytes arrived while Provenance was down.
func TestRecoveryAdoptsFilesOnDisk(t *testing.T) {
	rec, store, mount := recoveryFixture(t, nil)
	job := Job{ID: "prov-bbbbbbbbbbbbbbbb", Peer: "peer1", Status: JobInProgress,
		DestinationSubdir: "provenance/prov-bbbbbbbbbbbbbbbb",
		Items:             []Item{{RemoteFilename: `music\a.flac`, Size: 10}}}
	if _, err := store.Create(job); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(mount, "provenance", job.ID)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "a.flac"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Recovered) != 1 {
		t.Fatalf("report = %+v", report)
	}
	stored, _ := store.Get(job.ID)
	if stored.Status != JobCompletedPendingImport {
		t.Fatalf("status = %s", stored.Status)
	}
}

// TestRecoveryMarksVanishedJobInterrupted covers the volatile-restart case: the
// job must reach a terminal, actionable state instead of hanging forever.
func TestRecoveryMarksVanishedJobInterrupted(t *testing.T) {
	rec, store, _ := recoveryFixture(t, nil)
	job := Job{ID: "prov-cccccccccccccccc", Peer: "peer1", Status: JobInProgress,
		DestinationSubdir: "provenance/prov-cccccccccccccccc",
		Items:             []Item{{RemoteFilename: `music\a.flac`, Size: 10}}}
	created, err := store.Create(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(created.ID, func(j *Job) error {
		age(j, 10*time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Interrupted) != 1 {
		t.Fatalf("report = %+v", report)
	}
	stored, _ := store.Get(job.ID)
	if stored.Status != JobInterrupted {
		t.Fatalf("status = %s", stored.Status)
	}
	if stored.FailureReason == "" {
		t.Fatal("no reason recorded for an interrupted job")
	}
}

// TestRecoveryLeavesYoungJobsAlone proves a just-enqueued batch, which slskd has
// not turned into transfers yet, is not declared interrupted.
func TestRecoveryLeavesYoungJobsAlone(t *testing.T) {
	rec, store, _ := recoveryFixture(t, nil)
	job := Job{ID: "prov-dddddddddddddddd", Peer: "peer1", Status: JobQueued,
		Items: []Item{{RemoteFilename: `music\a.flac`, Size: 10}}}
	if _, err := store.Create(job); err != nil {
		t.Fatal(err)
	}
	report, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.LeftAlone) != 1 {
		t.Fatalf("report = %+v", report)
	}
	stored, _ := store.Get(job.ID)
	if stored.Status != JobQueued {
		t.Fatalf("status = %s, want QUEUED", stored.Status)
	}
}

// TestRecoveryRefusesToGuessWhenSlskdIsDown is the important negative: a network
// failure must not rewrite history.
func TestRecoveryRefusesToGuessWhenSlskdIsDown(t *testing.T) {
	store := mustOpenStore(t, t.TempDir())
	job := Job{ID: "prov-eeeeeeeeeeeeeeee", Peer: "peer1", Status: JobInProgress,
		Items: []Item{{RemoteFilename: `music\a.flac`, Size: 10}}}
	created, err := store.Create(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(created.ID, func(j *Job) error { age(j, time.Hour); return nil }); err != nil {
		t.Fatal(err)
	}
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k", TransferTimeout: 200 * time.Millisecond})
	rec := &Recovery{Client: c, Store: store, Mount: t.TempDir()}
	if _, err := rec.Run(context.Background()); err == nil {
		t.Fatal("recovery reported success while slskd was unreachable")
	}
	stored, _ := store.Get(job.ID)
	if stored.Status != JobInProgress {
		t.Fatalf("status changed to %s on a network failure", stored.Status)
	}
}

// TestRecoveryReportSummary proves the operator-facing lines name the actions.
func TestRecoveryReportSummary(t *testing.T) {
	rep := RecoveryReport{Checked: 3, Active: 1, Recovered: []string{"prov-1"}, Interrupted: []string{"prov-2"}, LeftAlone: []string{"prov-3"}}
	lines := rep.Summary()
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"3 pending job(s)", "recovered from disk: prov-1", "interrupted", "too new"} {
		if !contains(joined, want) {
			t.Errorf("summary is missing %q:\n%s", want, joined)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
