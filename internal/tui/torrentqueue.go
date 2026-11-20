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
	// readSeq orders durable reads within one view instance: a read begun before a
	// write must never overwrite a read begun after it.
	readSeq int
	// appliedSeq is the highest durable read already rendered.
	appliedSeq int
	// pendingRefresh is set when a refresh was refused because one was in flight, so
	// the post-write refresh is not silently dropped until the next tick.
	pendingRefresh bool
	// pendingSelect is the id to highlight once it appears in a durable read, so a
	// just-added entry is selected without guessing its row.
	pendingSelect string
	// discoveryMode remembers the last profile used for an add.
	discoveryMode bittorrent.Discovery
	// actionBusy is true while one durable action is being written, so repeated key
	// presses cannot queue a second write for the same row.
	actionBusy bool
	// status reports the outcome of the last action, phrased as what was saved.
	status    string
	statusErr bool
}

type queueDurableMsg struct {
	gen     int
	seq     int
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
	gen := m.queue.gen + 1
	m.queue = queueState{root: root, gen: gen, liveStatus: "live status: not polled yet"}
	return m, m.refreshQueue()
}

// refreshQueue starts at most one durable read and one live read. While a refresh
// is in flight another one is refused, so a slow endpoint cannot pile up requests.
func (m *model) refreshQueue() tea.Cmd {
	if m.queue.inflight > 0 {
		// A write just finished while a poll is still in flight: remember that a refresh
		// is owed instead of dropping it, and run it when the poll drains.
		m.queue.pendingRefresh = true
		return nil
	}
	// Deliberately NOT bumping the generation here: a poll runs every two seconds and
	// would otherwise invalidate an action that is still in flight, dropping its result.
	// The generation changes only when the view instance, the root or the intent to
	// display the view changes.
	m.queue.inflight = 2
	m.queue.pendingRefresh = false
	m.queue.readSeq++
	gen, root, seq := m.queue.gen, m.queue.root, m.queue.readSeq
	return tea.Batch(m.queueDurableCmd(gen, seq, root), m.queueLiveCmd(gen, root))
}

func (m *model) queueDurableCmd(gen, seq int, root string) tea.Cmd {
	return func() tea.Msg {
		store, err := torrentqueue.OpenStore(root)
		if err != nil {
			return queueDurableMsg{gen: gen, seq: seq, root: root, err: err}
		}
		entries, lerr := store.List()
		return queueDurableMsg{gen: gen, seq: seq, root: root, entries: entries, err: lerr}
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

// queueActionMsg is the result of one durable action. It carries the id it was for, so
// a late result can neither touch another row nor another root's view.
type queueActionMsg struct {
	gen    int
	root   string
	id     string
	action string
	entry  torrentqueue.Entry
	err    error
}

func (m *model) queueActionCmd(gen int, root, id, action string) tea.Cmd {
	return func() tea.Msg {
		store, err := torrentqueue.OpenStore(root)
		if err != nil {
			return queueActionMsg{gen: gen, root: root, id: id, action: action, err: err}
		}
		var entry torrentqueue.Entry
		var aerr error
		switch action {
		case "start":
			entry, aerr = store.Start(id)
		case "pause":
			entry, aerr = store.Pause(id)
		case "retry":
			entry, aerr = store.Retry(id)
		case "cancel":
			entry, aerr = store.Cancel(id)
		case "import":
			// The handoff is a local writer. Refuse while a runner owns the queue so
			// two importers cannot race the same capture.
			owner, lerr := store.LockRunner()
			if lerr != nil {
				aerr = lerr
			} else {
				entry, aerr = store.ImportCapture(id)
				_ = owner.Release()
			}
		case "seed-enable":
			entry, aerr = store.EnableSeed(id)
		case "seed-disable":
			entry, aerr = store.DisableSeed(id)
		default:
			aerr = fmt.Errorf("unknown queue action %q", action)
		}
		return queueActionMsg{gen: gen, root: root, id: id, action: action, entry: entry, err: aerr}
	}
}

// liveAttemptFor is the runner's current observation of one entry, if any.
func (m *model) liveAttemptFor(id string) (torrentqueue.LiveAttempt, bool) {
	if m.queue.live == nil {
		return torrentqueue.LiveAttempt{}, false
	}
	for _, a := range m.queue.live.Attempts {
		if a.EntryID == id {
			return a, true
		}
	}
	return torrentqueue.LiveAttempt{}, false
}

// describeAction phrases a result as what was SAVED, never as what the runner did.
func (m *model) describeAction(msg queueActionMsg) string {
	id := msg.id
	switch msg.action {
	case "start":
		live := ""
		if a, ok := m.liveAttemptFor(id); ok {
			live = "; the runner is already " + a.Phase
		} else if m.queue.live == nil {
			live = "; no runner is running, so nothing starts until one is"
		}
		return fmt.Sprintf("saved start intent for %s (desired=run, phase=%s)%s", id, msg.entry.Phase, live)
	case "pause":
		detail := "; recorded as paused"
		if a, ok := m.liveAttemptFor(id); ok {
			// Any live attempt means the runner has not acted on the request yet: it
			// keeps its slot until its client closes.
			detail = fmt.Sprintf("; the runner has not acknowledged yet — it is still %s and keeps its slot until it closes", a.Phase)
		}
		return fmt.Sprintf("saved pause intent for %s (desired=paused)%s", id, detail)
	case "retry":
		return fmt.Sprintf("saved retry for %s (desired=run, phase=%s, errors cleared)", id, msg.entry.Phase)
	case "cancel":
		live := ""
		if a, ok := m.liveAttemptFor(id); ok {
			live = fmt.Sprintf("; the runner has not acknowledged yet — it is still %s and keeps its slot until it closes", a.Phase)
		}
		return fmt.Sprintf("saved cancel for %s (desired=cancelled, phase=%s)%s", id, msg.entry.Phase, live)
	case "import":
		if msg.entry.Import == nil {
			return fmt.Sprintf("imported %s", id)
		}
		return fmt.Sprintf("imported %s into %s/%s (revision %s)", id,
			msg.entry.Import.Target.VaultRoot, msg.entry.Import.Target.Collection, msg.entry.Import.RevisionID)
	case "seed-enable":
		live := ""
		if a, ok := m.liveAttemptFor(id); ok {
			live = fmt.Sprintf("; the runner is already %s with this seed", a.Phase)
		} else {
			live = "; seeding starts only in a run with --max-seeds"
		}
		return fmt.Sprintf("saved seed intent: enabled for %s (seed_desired=%s, phase=%s)%s",
			id, msg.entry.SeedDesired, msg.entry.SeedPhase, live)
	case "seed-disable":
		detail := "; recorded as disabled"
		if a, ok := m.liveAttemptFor(id); ok {
			detail = fmt.Sprintf("; the runner has not acknowledged yet — it is still %s and keeps its slot until it closes", a.Phase)
		}
		return fmt.Sprintf("saved seed intent: disabled for %s (seed_desired=%s, phase=%s)%s",
			id, msg.entry.SeedDesired, msg.entry.SeedPhase, detail)
	}
	return "saved"
}

func (m *model) updateQueue(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case queueDurableMsg:
		if msg.gen != m.queue.gen {
			return m, nil // late message from a previous view instance or root
		}
		if msg.seq != 0 && msg.seq < m.queue.appliedSeq {
			// A read begun before a newer one has arrived: applying it would show state
			// the user already moved past.
			return m, nil
		}
		if msg.seq > m.queue.appliedSeq {
			m.queue.appliedSeq = msg.seq
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
		return m, m.finishQueueRefresh()
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
		return m, m.finishQueueRefresh()
	case queueActionMsg:
		if msg.gen != m.queue.gen || msg.root != m.queue.root || m.view != viewTorrentQueue {
			return m, nil // late result: another view, another root, or a newer generation
		}
		m.queue.actionBusy = false
		if msg.err != nil {
			m.queue.status = msg.action + " refused: " + msg.err.Error()
			m.queue.statusErr = true
			return m, nil
		}
		m.queue.statusErr = false
		m.queue.status = m.describeAction(msg)
		m.queue.pendingSelect = msg.id
		return m, m.refreshQueue()
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

// finishQueueRefresh drains one in-flight read and returns the refresh that was owed
// while it ran, so a write that finished during a poll still gets a post-write read.
func (m *model) finishQueueRefresh() tea.Cmd {
	if m.queue.inflight > 0 {
		m.queue.inflight--
	}
	if m.queue.inflight != 0 || !m.queue.pendingRefresh || m.view != viewTorrentQueue {
		return nil
	}
	m.queue.pendingRefresh = false
	m.queue.inflight = 2
	m.queue.readSeq++
	gen, root, seq := m.queue.gen, m.queue.root, m.queue.readSeq
	return tea.Batch(m.queueDurableCmd(gen, seq, root), m.queueLiveCmd(gen, root))
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
	case "s", "p", "t", "x", "i":
		return m.queueAction(msg.String())
	case "e", "d":
		return m.queueAction(msg.String())
	case "r":
		return m, m.refreshQueue()
	}
	return m, m.pollQueueCmd()
}

// queueAction applies one durable operation to the highlighted entry. The id comes
// from the record, never from the row position, and the store re-validates the
// transition at write time because the rendered row may already be stale.
func (m *model) queueAction(key string) (tea.Model, tea.Cmd) {
	if m.queue.actionBusy {
		return m, nil // one write at a time: repeated keys cannot double-apply
	}
	rows := m.queueRows()
	if len(rows) == 0 {
		m.queue.status = "no entry selected"
		m.queue.statusErr = true
		return m, nil
	}
	id := rows[m.queue.cursor].ID
	action := map[string]string{
		"s": "start", "p": "pause", "t": "retry", "x": "cancel", "i": "import",
		"e": "seed-enable", "d": "seed-disable",
	}[key]
	m.queue.actionBusy = true
	m.queue.status = action + " requested…"
	m.queue.statusErr = false
	return m, m.queueActionCmd(m.queue.gen, m.queue.root, id, action)
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
	if m.queue.status != "" {
		fmt.Fprintf(&b, "saved: %s\n", m.queue.status)
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
		seed := ""
		if e.SeedDesired != "" || e.SeedPhase != "" {
			seed = fmt.Sprintf("  seed=%s/%s", seedText(e.SeedDesired), e.SeedPhase)
		}
		handoff := ""
		if e.Import != nil {
			handoff = fmt.Sprintf("  import=%s", e.Import.State)
		}
		fmt.Fprintf(&b, "%s%-26s desired=%-6s recorded=%-9s%s%s", marker, trim(e.ID, 26), string(e.Desired), recorded, seed, handoff)
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
	b.WriteString("\n↑/↓ move • s start • p pause • x cancel • t retry • i import • e seed • d unseed • a add • r refresh • esc back\n")
	return clipLines(b.String(), width)
}

// livePhaseText renders exactly what the snapshot reported, and nothing else: a
// missing counter stays absent instead of becoming a zero.
// seedText renders seed intent, which is independent of download intent.
func seedText(d torrentqueue.SeedDesired) string {
	if d == "" {
		return "off"
	}
	return string(d)
}

func livePhaseText(a torrentqueue.LiveAttempt) string {
	out := a.Phase
	if a.Stopping {
		out += " (stopping; slot held until close)"
	}
	if a.UploadedBytes != nil {
		out += fmt.Sprintf("  uploaded=%s", humanBytes(*a.UploadedBytes))
	}
	// Selected progress appears only when the snapshot carried it, and the rate only
	// when the runner measured one: an absent value stays absent instead of becoming
	// "0 B". The rate is progress on the selected payload, not network throughput.
	if a.SelectedBytes != nil && a.SelectedTotal != nil {
		out += fmt.Sprintf("  selected=%s/%s", humanBytes(*a.SelectedBytes), humanBytes(*a.SelectedTotal))
		if a.ProgressRate != nil {
			out += fmt.Sprintf("  progress=%s/s", humanBytes(*a.ProgressRate))
		}
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
