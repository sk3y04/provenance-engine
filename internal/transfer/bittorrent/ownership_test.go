package bittorrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSecondSessionOnSameRootIsBusy: ownership is exclusive per staging root,
// and it is released by Close so the retry succeeds.
func TestSecondSessionOnSameRootIsBusy(t *testing.T) {
	staging := t.TempDir()
	first, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	if _, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true}); !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("second session error = %v, want ErrStagingBusy", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close third: %v", err)
	}
}

// TestBusySessionLeavesStateUntouched: a refused session must not read, create
// or rewrite job state. The check runs against a real completed job.
func TestBusySessionLeavesStateUntouched(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(4000, 'a')})
	staging := t.TempDir()

	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	before := jobStateDigest(t, staging, s.infohash())
	manifestsBefore := manifestCount(t, staging)
	filesBefore := snapshotTree(t, staging)

	if _, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true}); !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("competing session error = %v, want ErrStagingBusy", err)
	}

	if after := jobStateDigest(t, staging, s.infohash()); after != before {
		t.Errorf("job state changed: %q -> %q", before, after)
	}
	if after := manifestCount(t, staging); after != manifestsBefore {
		t.Errorf("capture manifests changed: %d -> %d", manifestsBefore, after)
	}
	if diff := diffSnapshots(filesBefore, snapshotTree(t, staging)); diff != "" {
		t.Errorf("competing session touched staging:\n%s", diff)
	}
}

// TestSymlinkedSpellingIsSameOwner: the lock is a file inside the root, so any
// spelling that resolves to the same directory (symlink, trailing slash, "." )
// contends on the same inode. Lexical spellings are already normalised by
// filepath.Abs; symlinks are not, and are the case that matters.
func TestSymlinkedSpellingIsSameOwner(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	owner, err := New(Config{StagingDir: root, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("owner session: %v", err)
	}
	for _, spelling := range []string{alias, alias + string(filepath.Separator), filepath.Join(root, ".", "")} {
		if _, err := New(Config{StagingDir: spelling, DisablePeerDiscovery: true}); !errors.Is(err, ErrStagingBusy) {
			t.Errorf("spelling %q: error = %v, want ErrStagingBusy", spelling, err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	viaAlias, err := New(Config{StagingDir: alias, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("acquire through alias after release: %v", err)
	}
	_ = viaAlias.Close()
}

// TestIndependentRootsRunConcurrently: different staging roots are unrelated
// scopes and must not serialize each other.
func TestIndependentRootsRunConcurrently(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(9000, 'i')})
	rootA := t.TempDir()
	rootB := t.TempDir()

	sessA := newTestSession(t, rootA, s.peer())
	// rootB is opened while rootA is owned: it must succeed.
	sessB := newTestSession(t, rootB, s.peer())

	ctx, cancel := waitCtx(t)
	defer cancel()
	var wg sync.WaitGroup
	results := make([]Capture, 2)
	errs := make([]error, 2)
	for i, sess := range []*Session{sessA, sessB} {
		wg.Add(1)
		go func(i int, sess *Session) {
			defer wg.Done()
			results[i], errs[i] = sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"})
		}(i, sess)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("root %d fetch: %v", i, err)
		}
	}
	if len(results[0].Assets) != 1 || len(results[1].Assets) != 1 {
		t.Fatalf("assets = %+v / %+v", results[0].Assets, results[1].Assets)
	}
	if results[0].CaptureKey != results[1].CaptureKey {
		t.Errorf("same content in different roots produced different keys: %s vs %s",
			results[0].CaptureKey, results[1].CaptureKey)
	}
	for _, root := range []string{rootA, rootB} {
		if n := manifestCount(t, root); n != 1 {
			t.Errorf("%s: manifests = %d, want 1", root, n)
		}
	}
}

// TestConcurrentFetchOnOneSessionIsSerialized: overlapping calls on one Session
// queue on the run mutex instead of interleaving; the second call reconciles the
// capture the first one committed.
func TestConcurrentFetchOnOneSessionIsSerialized(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(12000, 'c')})
	staging := t.TempDir()
	sess := newTestSession(t, staging, s.peer())

	ctx, cancel := waitCtx(t)
	defer cancel()
	var wg sync.WaitGroup
	caps := make([]Capture, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			caps[i], errs[i] = sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if caps[0].CaptureKey != caps[1].CaptureKey {
		t.Errorf("keys differ: %s vs %s", caps[0].CaptureKey, caps[1].CaptureKey)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("manifests = %d, want 1 (concurrent fetches duplicated the capture)", n)
	}
	if !caps[0].Reconciled && !caps[1].Reconciled {
		t.Error("neither call reconciled; the second should have adopted the first capture")
	}
}

// TestClosedSessionRejectsWork: Close is a hard boundary, so a caller holding a
// closed Session gets a clear error instead of operating on released staging.
func TestClosedSessionRejectsWork(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(2000, 'x')})
	staging := t.TempDir()
	sess, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true, Peers: []string{s.peer()}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("Fetch after Close = %v, want a closed-session error", err)
	}
	if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("List after Close = %v, want a closed-session error", err)
	}
	// Double Close is safe and keeps the root available.
	if err := sess.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	next, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("reopen after double close: %v", err)
	}
	_ = next.Close()
}

// TestOwnershipHeldDuringLongOperation: ownership is not dropped after the
// payload is verified or the capture committed — the client may still write
// (.part files, .torrent.db) until it is closed.
func TestOwnershipHeldDuringLongOperation(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(6000, 'h')})
	staging := t.TempDir()

	cfg := testConfig(t, staging, s.peer())
	cfg.KeepSeeding = true
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"})
	}()

	waitFor(t, "capture while seeding", 45*time.Second, func() bool {
		return manifestCount(t, staging) == 1
	})
	if _, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true}); !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("session created while the owner seeds: %v, want ErrStagingBusy", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("fetch did not return after cancellation")
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	next, err := New(Config{StagingDir: staging, DisablePeerDiscovery: true})
	if err != nil {
		t.Fatalf("acquire after the seeding owner closed: %v", err)
	}
	_ = next.Close()
}
