package torrentqueue

// Real process death (BT-08, acceptance 5): a runner is killed with SIGKILL while
// it holds an active attempt, then a fresh runner is started against the same root.
// This is not the injected-error test from BT-07: the process really dies, so the
// only recovery mechanism is what is on disk.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQueueRunnerHelper is the child process. It is skipped unless the parent asks
// for it, so it costs nothing in a normal run.
func TestQueueRunnerHelper(t *testing.T) {
	root := os.Getenv("QUEUE_HELPER_ROOT")
	if root == "" {
		t.Skip("queue runner helper: only runs as a child process")
	}
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("helper store: %v", err)
	}
	opts := Options{MaxActive: 1, PollInterval: 20 * time.Millisecond}
	if mode := os.Getenv("QUEUE_HELPER_MODE"); mode == "hold-after-publish" {
		marker := os.Getenv("QUEUE_HELPER_MARKER")
		opts.BeforeResultPersist = func(string) {
			_ = os.WriteFile(marker, []byte("published"), 0o644)
			// The capture is committed, the queue record is not. The parent kills us
			// here; blocking on a sleep keeps the process alive without spinning.
			time.Sleep(time.Hour)
		}
	}
	if _, err := NewRunner(store, realAdapter(), opts).Run(context.Background()); err != nil {
		t.Fatalf("helper run: %v", err)
	}
}

// startHelper launches the runner in a child process.
func startHelper(t *testing.T, root, mode, marker string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestQueueRunnerHelper$", "-test.timeout=0", "-test.v")
	cmd.Env = append(os.Environ(),
		"QUEUE_HELPER_ROOT="+root,
		"QUEUE_HELPER_MODE="+mode,
		"QUEUE_HELPER_MARKER="+marker,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// killHelper kills the child and waits for the OS to reap it, so lock release is
// observable afterwards.
func killHelper(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited on its own; the kill proved nothing")
	}
}

func waitEntry(t *testing.T, store *Store, id string, cond func(Entry) bool, what string, timeout time.Duration) Entry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		entry, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if cond(entry) {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: entry stayed %q/%q (note %q)", what, entry.Desired, entry.Phase, entry.Note)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQueueRunnerKilledMidTransfer kills a real runner while it is transferring and
// proves the restart reacquires ownership, keeps a paused entry paused, and finishes
// the interrupted work with exactly one capture.
func TestQueueRunnerKilledMidTransfer(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(1<<20, 31)}
	seed := newLoopbackSeeder(t, "fixture", files, 64<<10)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	doomed := enqueue(t, store, seed, "fixture/a.bin")
	// A second, unrelated fixture: a paused entry for the *same* torrent would
	// legitimately dedupe onto the running one, which is not what this tests.
	other := newLoopbackSeeder(t, "fixture", map[string][]byte{"b.bin": patternedBytes(20<<10, 41)}, 0)
	paused := enqueue(t, store, other, "fixture/b.bin")
	startEntry(t, store, doomed.ID)

	cmd := startHelper(t, root, "drain", "")
	// The child must own the queue and be transferring, not merely have started.
	waitEntry(t, store, doomed.ID, func(e Entry) bool { return e.Phase == PhaseRunning }, "child never started the job", 60*time.Second)
	if !RunnerHeld(root) {
		t.Fatal("child does not hold the runner lock, so the kill would prove nothing")
	}
	deadline := time.Now().Add(60 * time.Second)
	for !findPart(t, filepath.Join(root, ".provenance", "torrent", "jobs", doomed.JobID)) {
		if time.Now().After(deadline) {
			t.Fatal("child never wrote payload; killing now would not be a mid-transfer death")
		}
		time.Sleep(10 * time.Millisecond)
	}
	killHelper(t, cmd)

	// Ownership is released by the OS, and the record is whatever the child last
	// wrote - a recorded "running", not a claim about a live process.
	deadline = time.Now().Add(30 * time.Second)
	for RunnerHeld(root) {
		if time.Now().After(deadline) {
			t.Fatal("runner lock survived process death")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := store.Get(doomed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Phase != PhaseRunning {
		t.Fatalf("phase after the kill = %q, want the recorded running observation", after.Phase)
	}
	if after.Capture != nil {
		t.Fatalf("a killed mid-transfer runner recorded a capture: %+v", after.Capture)
	}

	// Restart in-process against the same root: it reacquires ownership, reconciles
	// the interrupted observation, finishes the job, and leaves the paused entry
	// alone.
	sum, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	final, err := store.Get(doomed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseCompleted || final.Capture == nil {
		t.Fatalf("after restart = %q capture %+v (%s)", final.Phase, final.Capture, final.LastError)
	}
	if final.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the killed one and the restart)", final.Attempts)
	}
	ws := filepath.Join(root, filepath.FromSlash(final.Workspace))
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests after recovery, want 1", n)
	}
	if payload, rerr := os.ReadFile(filepath.Join(ws, "fixture", "a.bin")); rerr != nil {
		t.Fatalf("payload: %v", rerr)
	} else if string(payload) != string(files["a.bin"]) {
		t.Error("recovered payload does not match the fixture")
	}
	if sum.Completed != 1 {
		t.Errorf("restart summary = %+v, want 1 completed", sum)
	}
	if len(sum.Ran) != 1 || sum.Ran[0] != doomed.ID {
		t.Errorf("restart claimed %v, want only %s", sum.Ran, doomed.ID)
	}

	kept, err := store.Get(paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Desired != DesiredPaused || kept.Phase != PhaseQueued {
		t.Errorf("paused entry = %q/%q, want it untouched across the kill and restart", kept.Desired, kept.Phase)
	}
	if kept.Attempts != 0 {
		t.Errorf("paused entry attempts = %d, want 0", kept.Attempts)
	}
}

// TestQueueRunnerKilledAfterPublish is the publish-before-queue-update boundary,
// reported separately from the injected-error test: the child really publishes and
// really dies, and the restart must adopt rather than refetch.
func TestQueueRunnerKilledAfterPublish(t *testing.T) {
	files := map[string][]byte{"a.bin": patternedBytes(120<<10, 37)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, entry.ID)

	marker := filepath.Join(t.TempDir(), "published.marker")
	cmd := startHelper(t, root, "hold-after-publish", marker)
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, serr := os.Stat(marker); serr == nil {
			break
		}
		if time.Now().After(deadline) {
			killHelper(t, cmd)
			got, _ := store.Get(entry.ID)
			t.Fatalf("child never reached the publish boundary: %q/%q %s", got.Desired, got.Phase, got.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}
	killHelper(t, cmd)

	// The capture is on disk; the queue record never learned about it.
	before, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Phase != PhaseRunning || before.Capture != nil {
		t.Fatalf("record before restart = %q capture %+v, want the stale running observation", before.Phase, before.Capture)
	}
	ws := filepath.Join(root, filepath.FromSlash(".provenance/torrent/jobs/"+entry.JobID))
	if n := runManifestCount(t, ws); n != 1 {
		t.Fatalf("%d run manifests published before the kill, want 1", n)
	}

	sum, err := NewRunner(store, realAdapter(), Options{PollInterval: 10 * time.Millisecond}).Run(context.Background())
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	after, err := store.Get(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Phase != PhaseCompleted || after.Capture == nil {
		t.Fatalf("after restart = %q capture %+v", after.Phase, after.Capture)
	}
	if len(sum.Ran) != 0 {
		t.Errorf("restart claimed %v, want nothing: the capture was already published", sum.Ran)
	}
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests after adoption, want 1 (no duplicate publication)", n)
	}
	if !strings.Contains(after.Note, "adopted") {
		t.Errorf("note = %q, want an adoption explanation", after.Note)
	}
}
