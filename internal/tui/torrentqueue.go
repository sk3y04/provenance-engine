package tui

// BT-10b: a read-only torrent queue view.
//
// Two reads, never one snapshot: the durable queue (entry records) and, when a
// foreground runner answers, one live snapshot. They are rendered in separate,
// labelled places and joined only by exact entry id. A live observation never
// rewrites a recorded phase, an entry without a live attempt simply has no live
// column, and a live attempt without a durable row stays visible as a live-only
// observation instead of being attached to a neighbour.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

const livePollInterval = 2 * time.Second

// errIsNoRunner reports whether the live read failed because nothing is listening,
// which is a normal state for a stopped runner and not an error to shout about.
func errIsNoRunner(err error) bool {
	return errors.Is(err, torrentqueue.ErrNoLiveRunner)
}

// queueState is everything the view holds. gen invalidates late messages: every
// refresh and every open bumps it, and a message whose gen is not current is
// dropped instead of being applied to a view it does not belong to.
type queueState struct {
	root       string
	entries    []torrentqueue.Entry
	cursor     int
	live       *torrentqueue.LiveSnapshot
	liveStatus string // human label for the live half
	durableErr string
	gen        int
	inflight   int
	// pendingSelect is the id to highlight once it appears in a durable read, so a
	// just-added entry is selected without guessing its row.
	pendingSelect string
	// discoveryMode remembers the last profile used for an add.
	discoveryMode bittorrent.Discovery
}

type queueDurableMsg struct {
	gen     int
	root    string
	entries []torrentqueue.Entry
	err     error
}

type queueLiveMsg struct {
	gen  int
	root string
	snap torrentqueue.LiveSnapshot
	err  error
}

// openQueueView enters the view without creating a queue, starting a runner or
// taking any lock: the durable read only lists records, and the live read is one
// request to an endpoint that may not exist.
func (m *model) openQueueView() (tea.Model, tea.Cmd) {
	root := m.queue.root
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = "."
		}
	}
	m.view = viewTorrentQueue
	// The generation counter survives re-entry, so a message from a previous visit to
	// this view can never match the generation of the new one.
	gen := m.queue.gen
	m.queue = queueState{root: root, gen: gen, liveStatus: "live status: not polled yet"}
	return m, m.refreshQueue()
}

// refreshQueue starts at most one durable read and one live read. While a refresh
// is in flight another one is refused, so a slow endpoint cannot pile up requests.
func (m *model) refreshQueue() tea.Cmd {
	if m.queue.inflight > 0 {
		return nil
	}
	m.queue.gen++
	m.queue.inflight = 2
	gen, root := m.queue.gen, m.queue.root
	return tea.Batch(m.queueDurableCmd(gen, root), m.queueLiveCmd(gen, root))
}

func (m *model) queueDurableCmd(gen int, root string) tea.Cmd {
	return func() tea.Msg {
		store, err := torrentqueue.OpenStore(root)
		if err != nil {
			return queueDurableMsg{gen: gen, root: root, err: err}
		}
		entries, lerr := store.List()
		return queueDurableMsg{gen: gen, root: root, entries: entries, err: lerr}
	}
}

func (m *model) queueLiveCmd(gen int, root string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		snap, err := torrentqueue.LiveSnapshotFor(ctx, root)
		return queueLiveMsg{gen: gen, root: root, snap: snap, err: err}
	}
}

// pollQueueCmd schedules the next refresh. It is only ever scheduled from a
// message handler, so it cannot outlive the view: the generation check on the way
// in drops anything that arrives after the user left.
func (m *model) pollQueueCmd() tea.Cmd {
	if m.view != viewTorrentQueue {
		return nil
	}
	return tea.Tick(livePollInterval, func(time.Time) tea.Msg { return queuePollMsg{gen: m.queue.gen} })
}

type queuePollMsg struct{ gen int }

func (m *model) updateQueue(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case queueDurableMsg:
		if msg.gen != m.queue.gen {
			return m, nil // late message from a previous view instance or root
		}
		if msg.err != nil {
			// A failed refresh keeps the rows it could not replace: the durable list is
			// still the last good read, and the failure is reported above it.
			m.queue.durableErr = msg.err.Error()
		} else {
			m.queue.entries = msg.entries
			m.queue.durableErr = ""
			m.reconcileQueueCursor()
			if m.queue.pendingSelect != "" {
				for i, e := range m.queue.entries {
					if e.ID == m.queue.pendingSelect {
						m.queue.cursor = i
						m.queue.pendingSelect = ""
						break
					}
				}
			}
		}
		m.finishQueueRefresh()
		return m, nil
	case queueLiveMsg:
		if msg.gen != m.queue.gen {
			return m, nil
		}
		switch {
		case msg.err == nil:
			snap := msg.snap
			m.queue.live = &snap
			m.queue.liveStatus = fmt.Sprintf("live: runner pid %d, started %s", snap.PID, snap.StartedAt.Format("15:04:05"))
		case errIsNoRunner(msg.err):
			m.queue.live = nil
			m.queue.liveStatus = "live status: unavailable (no foreground runner is reachable)"
		default:
			m.queue.live = nil
			m.queue.liveStatus = "live status: unavailable (" + msg.err.Error() + ")"
		}
		m.finishQueueRefresh()
		return m, nil
	case queuePollMsg:
		if msg.gen != m.queue.gen || m.view != viewTorrentQueue {
			return m, nil
		}
		return m, m.refreshQueue()
	case tea.KeyMsg:
		return m.updateQueueKeys(msg)
	}
	return m, nil
}

func (m *model) finishQueueRefresh() {
	if m.queue.inflight > 0 {
		m.queue.inflight--
	}
}

func (m *model) updateQueueKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	total := len(m.queueRows())
	switch msg.String() {
	case "esc", "backspace":
		m.view = viewMain
		m.queue.inflight = 0
		m.queue.gen++ // invalidate anything still in flight
		return m, nil
	case "up", "k":
		if m.queue.cursor > 0 {
			m.queue.cursor--
		}
	case "down", "j":
		if m.queue.cursor < total-1 {
			m.queue.cursor++
		}
	case "a":
		return m.openTorrentAdd()
	case "r":
		return m, m.refreshQueue()
	}
	return m, m.pollQueueCmd()
}

// reconcileQueueCursor keeps the highlighted entry by id when the list changes.
func (m *model) reconcileQueueCursor() {
	if len(m.queue.entries) == 0 {
		m.queue.cursor = 0
		return
	}
	if m.queue.cursor >= len(m.queue.entries) {
		m.queue.cursor = len(m.queue.entries) - 1
	}
	if m.queue.cursor < 0 {
		m.queue.cursor = 0
	}
}

// queueRows is the durable list, one row per entry.
func (m *model) queueRows() []torrentqueue.Entry { return m.queue.entries }

// liveAttempts maps entry id to its live attempt, and returns the attempts that have
// no durable row so they can be shown as live-only.
func (m *model) liveByEntry() (map[string]torrentqueue.LiveAttempt, []torrentqueue.LiveAttempt) {
	if m.queue.live == nil {
		return nil, nil
	}
	known := make(map[string]bool, len(m.queue.entries))
	for _, e := range m.queue.entries {
		known[e.ID] = true
	}
	byID := make(map[string]torrentqueue.LiveAttempt, len(m.queue.live.Attempts))
	var orphans []torrentqueue.LiveAttempt
	for _, a := range m.queue.live.Attempts {
		if known[a.EntryID] {
			byID[a.EntryID] = a
			continue
		}
		orphans = append(orphans, a)
	}
	return byID, orphans
}

func (m *model) viewTorrentQueue() string {
	var b strings.Builder
	width := m.width
	if width <= 0 {
		width = 100
	}
	fmt.Fprintf(&b, "Torrent queue — %s\n", m.queue.root)
	if m.queue.live != nil {
		slots := m.queue.live
		// The header is derived from the snapshot that is actually being rendered, so a
		// label left over from an earlier poll can never describe a different snapshot.
		fmt.Fprintf(&b, "live: runner pid %d, started %s   download slots %d/%d   seed slots %d/%d\n",
			slots.PID, slots.StartedAt.Format("15:04:05"),
			slots.Downloads.Used, slots.Downloads.Max, slots.Seeds.Used, slots.Seeds.Max)
	} else {
		fmt.Fprintf(&b, "%s\n", m.queue.liveStatus)
	}
	if m.queue.durableErr != "" {
		fmt.Fprintf(&b, "durable read failed: %s\n", m.queue.durableErr)
	}
	b.WriteString("\n")
	if len(m.queue.entries) == 0 && m.queue.durableErr == "" {
		b.WriteString("no queue entries\n")
	}
	byID, orphans := m.liveByEntry()
	for i, e := range m.queue.entries {
		marker := "  "
		if i == m.queue.cursor {
			marker = "> "
		}
		recorded := string(e.Phase)
		if recorded == "" {
			recorded = "recorded: ?"
		}
		fmt.Fprintf(&b, "%s%-26s desired=%-6s recorded=%-9s", marker, trim(e.ID, 26), string(e.Desired), recorded)
		if a, ok := byID[e.ID]; ok {
			fmt.Fprintf(&b, "  live=%s %s", a.Kind, livePhaseText(a))
		}
		b.WriteString("\n")
	}
	if len(orphans) > 0 {
		b.WriteString("\nlive only (no durable entry with this id):\n")
		for _, a := range orphans {
			fmt.Fprintf(&b, "  %-26s %s %s\n", trim(a.EntryID, 26), a.Kind, livePhaseText(a))
		}
	}
	b.WriteString("\n↑/↓ move • a add torrent • r refresh • esc back\n")
	return clipLines(b.String(), width)
}

// livePhaseText renders exactly what the snapshot reported, and nothing else: a
// missing counter stays absent instead of becoming a zero.
func livePhaseText(a torrentqueue.LiveAttempt) string {
	out := a.Phase
	if a.Stopping {
		out += " (stopping; slot held until close)"
	}
	if a.UploadedBytes != nil {
		out += fmt.Sprintf("  uploaded=%s", humanBytes(*a.UploadedBytes))
	}
	return out
}

func clipLines(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = trim(line, width)
	}
	return strings.Join(lines, "\n")
}
