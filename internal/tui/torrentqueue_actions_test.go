package tui

// BT-10d: queue actions from the TUI.

import (
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
)

func actionModel(t *testing.T, entries ...torrentqueue.Entry) *model {
	t.Helper()
	m := &model{view: viewTorrentQueue, width: 200, height: 40}
	m.queue = queueState{root: t.TempDir(), entries: entries, liveStatus: "live status: unavailable"}
	return m
}

func storeAt(t *testing.T, root string) *torrentqueue.Store {
	t.Helper()
	s, err := torrentqueue.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func seedEntry(t *testing.T, store *torrentqueue.Store, phase torrentqueue.Phase, desired torrentqueue.Desired) torrentqueue.Entry {
	t.Helper()
	e, _, err := store.Add(torrentqueue.AddRequest{
		Source:    torrentqueue.Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)},
		RawSource: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20),
		Discovery: "direct",
		Selection: []string{"a.bin"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	got, uerr := store.Update(e.ID, func(x *torrentqueue.Entry) error {
		x.Phase = phase
		x.Desired = desired
		if phase == torrentqueue.PhaseFailed {
			x.LastError = "boom"
			x.Note = "note"
			x.NotBefore = time.Now().Add(time.Hour)
		}
		return nil
	})
	if uerr != nil {
		t.Fatalf("seed state: %v", uerr)
	}
	return got
}

func pressAndApply(t *testing.T, m *model, keyStr string) {
	t.Helper()
	_, cmd := m.Update(key(keyStr))
	if cmd == nil {
		t.Fatalf("key %q produced no command (status=%q)", keyStr, m.queue.status)
	}
	for _, msg := range drainCmd(t, cmd) {
		_, _ = m.Update(msg)
	}
}

// TestQueueActionStartFromTUI: the row's id is started, the status says "saved", and no
// runner is launched.
func TestQueueActionStartFromTUI(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhasePaused, torrentqueue.DesiredPaused)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root

	pressAndApply(t, m, "s")
	if !strings.Contains(m.queue.status, "saved start intent") {
		t.Fatalf("status = %q", m.queue.status)
	}
	if !strings.Contains(m.queue.status, "no runner is running") {
		t.Fatalf("status does not explain that nothing starts without a runner: %q", m.queue.status)
	}
	got := mustEntry(t, store, e.ID)
	if got.Desired != torrentqueue.DesiredRun || got.Phase != torrentqueue.PhaseQueued {
		t.Fatalf("durable state = desired %q phase %q", got.Desired, got.Phase)
	}
	if torrentqueue.RunnerHeld(root) {
		t.Fatal("a TUI action started a runner")
	}
	if m.queue.pendingSelect != e.ID {
		t.Fatalf("selection not preserved: %q", m.queue.pendingSelect)
	}
}

// TestQueueActionPauseDistinguishesRequestFromObservation: with a live serving attempt,
// the status says the runner has not acknowledged and the live slot is untouched.
func TestQueueActionPauseDistinguishesRequestFromObservation(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhaseRunning, torrentqueue.DesiredRun)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: e.ID, Kind: "download", Phase: "running"})
	snap.Seeds = torrentqueue.LiveSlots{Used: 0, Max: 1}
	m.queue.live = &snap

	pressAndApply(t, m, "p")
	if !strings.Contains(m.queue.status, "not acknowledged") {
		t.Fatalf("status = %q, want the request/observation distinction", m.queue.status)
	}
	got := mustEntry(t, store, e.ID)
	if got.Desired != torrentqueue.DesiredPaused {
		t.Fatalf("pause intent not saved: %+v", got)
	}
	if got.Phase != torrentqueue.PhaseRunning {
		t.Fatalf("pause rewrote the observed phase to %q", got.Phase)
	}
	if m.queue.live == nil || m.queue.live.Attempts[0].Phase != "running" {
		t.Fatal("the live observation was cleared to make the action look done")
	}
	if out := m.viewTorrentQueue(); !strings.Contains(out, "live=download running") {
		t.Fatalf("the screen no longer shows the live attempt:\n%s", out)
	}
}

// TestQueueActionRetryFromTUI: retry clears the failure state, not just intent.
func TestQueueActionRetryFromTUI(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhaseFailed, torrentqueue.DesiredRun)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root

	pressAndApply(t, m, "t")
	got := mustEntry(t, store, e.ID)
	if got.Phase != torrentqueue.PhaseQueued || got.Desired != torrentqueue.DesiredRun {
		t.Fatalf("retry = %+v", got)
	}
	if got.LastError != "" || got.Note != "" || !got.NotBefore.IsZero() {
		t.Fatalf("retry left failure state: %+v", got)
	}
	if !strings.Contains(m.queue.status, "errors cleared") {
		t.Fatalf("status = %q", m.queue.status)
	}
}

// TestQueueActionInvalidStateRefused: start on a failed entry is refused, visibly.
func TestQueueActionInvalidStateRefused(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhaseFailed, torrentqueue.DesiredRun)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root

	pressAndApply(t, m, "s")
	if !m.queue.statusErr || !strings.Contains(m.queue.status, "retry") {
		t.Fatalf("status = %q (err=%v), want an actionable refusal", m.queue.status, m.queue.statusErr)
	}
	if got := mustEntry(t, store, e.ID); got.Phase != torrentqueue.PhaseFailed {
		t.Fatalf("a refused action changed the entry: %+v", got)
	}
}

// TestQueueActionUsesSelectedIDNotRow: with two rows, the action hits the highlighted one.
func TestQueueActionUsesSelectedIDNotRow(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	first := seedEntry(t, store, torrentqueue.PhasePaused, torrentqueue.DesiredPaused)
	second, _, err := store.Add(torrentqueue.AddRequest{
		Source:    torrentqueue.Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20)},
		RawSource: "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20),
		Discovery: "direct",
		Selection: []string{"b.bin"},
	})
	if err != nil {
		t.Fatalf("add 2: %v", err)
	}
	if _, uerr := store.Update(second.ID, func(x *torrentqueue.Entry) error {
		x.Phase = torrentqueue.PhasePaused
		x.Desired = torrentqueue.DesiredPaused
		return nil
	}); uerr != nil {
		t.Fatalf("seed 2: %v", uerr)
	}
	m := actionModel(t, mustEntry(t, store, first.ID), mustEntry(t, store, second.ID))
	m.queue.root = root
	m.queue.cursor = 1

	pressAndApply(t, m, "s")
	if got := mustEntry(t, store, first.ID); got.Desired != torrentqueue.DesiredPaused {
		t.Fatalf("the unselected row was changed: %+v", got)
	}
	if got := mustEntry(t, store, second.ID); got.Desired != torrentqueue.DesiredRun {
		t.Fatalf("the selected row was not changed: %+v", got)
	}
}

// TestQueueActionSingleFlightAndPollDuringWrite: repeated keys cannot double-apply and a
// poll that lands during the write does not clear the pending state or the status.
func TestQueueActionSingleFlightAndPollDuringWrite(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhasePaused, torrentqueue.DesiredPaused)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root
	m.queue.gen = 3

	_, first := m.Update(key("s"))
	if first == nil || !m.queue.actionBusy {
		t.Fatalf("first action: cmd=%v busy=%v", first, m.queue.actionBusy)
	}
	_, second := m.Update(key("s"))
	if second != nil {
		t.Fatal("a second key press started a second write")
	}
	// A poll result arriving while the write is pending must not disturb it.
	snap := liveSnap()
	_, _ = m.Update(queueLiveMsg{gen: 3, root: root, snap: snap})
	if !m.queue.actionBusy {
		t.Fatal("a poll cleared the pending write state")
	}
	if !strings.Contains(m.queue.status, "requested") {
		t.Fatalf("a poll overwrote the pending status: %q", m.queue.status)
	}
}

// TestQueueActionLateResultIgnored: a result for another root, another generation or
// after navigation changes nothing.
func TestQueueActionLateResultIgnored(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhasePaused, torrentqueue.DesiredPaused)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root
	m.queue.gen = 4

	_, _ = m.Update(queueActionMsg{gen: 3, root: root, id: e.ID, action: "start", entry: torrentqueue.Entry{ID: e.ID}})
	if m.queue.status != "" {
		t.Fatalf("a stale generation applied a status: %q", m.queue.status)
	}
	_, _ = m.Update(queueActionMsg{gen: 4, root: "/other/root", id: e.ID, action: "start", entry: torrentqueue.Entry{ID: e.ID}})
	if m.queue.status != "" {
		t.Fatalf("a result from another root applied a status: %q", m.queue.status)
	}
	m.view = viewMain
	_, _ = m.Update(queueActionMsg{gen: 4, root: root, id: e.ID, action: "start", entry: torrentqueue.Entry{ID: e.ID}})
	if m.queue.status != "" {
		t.Fatalf("a late result after navigation applied a status: %q", m.queue.status)
	}
}

// TestQueueActionFailedWriteKeepsRow: a write failure is reported and nothing changes.
func TestQueueActionFailedWriteKeepsRow(t *testing.T) {
	root := t.TempDir()
	store := storeAt(t, root)
	e := seedEntry(t, store, torrentqueue.PhasePaused, torrentqueue.DesiredPaused)
	m := actionModel(t, mustEntry(t, store, e.ID))
	m.queue.root = root
	m.queue.gen = 2
	m.queue.actionBusy = true

	_, _ = m.Update(queueActionMsg{gen: 2, root: root, id: e.ID, action: "start", err: errStub})
	if !m.queue.statusErr || !strings.Contains(m.queue.status, "permission denied") {
		t.Fatalf("status = %q (err=%v)", m.queue.status, m.queue.statusErr)
	}
	if m.queue.actionBusy {
		t.Fatal("a failed write left the action pending forever")
	}
	if len(m.queue.entries) != 1 {
		t.Fatal("a failed write dropped the row")
	}
	if out := m.viewTorrentQueue(); !strings.Contains(out, "refused") {
		t.Fatalf("the refusal is not rendered:\n%s", out)
	}
}

func mustEntry(t *testing.T, store *torrentqueue.Store, id string) torrentqueue.Entry {
	t.Helper()
	e, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return e
}
