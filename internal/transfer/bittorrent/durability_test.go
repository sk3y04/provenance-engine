package bittorrent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// offlineConfig is a session with no peers and no discovery: anything it
// accomplishes had to come from staged bytes and the persisted job.
func offlineConfig(staging string) Config {
	return Config{
		StagingDir:           staging,
		DisablePeerDiscovery: true,
		MetadataTimeout:      5 * time.Second,
	}
}

// TestCaptureRetryAfterPayloadVerifyDoesNotRefetch is the capture-stage
// durability boundary: verification completed, capture never started. The retry
// must land the capture from staging alone, with the swarm unreachable.
func TestCaptureRetryAfterPayloadVerifyDoesNotRefetch(t *testing.T) {
	content := patterned(40000, 'p')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": patterned(5000, 'a'),
		"b.bin": content,
	})
	staging := t.TempDir()
	selection := []string{"fixture/b.bin"}

	failAfterPayloadReady = func() error { return errors.New("injected: died after verification") }
	t.Cleanup(func() { failAfterPayloadReady = nil })

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), selection); err == nil {
		t.Fatal("fetch reported success despite the injected failure")
	} else if !strings.Contains(err.Error(), "injected") {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	job := jobRecordFor(t, staging, s.infohash())
	if job.State.Payload != PayloadReady {
		t.Fatalf("payload state = %q, want ready", job.State.Payload)
	}
	if job.State.Capture != CapturePending {
		t.Fatalf("capture state = %q, want pending", job.State.Capture)
	}
	if job.Payload == nil || len(job.Payload.Assets) != 1 {
		t.Fatalf("payload attestation = %+v, want one asset", job.Payload)
	}
	if n := len(captureManifests(t, staging)); n != 0 {
		t.Fatalf("capture manifests = %d, want 0 after a pre-capture failure", n)
	}

	// Take the swarm away entirely: the retry must not need it. The staging root
	// is owned by a Session, so the failed one is closed first.
	if errs := s.client.Close(); len(errs) > 0 {
		t.Logf("seeder close: %v", errs)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close failed session: %v", err)
	}
	sess2, err := New(offlineConfig(staging))
	if err != nil {
		t.Fatalf("offline session: %v", err)
	}
	defer func() { _ = sess2.Close() }()

	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, s.magnet(), selection)
	if err != nil {
		t.Fatalf("capture retry: %v", err)
	}
	if cap.BytesRead != 0 {
		t.Errorf("capture retry read %d bytes; it went back to the swarm", cap.BytesRead)
	}
	if cap.Reconciled {
		t.Error("a freshly written capture was reported as reconciled")
	}
	if cap.State.Payload != PayloadReady || cap.State.Capture != CaptureCommitted {
		t.Errorf("state = %+v, want ready/committed", cap.State)
	}
	if got := cap.Assets[0].SHA256; got != sha256Hex(content) {
		t.Errorf("asset sha256 = %s, want %s", got, sha256Hex(content))
	}
	if cap.CaptureKey == "" {
		t.Error("capture key is empty")
	}

	job = jobRecordFor(t, staging, s.infohash())
	if job.State.Capture != CaptureCommitted || job.Capture == nil {
		t.Fatalf("job did not record the capture: %+v", job.State)
	}
	if job.Capture.Key != cap.CaptureKey || job.Capture.Manifest != cap.ManifestPath {
		t.Errorf("recorded capture = %+v, want key %s manifest %s", job.Capture, cap.CaptureKey, cap.ManifestPath)
	}
	if n := len(manifestsWithKey(t, staging, cap.CaptureKey)); n != 1 {
		t.Errorf("manifests with the job key = %d, want 1", n)
	}
}

// TestCaptureCommitCrashReconcilesWithoutDuplicate is the second durability
// boundary: the run manifest was committed, the job record was not updated. A
// restart must recognise the existing capture by key and write no duplicate.
func TestCaptureCommitCrashReconcilesWithoutDuplicate(t *testing.T) {
	content := patterned(30000, 'c')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"c.bin": content})
	staging := t.TempDir()
	selection := []string{"fixture/c.bin"}

	failAfterCaptureCommit = func() error { return errors.New("injected: died after commit") }
	t.Cleanup(func() { failAfterCaptureCommit = nil })

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), selection); err == nil {
		t.Fatal("fetch reported success despite the injected failure")
	} else if !strings.Contains(err.Error(), "injected") {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	// The capture is committed; the job state is still behind.
	job := jobRecordFor(t, staging, s.infohash())
	if job.State.Capture != CapturePending {
		t.Fatalf("capture state = %q, want pending (the crash point)", job.State.Capture)
	}
	if job.Payload == nil {
		t.Fatal("payload attestation missing after a post-capture crash")
	}
	key := captureKey(job.JobID, job.Payload.Assets)
	if n := len(manifestsWithKey(t, staging, key)); n != 1 {
		t.Fatalf("manifests with key = %d, want 1 committed capture", n)
	}

	// Restart with no swarm and no verification input other than staging. The
	// crashed session still holds ownership until it is closed, exactly as an
	// exited process releases it.
	if errs := s.client.Close(); len(errs) > 0 {
		t.Logf("seeder close: %v", errs)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close crashed session: %v", err)
	}
	sess2, err := New(offlineConfig(staging))
	if err != nil {
		t.Fatalf("offline session: %v", err)
	}
	defer func() { _ = sess2.Close() }()

	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, s.magnet(), selection)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !cap.Reconciled {
		t.Error("reconciled = false, want the existing capture to be adopted")
	}
	if cap.CaptureKey != key {
		t.Errorf("key = %s, want the committed key %s", cap.CaptureKey, key)
	}
	if cap.BytesRead != 0 {
		t.Errorf("reconcile read %d bytes from peers", cap.BytesRead)
	}
	if n := len(manifestsWithKey(t, staging, key)); n != 1 {
		t.Fatalf("manifests with key = %d after reconcile, want 1 (no duplicate)", n)
	}
	if n := len(captureManifests(t, staging)); n != 1 {
		t.Fatalf("total capture manifests = %d, want 1", n)
	}

	job = jobRecordFor(t, staging, s.infohash())
	if job.State.Capture != CaptureCommitted || job.Capture == nil {
		t.Fatalf("job not reconciled: %+v", job.State)
	}
	if job.Capture.Key != key || job.Capture.Manifest != cap.ManifestPath {
		t.Errorf("reconciled capture = %+v", job.Capture)
	}
	if !job.Capture.Reconciled {
		t.Error("reconciled capture not marked as such")
	}
}

// TestSeedingDoesNotBlockCaptureCompletion: seeding is a separate state. The
// capture must be visible to downstream readers while the client is still up,
// and stopping the seeding must not change the capture.
func TestSeedingDoesNotBlockCaptureCompletion(t *testing.T) {
	content := patterned(20000, 's')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"s.bin": content})
	staging := t.TempDir()
	selection := []string{"fixture/s.bin"}

	cfg := testConfig(t, staging, s.peer())
	cfg.KeepSeeding = true
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
	go func() {
		cap, err := sess.Fetch(ctx, s.magnet(), selection)
		done <- result{cap, err}
	}()

	jobPath := filepath.Join(staging, filepath.FromSlash(torrentStateDir), jobID(s.infohash())+".job.json")
	waitFor(t, "capture committed while seeding", 45*time.Second, func() bool {
		job, err := readJob(jobPath)
		if err != nil {
			return false
		}
		if job.State.Capture != CaptureCommitted || job.State.Seeding != SeedingActive {
			return false
		}
		runs, err := manifest.ListRunManifests(staging)
		return err == nil && len(runs) == 1
	})

	select {
	case res := <-done:
		t.Fatalf("fetch returned before seeding was stopped: %+v", res)
	default:
	}

	// The capture is complete and readable while seeding continues.
	cm := captureManifests(t, staging)[0]
	if len(cm.Items) != 1 || cm.Items[0].Sha256 != sha256Hex(content) {
		t.Fatalf("capture manifest = %+v", cm)
	}

	cancel()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("fetch error: %v", res.err)
		}
		if res.cap.State.Capture != CaptureCommitted {
			t.Errorf("state = %+v, want the capture still committed", res.cap.State)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fetch did not return after cancellation")
	}

	job := jobRecordFor(t, staging, s.infohash())
	if job.State.Seeding != SeedingStopped {
		t.Errorf("seeding state = %q, want stopped after cancellation", job.State.Seeding)
	}
	if job.State.Capture != CaptureCommitted {
		t.Errorf("capture state = %q, want committed", job.State.Capture)
	}
}

// TestStaleStagedPayloadIsReportedAndRepaired: if the bytes behind a verified
// attestation change, the job drops back to unverified, says so, and repairs
// through the normal resume path instead of capturing the corruption.
func TestStaleStagedPayloadIsReportedAndRepaired(t *testing.T) {
	content := patterned(60000, 'q')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"q.bin": content})
	staging := t.TempDir()
	selection := []string{"fixture/q.bin"}

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), selection); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	target := filepath.Join(staging, "fixture", "q.bin")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	f, err := os.OpenFile(target, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 4096); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	_ = f.Close()

	// Release ownership before the repair run opens its own Session.
	if err := sess.Close(); err != nil {
		t.Fatalf("close first session: %v", err)
	}
	sess2 := newTestSession(t, staging, s.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, s.magnet(), selection)
	if err != nil {
		t.Fatalf("repair fetch: %v", err)
	}
	if len(cap.Warnings) == 0 {
		t.Error("stale payload was repaired without reporting it")
	} else if !strings.Contains(strings.Join(cap.Warnings, " "), "staged payload") {
		t.Errorf("warnings = %v, want the stale-payload reason", cap.Warnings)
	}
	if cap.BytesRead <= 0 {
		t.Error("repair read nothing from the peer")
	}
	repaired, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if !bytes.Equal(repaired, content) {
		t.Fatal("repaired file does not match the fixture")
	}
	if n := len(manifestsWithKey(t, staging, cap.CaptureKey)); n != 1 {
		t.Errorf("manifests with key = %d, want the single committed capture", n)
	}
}

// TestJobArtifactsDoNotLeakTrackerSecrets: a magnet may carry tracker URLs with
// private passkeys. Records and captures that a user may read or export must not
// contain them, even though the backend keeps the announce list for resume.
func TestJobArtifactsDoNotLeakTrackerSecrets(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(4000, 'k')})
	staging := t.TempDir()
	magnet := s.magnet() + "&tr=" + url.QueryEscape("http://tracker.example/announce?passkey=SECRET")

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, magnet, []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	job := jobRecordFor(t, staging, s.infohash())
	if want := "magnet:?xt=urn:btih:" + s.infohash(); job.Source != want {
		t.Errorf("recorded source = %q, want %q", job.Source, want)
	}

	// Only the user-facing artifacts: job record, capture manifests, items.
	for _, dir := range []string{
		filepath.Join(staging, filepath.FromSlash(torrentStateDir)),
		filepath.Join(staging, ".provenance", "runs"),
		filepath.Join(staging, ".provenance", "items"),
	} {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			for _, secret := range []string{"SECRET", "tracker.example", "passkey"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Errorf("%s leaks %q", p, secret)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
