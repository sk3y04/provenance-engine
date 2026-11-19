package tui

import (
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
)

func queueTestModel(entries ...torrentqueue.Entry) *model {
	m := &model{view: viewTorrentQueue, width: 200, height: 40}
	m.queue = queueState{root: "/tmp/root", entries: entries, liveStatus: "live status: unavailable (no foreground runner is reachable)"}
	return m
}

func entry(id string, phase torrentqueue.Phase) torrentqueue.Entry {
	return torrentqueue.Entry{
		ID:        id,
		Desired:   torrentqueue.DesiredRun,
		Phase:     phase,
		Selection: []string{"a.bin"},
	}
}

func liveSnap(attempts ...torrentqueue.LiveAttempt) torrentqueue.LiveSnapshot {
	return torrentqueue.LiveSnapshot{
		Format:    torrentqueue.LiveFormat,
		Root:      "/tmp/root",
		PID:       4242,
		Downloads: torrentqueue.LiveSlots{Used: 1, Max: 1},
		Seeds:     torrentqueue.LiveSlots{Used: 1, Max: 2},
		Attempts:  attempts,
	}
}

// TestQueueViewDurableOnly: with no runner, the durable queue is fully browsable and
// nothing is presented as live.
func TestQueueViewDurableOnly(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning), entry("queued-000002-bbbbbbbb", torrentqueue.PhaseCompleted))
	out := m.viewTorrentQueue()
	for _, want := range []string{"queued-000001-aaaaaaaa", "queued-000002-bbbbbbbb", "recorded=running", "recorded=completed", "unavailable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("durable-only view missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"live=download", "live=seed", "download slots", "uploaded="} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("durable-only view shows %q as if it were live:\n%s", unwanted, out)
		}
	}
}

// TestQueueViewOverlapAndSlots: two attempts are shown on their own rows, with both
// pools' used/configured counts and the seed's uploaded bytes.
func TestQueueViewOverlapAndSlots(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning), entry("queued-000002-bbbbbbbb", torrentqueue.PhaseCompleted))
	up := int64(4096)
	snap := liveSnap(
		torrentqueue.LiveAttempt{EntryID: "queued-000001-aaaaaaaa", Kind: "download", Phase: "running"},
		torrentqueue.LiveAttempt{EntryID: "queued-000002-bbbbbbbb", Kind: "seed", Phase: "serving", UploadedBytes: &up},
	)
	m.queue.live = &snap
	m.queue.liveStatus = "live: runner pid 4242, started 12:00:00"
	out := m.viewTorrentQueue()
	for _, want := range []string{"live=download running", "live=seed serving", "uploaded=4.0 KB", "download slots 1/1", "seed slots 1/2", "pid 4242"} {
		if !strings.Contains(out, want) {
			t.Fatalf("overlap view missing %q:\n%s", want, out)
		}
	}
	// Recorded and live are separate columns, and the recorded phase is untouched.
	if !strings.Contains(out, "recorded=running") {
		t.Fatalf("recorded phase was replaced by a live one:\n%s", out)
	}
}

// TestQueueViewStoppingHoldsSlot: a stopping attempt keeps its slot and says so.
func TestQueueViewStoppingHoldsSlot(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseCompleted))
	snap := liveSnap(torrentqueue.LiveAttempt{
		EntryID: "queued-000001-aaaaaaaa", Kind: "seed", Phase: "stopping", Stopping: true,
	})
	snap.Seeds = torrentqueue.LiveSlots{Used: 1, Max: 1}
	m.queue.live = &snap
	out := m.viewTorrentQueue()
	if !strings.Contains(out, "stopping; slot held until close") {
		t.Fatalf("stopping attempt not labelled:\n%s", out)
	}
	if !strings.Contains(out, "seed slots 1/1") {
		t.Fatalf("slot occupancy not shown:\n%s", out)
	}
}

// TestQueueViewAbsentCounter: a missing counter renders as absent, never as zero.
func TestQueueViewAbsentCounter(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning))
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: "queued-000001-aaaaaaaa", Kind: "download", Phase: "running"})
	m.queue.live = &snap
	out := m.viewTorrentQueue()
	if strings.Contains(out, "uploaded=") || strings.Contains(out, "0 B") {
		t.Fatalf("an absent counter was rendered as a value:\n%s", out)
	}
}

// TestQueueViewLiveOnlyEntry: a snapshot naming an entry the durable list does not
// have stays visible, and is not attached to another row.
func TestQueueViewLiveOnlyEntry(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning))
	snap := liveSnap(
		torrentqueue.LiveAttempt{EntryID: "queued-000001-aaaaaaaa", Kind: "download", Phase: "running"},
		torrentqueue.LiveAttempt{EntryID: "queued-000009-orphan00", Kind: "seed", Phase: "serving"},
	)
	m.queue.live = &snap
	out := m.viewTorrentQueue()
	if !strings.Contains(out, "live only") || !strings.Contains(out, "queued-000009-orphan00") {
		t.Fatalf("live-only attempt hidden:\n%s", out)
	}
	if strings.Count(out, "live=seed") != 0 {
		t.Fatalf("a live-only attempt was attached to a durable row:\n%s", out)
	}
}

// TestQueueRunnerDisappears: after a successful poll, a failed one clears the live
// overlay and leaves the durable queue browsable.
func TestQueueRunnerDisappears(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseCompleted))
	m.queue.gen = 7
	m.queue.inflight = 2
	up := int64(1024)
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: "queued-000001-aaaaaaaa", Kind: "seed", Phase: "serving", UploadedBytes: &up})
	_, _ = m.updateQueue(queueLiveMsg{gen: 7, root: m.queue.root, snap: snap})
	if m.queue.live == nil {
		t.Fatal("a successful poll did not record the snapshot")
	}
	if out := m.viewTorrentQueue(); !strings.Contains(out, "live=seed serving") {
		t.Fatalf("live overlay not rendered after a successful poll:\n%s", out)
	}

	_, _ = m.updateQueue(queueLiveMsg{gen: 7, root: m.queue.root, err: torrentqueue.ErrNoLiveRunner})
	if m.queue.live != nil {
		t.Fatal("the last snapshot survived the runner's disappearance")
	}
	out := m.viewTorrentQueue()
	if strings.Contains(out, "live=seed") || strings.Contains(out, "uploaded=") {
		t.Fatalf("a dead runner's snapshot is still presented as live:\n%s", out)
	}
	if !strings.Contains(out, "queued-000001-aaaaaaaa") || !strings.Contains(out, "unavailable") {
		t.Fatalf("the durable queue was not preserved:\n%s", out)
	}
}

// TestQueueLateMessageIgnored: a message from a previous view generation cannot
// change the current view.
func TestQueueLateMessageIgnored(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning))
	m.queue.gen = 5
	stale := []torrentqueue.Entry{entry("queued-000099-stale000", torrentqueue.PhaseCompleted)}
	_, _ = m.updateQueue(queueDurableMsg{gen: 4, root: m.queue.root, entries: stale})
	if len(m.queue.entries) != 1 || m.queue.entries[0].ID != "queued-000001-aaaaaaaa" {
		t.Fatalf("a stale durable message replaced the current list: %+v", m.queue.entries)
	}
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: "queued-000099-stale000", Kind: "seed", Phase: "serving"})
	_, _ = m.updateQueue(queueLiveMsg{gen: 4, root: m.queue.root, snap: snap})
	if m.queue.live != nil {
		t.Fatal("a stale live message was applied")
	}
}

// TestQueueRefreshSingleFlight: a refresh cannot start while one is in flight.
func TestQueueRefreshSingleFlight(t *testing.T) {
	m := queueTestModel()
	cmd := m.refreshQueue()
	if cmd == nil {
		t.Fatal("the first refresh produced no command")
	}
	if m.queue.inflight != 2 || m.queue.gen != 1 {
		t.Fatalf("first refresh: inflight=%d gen=%d", m.queue.inflight, m.queue.gen)
	}
	if again := m.refreshQueue(); again != nil {
		t.Fatal("a second refresh started while one was in flight")
	}
	if m.queue.gen != 1 {
		t.Fatalf("the refused refresh bumped the generation to %d", m.queue.gen)
	}
	_, _ = m.updateQueue(queueDurableMsg{gen: 1, root: m.queue.root, entries: nil})
	_, _ = m.updateQueue(queueLiveMsg{gen: 1, root: m.queue.root, err: torrentqueue.ErrNoLiveRunner})
	if m.queue.inflight != 0 {
		t.Fatalf("inflight count did not drain: %d", m.queue.inflight)
	}
	if next := m.refreshQueue(); next == nil {
		t.Fatal("a refresh after completion was refused")
	}
}

// TestQueueDurableFailureKeepsLive: a durable read failure is reported without
// erasing the live observation or the previously read rows.
func TestQueueDurableFailureKeepsLive(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning))
	m.queue.gen = 3
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: "queued-000001-aaaaaaaa", Kind: "download", Phase: "running"})
	m.queue.live = &snap
	_, _ = m.updateQueue(queueDurableMsg{gen: 3, root: m.queue.root, err: errStub})
	out := m.viewTorrentQueue()
	if !strings.Contains(out, "durable read failed") {
		t.Fatalf("durable failure not reported:\n%s", out)
	}
	if !strings.Contains(out, "queued-000001-aaaaaaaa") {
		t.Fatalf("a durable failure erased the rows it could not refresh:\n%s", out)
	}
	if !strings.Contains(out, "live=download running") {
		t.Fatalf("a durable failure erased a live observation:\n%s", out)
	}
}

type stubErr struct{}

func (stubErr) Error() string { return "permission denied" }

var errStub = stubErr{}

// TestQueueReopenInvalidatesPreviousGenerations: reopening the view bumps the
// generation, so a message queued by the previous visit is dropped.
func TestQueueReopenInvalidatesPreviousGenerations(t *testing.T) {
	m := queueTestModel(entry("queued-000001-aaaaaaaa", torrentqueue.PhaseRunning))
	m.queue.gen = 9
	_, _ = m.openQueueView()
	if m.queue.gen != 10 {
		t.Fatalf("reopening did not advance the generation: %d", m.queue.gen)
	}
	stale := []torrentqueue.Entry{entry("queued-000099-stale000", torrentqueue.PhaseCompleted)}
	_, _ = m.updateQueue(queueDurableMsg{gen: 9, root: m.queue.root, entries: stale})
	if len(m.queue.entries) != 0 {
		t.Fatalf("a message from the previous visit was applied: %+v", m.queue.entries)
	}
}
