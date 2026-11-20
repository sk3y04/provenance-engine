package bittorrent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestSeedingOutlivesTransferTimeout: Config.Timeout bounds the transfer, not
// the KeepSeeding wait. A run that commits its capture and keeps seeding must
// still own the staging root after the transfer deadline expires; seeding ends
// only when the caller's context is cancelled.
//
// Regression for the v0.10.0 release gate: the seeding wait used the
// timeout-derived context, so on a slow runner the deadline fired mid-seeding,
// the owner recorded "stopped" and released the root, and the status acceptance
// test's competitor acquired a root that was supposed to be busy.
func TestSeedingOutlivesTransferTimeout(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(6000, 'x')})
	staging := t.TempDir()

	cfg := testConfig(t, staging, s.peer())
	cfg.KeepSeeding = true
	// Generous enough for the loopback transfer on a loaded runner, short
	// enough that the test can wait it out while seeding is expected to live.
	cfg.Timeout = 20 * time.Second
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		cap Capture
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		cap, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"})
		done <- result{cap, err}
	}()

	jobPath := filepath.Join(staging, filepath.FromSlash(torrentStateDir), jobID(s.infohash())+".job.json")
	waitFor(t, "capture committed while seeding", 45*time.Second, func() bool {
		job, err := readJob(jobPath)
		if err != nil {
			return false
		}
		return job.State.Capture == CaptureCommitted && job.State.Seeding == SeedingActive
	})

	// Let the transfer deadline expire while the caller still wants seeding.
	if remaining := time.Until(start.Add(cfg.Timeout + time.Second)); remaining > 0 {
		time.Sleep(remaining)
	}

	select {
	case res := <-done:
		t.Fatalf("fetch returned when the transfer deadline expired (cap=%+v err=%v); seeding must end only on caller cancellation", res.cap, res.err)
	default:
	}
	if _, err := New(Config{StagingDir: staging, Discovery: DiscoveryDirect}); !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("session created after the transfer deadline expired: %v, want ErrStagingBusy (the seeding owner must still hold the root)", err)
	}
	job, err := readJob(jobPath)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State.Seeding != SeedingActive {
		t.Fatalf("recorded seeding = %q after the transfer deadline expired, want %q", job.State.Seeding, SeedingActive)
	}

	// Cancellation is the only legitimate end of the seeding wait.
	cancel()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("fetch error after cancellation: %v", res.err)
		}
		if res.cap.State.Capture != CaptureCommitted {
			t.Errorf("capture state = %+v, want still committed", res.cap.State)
		}
		if res.cap.State.Seeding != SeedingStopped {
			t.Errorf("returned seeding state = %q, want %q", res.cap.State.Seeding, SeedingStopped)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fetch did not return after cancellation")
	}
	job, err = readJob(jobPath)
	if err != nil {
		t.Fatalf("read job after stop: %v", err)
	}
	if job.State.Seeding != SeedingStopped {
		t.Errorf("recorded seeding after cancellation = %q, want %q", job.State.Seeding, SeedingStopped)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	next, err := New(Config{StagingDir: staging, Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("acquire after the seeding owner closed: %v", err)
	}
	_ = next.Close()
}
