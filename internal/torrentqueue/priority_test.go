package torrentqueue

// BT-09b: download priorities inside an immutable selection.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestQueuePriorityEditsOverrideNotSelection(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{
		"a.bin": patternedBytes(64<<10, 61),
		"b.bin": patternedBytes(64<<10, 67),
		"c.bin": patternedBytes(64<<10, 71),
	}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin", "alpha/b.bin")
	before := mustGet(t, store, entry.ID)

	high, perr := store.SetPriority(entry.ID, "alpha/a.bin", "high")
	if perr != nil {
		t.Fatalf("set high: %v", perr)
	}
	if high.Priorities["alpha/a.bin"] != "high" || len(high.Priorities) != 1 {
		t.Fatalf("override = %+v, want only alpha/a.bin high", high.Priorities)
	}
	// Nothing else moves: not the selection, not the intent, not the order.
	if len(high.Selection) != 2 || high.Desired != before.Desired || high.Phase != before.Phase || high.Seq != before.Seq {
		t.Fatalf("a priority edit changed the entry: %+v", high)
	}
	if high.JobID != before.JobID || high.Workspace != before.Workspace {
		t.Fatal("a priority edit changed the workspace identity")
	}

	normal, perr := store.SetPriority(entry.ID, "./alpha/a.bin", "normal")
	if perr != nil {
		t.Fatalf("clear: %v", perr)
	}
	if len(normal.Priorities) != 0 {
		t.Fatalf("clearing left an override: %+v", normal.Priorities)
	}

	// Refusals: unselected path, invalid level, bad path, unresolved entry.
	if _, uerr := store.SetPriority(entry.ID, "alpha/c.bin", "high"); !errors.Is(uerr, ErrSelectionNotSelected) {
		t.Fatalf("unselected path = %v, want ErrSelectionNotSelected", uerr)
	}
	if _, uerr := store.SetPriority(entry.ID, "alpha/a.bin", "urgent"); !errors.Is(uerr, ErrUsage) {
		t.Fatalf("invalid level = %v, want ErrUsage", uerr)
	}
	if _, uerr := store.SetPriority(entry.ID, "../../etc/passwd", "high"); !errors.Is(uerr, ErrUsage) {
		t.Fatalf("traversal path = %v, want ErrUsage", uerr)
	}
	unresolved := addWithSecret(t, store, fakeMagnet("ff"), "SECRET-P", "alpha/a.bin")
	if _, uerr := store.SetPriority(unresolved.ID, "alpha/a.bin", "high"); !errors.Is(uerr, ErrUsage) {
		t.Fatalf("unresolved entry = %v, want ErrUsage (selection not known yet)", uerr)
	}

	// A completed entry cannot be reopened by editing a priority.
	done := completedSeededEntry(t, store, seed, "alpha/a.bin")
	if _, uerr := store.SetPriority(done.ID, "alpha/a.bin", "high"); !errors.Is(uerr, ErrUsage) {
		t.Fatalf("completed entry = %v, want ErrUsage", uerr)
	}
}

func TestQueuePriorityRefusedWhileRunnerOwnsQueue(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(32<<10, 73)}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin")
	before := mustGet(t, store, entry.ID)

	owner, lerr := store.LockRunner()
	if lerr != nil {
		t.Fatalf("take ownership: %v", lerr)
	}
	if _, perr := store.SetPriority(entry.ID, "alpha/a.bin", "high"); !errors.Is(perr, ErrRunnerActive) {
		t.Fatalf("edit under a runner = %v, want ErrRunnerActive", perr)
	}
	after := mustGet(t, store, entry.ID)
	if len(after.Priorities) != 0 || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("a refused edit changed the record: %+v", after)
	}
	if _, rerr := NewRunner(store, realAdapter(), Options{}).Run(context.Background()); !errors.Is(rerr, ErrRunnerActive) {
		t.Fatalf("a runner started while the edit held ownership: %v", rerr)
	}
	_ = owner.Release()
	if _, perr := store.SetPriority(entry.ID, "alpha/a.bin", "high"); perr != nil {
		t.Fatalf("edit after release: %v", perr)
	}
}

// priorityRecorder records the priorities each attempt was handed.
type priorityRecorder struct {
	Adapter
	got chan map[string]string
}

func (p *priorityRecorder) Run(ctx context.Context, req Request) (Outcome, error) {
	select {
	case p.got <- req.Priorities:
	default:
	}
	return p.Adapter.Run(ctx, req)
}

// TestQueuePriorityReachesRequest: the persisted override is carried into the real
// request, and the fetch still captures exactly the selected files.
func TestQueuePriorityReachesRequest(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{
		"a.bin": patternedBytes(96<<10, 79),
		"b.bin": patternedBytes(64<<10, 83),
		"c.bin": patternedBytes(32<<10, 89),
	}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin", "alpha/b.bin")
	if _, perr := store.SetPriority(entry.ID, "alpha/a.bin", "high"); perr != nil {
		t.Fatalf("set high: %v", perr)
	}

	rec := &priorityRecorder{Adapter: realAdapter(), got: make(chan map[string]string, 1)}
	startEntry(t, store, entry.ID)
	sum, rerr := NewRunner(store, rec, Options{Log: os.Stderr}).Run(context.Background())
	if rerr != nil || sum.Completed != 1 {
		t.Fatalf("drain: %+v %v", sum, rerr)
	}
	select {
	case got := <-rec.got:
		if got["alpha/a.bin"] != "high" || len(got) != 1 {
			t.Fatalf("request priorities = %+v, want only alpha/a.bin high", got)
		}
	default:
		t.Fatal("no request carried priorities")
	}
	done := mustGet(t, store, entry.ID)
	ws := workspacePath(t, store, done)
	for _, want := range []string{"a.bin", "b.bin"} {
		if _, serr := os.Stat(ws + "/alpha/" + want); serr != nil {
			t.Fatalf("selected %s missing: %v", want, serr)
		}
	}
	if _, serr := os.Stat(ws + "/alpha/c.bin"); !os.IsNotExist(serr) {
		t.Fatal("an unselected file was fetched")
	}
	// The override is not part of the capture identity.
	if done.Capture == nil {
		t.Fatal("no capture")
	}
	// A priority edit is refused once completed, so a capture can never be rewritten.
	if _, perr := store.SetPriority(done.ID, "alpha/a.bin", "normal"); !errors.Is(perr, ErrUsage) {
		t.Fatalf("editing a completed entry = %v, want ErrUsage", perr)
	}
}

// TestQueuePrioritySurvivesInterrupt: the override persists across an interrupted
// fetch, is reapplied on resume, and neither the workspace identity nor the selection
// changes; the resumed run publishes exactly one capture.
func TestQueuePrioritySurvivesInterrupt(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// A slow seeder so the interrupt lands mid-transfer.
	seedFiles := map[string][]byte{
		"a.bin": patternedBytes(1<<20, 97),
		"b.bin": patternedBytes(1<<20, 101),
	}
	seed := newLoopbackSeeder(t, "alpha", seedFiles, 192<<10)
	entry := enqueue(t, store, seed, "alpha/a.bin", "alpha/b.bin")
	if _, perr := store.SetPriority(entry.ID, "alpha/b.bin", "high"); perr != nil {
		t.Fatalf("set high: %v", perr)
	}
	before := mustGet(t, store, entry.ID)
	startEntry(t, store, entry.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	_, _ = NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(ctx)
	cancel()
	mid := mustGet(t, store, entry.ID)
	if mid.JobID != before.JobID || mid.Workspace != before.Workspace {
		t.Fatalf("interrupt changed identity: %q -> %q", before.JobID, mid.JobID)
	}
	if len(mid.Selection) != 2 {
		t.Fatalf("interrupt changed the selection: %+v", mid.Selection)
	}
	if mid.Priorities["alpha/b.bin"] != "high" {
		t.Fatalf("override lost across the interrupt: %+v", mid.Priorities)
	}

	// An explicit start after an interrupt, the documented way to continue.
	if _, uerr := store.Update(entry.ID, func(e *Entry) error {
		if e.Phase != PhaseCompleted {
			e.Desired = DesiredRun
			e.Phase = PhaseQueued
			e.Attempt = ""
			e.LastError = ""
			e.NotBefore = time.Time{}
		}
		return nil
	}); uerr != nil {
		t.Fatalf("restart entry: %v", uerr)
	}
	// Resume with a fresh runner and let it finish.
	sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
	if rerr != nil || sum.Completed != 1 {
		t.Fatalf("resume: %+v %v", sum, rerr)
	}
	done := mustGet(t, store, entry.ID)
	if done.JobID != before.JobID || done.Workspace != before.Workspace || len(done.Selection) != 2 {
		t.Fatalf("resume changed identity or selection: %+v", done)
	}
	if done.Priorities["alpha/b.bin"] != "high" {
		t.Fatalf("override lost on resume: %+v", done.Priorities)
	}
	ws := workspacePath(t, store, done)
	for _, rel := range []string{"a.bin", "b.bin"} {
		got, rerr := os.ReadFile(ws + "/alpha/" + rel)
		if rerr != nil {
			t.Fatalf("resumed %s: %v", rel, rerr)
		}
		want := seedFiles[rel]
		if len(got) != len(want) {
			t.Fatalf("%s length %d, want %d", rel, len(got), len(want))
		}
	}
	if runs := runManifestCount(t, ws); runs != 1 {
		t.Fatalf("resume published %d run manifests, want 1", runs)
	}
}

// TestQueuePriorityReAddAdoptsWithoutRefetch: the same selection with a different
// priority is the same capture intent, so a re-add adopts the existing capture.
func TestQueuePriorityReAddAdoptsWithoutRefetch(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{
		"a.bin": patternedBytes(256<<10, 103),
		"b.bin": patternedBytes(64<<10, 107),
	}, 0)
	first := enqueue(t, store, seed, "alpha/a.bin", "alpha/b.bin")
	startEntry(t, store, first.ID)
	if sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background()); rerr != nil || sum.Completed != 1 {
		t.Fatalf("prepare completed entry: %+v %v", sum, rerr)
	}
	first = mustGet(t, store, first.ID)
	ws := workspacePath(t, store, first)
	payload := fileDigest(t, ws+"/alpha/a.bin")
	runs := runManifestCount(t, ws)

	// The same selection with a different priority is the same capture intent: the
	// original entry is forgotten first, exactly as a user would.
	if _, rerr := store.Remove(first.ID); rerr != nil {
		t.Fatalf("remove: %v", rerr)
	}
	again, dup, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: seed.magnet()},
		RawSource: seed.magnet(),
		Peers:     []string{seed.addr},
		Discovery: "direct",
		Selection: []string{"alpha/a.bin", "alpha/b.bin"},
		JobID:     first.JobID,
		InfoHash:  first.InfoHash,
		Workspace: first.Workspace,
	})
	if aerr != nil || dup {
		t.Fatalf("re-add: duplicate=%v err=%v", dup, aerr)
	}
	if _, perr := store.SetPriority(again.ID, "alpha/b.bin", "high"); perr != nil {
		t.Fatalf("priority on the re-added entry: %v", perr)
	}
	startEntry(t, store, again.ID)
	sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
	if rerr != nil || sum.Completed != 1 {
		t.Fatalf("re-added drain: %+v %v", sum, rerr)
	}
	done := mustGet(t, store, again.ID)
	if done.Capture == nil || done.Capture.Key != first.Capture.Key {
		t.Fatalf("re-add published a different capture: %+v", done.Capture)
	}
	if fileDigest(t, ws+"/alpha/a.bin") != payload {
		t.Fatal("re-add refetched the payload")
	}
	if now := runManifestCount(t, ws); now != runs {
		t.Fatalf("re-add published another run: %d, want %d", now, runs)
	}
}
