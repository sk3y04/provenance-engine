package tui

// BT-10f: the async boundaries, pinned through top-level Update.

import (
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
)

// TestQueueActionResultAcceptedAfterReadsAndPoll: a poll and both read results land
// before the action result. The result must still be accepted, and the post-write read
// must reflect the saved state.
func TestQueueActionResultAcceptedAfterReadsAndPoll(t *testing.T) {
	store, root, id := seedableStore(t)
	m := actionModel(t, mustEntry(t, store, id))
	m.queue.root = root
	m.queue.gen = 1

	// A poll refresh is in flight.
	refresh := m.refreshQueue()
	if refresh == nil {
		t.Fatal("no refresh command")
	}
	_, cmd := m.Update(key("e")) // the write starts
	if cmd == nil {
		t.Fatal("no action command")
	}
	// Both read results for the pre-write snapshot arrive first.
	_, _ = m.Update(queueDurableMsg{gen: 1, seq: 1, root: root, entries: m.queue.entries})
	_, _ = m.Update(queueLiveMsg{gen: 1, root: root, err: torrentqueue.ErrNoLiveRunner})
	// Then the action result.
	for _, msg := range drainCmd(t, cmd) {
		_, follow := m.Update(msg)
		// The post-write refresh must have been scheduled, not dropped.
		if follow == nil {
			t.Fatalf("no post-write refresh after the action result (status=%q)", m.queue.status)
		}
		for _, f := range drainCmd(t, follow) {
			_, _ = m.Update(f)
		}
	}
	if !strings.Contains(m.queue.status, "saved seed intent") {
		t.Fatalf("the action result was lost: %q", m.queue.status)
	}
	rows := m.queueRows()
	if len(rows) != 1 || rows[0].SeedDesired != torrentqueue.SeedOn {
		t.Fatalf("the post-write refresh did not show the saved state: %+v", rows)
	}
}

// TestQueueStaleReadCannotOverwriteNewer: a durable read begun before the write returns
// after a newer read and must be ignored.
func TestQueueStaleReadCannotOverwriteNewer(t *testing.T) {
	store, root, id := seedableStore(t)
	m := actionModel(t, mustEntry(t, store, id))
	m.queue.root = root
	m.queue.gen = 1

	m.refreshQueue() // seq 1
	// A newer read is applied first and shows the saved state.
	if _, uerr := store.EnableSeed(id); uerr != nil {
		t.Fatalf("enable: %v", uerr)
	}
	fresh := mustEntry(t, store, id)
	_, _ = m.Update(queueDurableMsg{gen: 1, seq: 2, root: root, entries: []torrentqueue.Entry{fresh}})
	if m.queueRows()[0].SeedDesired != torrentqueue.SeedOn {
		t.Fatal("the newer read was not applied")
	}
	// The older read (pre-write contents) arrives late.
	stale := fresh
	stale.SeedDesired = ""
	stale.SeedPhase = ""
	_, _ = m.Update(queueDurableMsg{gen: 1, seq: 1, root: root, entries: []torrentqueue.Entry{stale}})
	if got := m.queueRows()[0]; got.SeedDesired == "" {
		t.Fatalf("a stale read overwrote the newer one: %+v", got)
	}
}

// TestQueuePostWriteRefreshNotLostDuringPoll: when a write finishes while a poll is in
// flight, the post-write refresh is deferred and then run.
func TestQueuePostWriteRefreshNotLostDuringPoll(t *testing.T) {
	store, root, id := seedableStore(t)
	m := actionModel(t, mustEntry(t, store, id))
	m.queue.root = root
	m.queue.gen = 1

	m.refreshQueue() // poll in flight, inflight = 2
	_, cmd := m.Update(key("e"))
	var deferred bool
	for _, msg := range drainCmd(t, cmd) {
		_, follow := m.Update(msg)
		if follow != nil {
			for _, f := range drainCmd(t, follow) {
				_, _ = m.Update(f)
			}
		}
		// The action landed while the poll was still in flight, so the refresh is owed.
		if m.queue.pendingRefresh {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("the post-write refresh was not deferred behind the in-flight poll")
	}
	// Draining the poll now must hand back the owed refresh.
	_, follow := m.Update(queueLiveMsg{gen: 1, root: root, err: torrentqueue.ErrNoLiveRunner})
	_, follow2 := m.Update(queueDurableMsg{gen: 1, seq: 1, root: root, entries: m.queue.entries})
	cmd2 := follow
	if cmd2 == nil {
		cmd2 = follow2
	}
	if cmd2 == nil {
		t.Fatal("the owed refresh was lost once the poll drained")
	}
	for _, f := range drainCmd(t, cmd2) {
		_, _ = m.Update(f)
	}
	if m.queue.pendingRefresh {
		t.Fatal("the owed refresh was not consumed")
	}
	if got := m.queueRows()[0]; got.SeedDesired != torrentqueue.SeedOn {
		t.Fatalf("the deferred refresh did not read the saved state: %+v", got)
	}
}

// TestQueueReentryReadsCommittedWriteFromDisk: leaving with work pending, then coming
// back, shows the committed state from disk and ignores the old view's messages.
func TestQueueReentryReadsCommittedWriteFromDisk(t *testing.T) {
	store, root, id := seedableStore(t)
	m := actionModel(t, mustEntry(t, store, id))
	m.queue.root = root
	m.queue.gen = 3

	_, cmd := m.Update(key("e"))
	if cmd == nil {
		t.Fatal("no action command")
	}
	// The program runs the command immediately: the write commits now, but its message
	// is delivered later (after navigation).
	pending := drainCmd(t, cmd)
	// Leave the view before the result arrives, then come back.
	m.view = viewMain
	_, _ = m.Update(key("esc"))
	_, reopen := m.openQueueView()
	for _, msg := range drainCmd(t, reopen) {
		_, _ = m.Update(msg)
	}
	// The old view's action result must not touch the new view.
	for _, msg := range pending {
		_, _ = m.Update(msg)
	}
	if strings.Contains(m.queue.status, "saved seed intent") {
		t.Fatal("an obsolete action result touched the new view")
	}
	// The write is still on disk and the new view reads it.
	rows := m.queueRows()
	if len(rows) != 1 || rows[0].SeedDesired != torrentqueue.SeedOn {
		t.Fatalf("re-entry did not read the committed write from disk: %+v", rows)
	}
}
