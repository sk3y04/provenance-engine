package tui

// BT-10c: add a torrent to the paused queue from the TUI.
//
// Inspection uses the metadata-only API: a local .torrent is parsed offline, a magnet
// gets a short-lived client that carries no payload storage. Adding goes through the
// same admission path as the CLI (torrentqueue.Admit), so duplicates, conflicts and
// unsupported sources are decided in one place. Nothing here starts a runner, opens
// a workspace or records a download intent.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

type addStage int

const (
	addForm addStage = iota
	addInspecting
	addPick
	addReview
)

// inspectCancelWindow bounds the metadata exchange started from the TUI, so a
// picker cannot hold a client open indefinitely.
const inspectCancelWindow = 2 * time.Minute

type torrentAddState struct {
	stage  addStage
	source textinput.Model
	mode   bittorrent.Discovery
	// gen invalidates late inspection and add results: every new inspection bumps it.
	gen  int
	err  string
	busy bool

	info   *bittorrent.Info
	files  []bittorrent.File
	picked map[string]bool
	cursor int
	offset int
}

type torrentInspectMsg struct {
	gen  int
	root string
	info bittorrent.Info
	err  error
}

type torrentAdmitMsg struct {
	gen       int
	entry     torrentqueue.Entry
	duplicate bool
	err       error
}

func newTorrentAddState(root string) torrentAddState {
	in := textinput.New()
	in.Placeholder = "path/to/file.torrent or magnet:?xt=urn:btih:…"
	in.CharLimit = 4096
	// The field is masked so a magnet carrying tracker credentials is never echoed to
	// the screen, a scrollback buffer or a bug report.
	in.EchoMode = textinput.EchoPassword
	in.Focus()
	return torrentAddState{
		stage:  addForm,
		source: in,
		mode:   bittorrent.DiscoveryPublic,
		picked: map[string]bool{},
	}
}

func (m *model) openTorrentAdd() (tea.Model, tea.Cmd) {
	if m.queue.root == "" {
		m.queue.root = "."
	}
	m.view = viewTorrentAdd
	m.add = newTorrentAddState(m.queue.root)
	m.add.mode = m.queueDiscovery()
	m.queue.gen++ // leaving the queue view invalidates anything it had in flight
	return m, textinput.Blink
}

// discovery is the profile the picker offers: the last one used, else public.
func (m *model) queueDiscovery() bittorrent.Discovery {
	if m.queue.discoveryMode == "" {
		return bittorrent.DiscoveryPublic
	}
	return m.queue.discoveryMode
}

func (m *model) inspectCmd(gen int, root, source string, mode bittorrent.Discovery) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), inspectCancelWindow)
		defer cancel()
		info, err := bittorrent.Inspect(ctx, source, bittorrent.InspectConfig{Discovery: mode})
		return torrentInspectMsg{gen: gen, root: root, info: info, err: err}
	}
}

func (m *model) admitCmd(gen int, root, source string, mode bittorrent.Discovery, selection []string) tea.Cmd {
	return func() tea.Msg {
		store, err := torrentqueue.OpenStore(root)
		if err != nil {
			return torrentAdmitMsg{gen: gen, err: err}
		}
		admitted, aerr := torrentqueue.Admit(store, source, mode, nil, selection, nil)
		return torrentAdmitMsg{gen: gen, entry: admitted.Entry, duplicate: admitted.Duplicate, err: aerr}
	}
}

func (m *model) updateTorrentAdd(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case torrentInspectMsg:
		if msg.gen != m.add.gen || m.view != viewTorrentAdd {
			return m, nil // obsolete inspection: dropped, never applied
		}
		m.add.busy = false
		if msg.err != nil {
			m.add.err = sanitizeAddError(msg.err, m.add.source.Value())
			m.add.stage = addForm
			return m, nil
		}
		m.add.err = ""
		info := msg.info
		if info.InfoHash == "" && info.InfoHashV2 != "" {
			// Inspectable, but a v2-only identity cannot be resumed by the fetch path,
			// so this picker must not offer a successful add for it.
			m.add.err = "this torrent is v2-only: it can be inspected but not queued for fetch"
			m.add.stage = addForm
			return m, nil
		}
		m.add.info = &info
		m.add.files = info.Files
		m.add.picked = map[string]bool{}
		m.add.cursor, m.add.offset = 0, 0
		m.add.stage = addPick
		return m, nil
	case torrentAdmitMsg:
		if msg.gen != m.add.gen || m.view != viewTorrentAdd {
			return m, nil
		}
		m.add.busy = false
		if msg.err != nil {
			m.add.err = sanitizeAddError(msg.err, m.add.source.Value())
			return m, nil // stay on the review step with the selection intact
		}
		m.queue.pendingSelect = msg.entry.ID
		m.queue.discoveryMode = m.add.mode
		m.add = torrentAddState{}
		m.view = viewTorrentQueue
		return m, m.refreshQueue()
	case tea.KeyMsg:
		return m.updateTorrentAddKeys(msg)
	}
	return m, nil
}

func (m *model) updateTorrentAddKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.add.stage {
	case addForm:
		switch key {
		case "esc":
			m.view = viewTorrentQueue
			m.add.gen++
			return m, m.refreshQueue()
		case "ctrl+d":
			m.add.mode = nextDiscovery(m.add.mode)
			return m, nil
		case "enter":
			src := strings.TrimSpace(m.add.source.Value())
			if src == "" {
				m.add.err = "enter a .torrent path or a magnet URI"
				return m, nil
			}
			if m.add.busy {
				return m, nil
			}
			m.add.gen++
			m.add.busy = true
			m.add.err = ""
			m.add.stage = addInspecting
			return m, m.inspectCmd(m.add.gen, m.queue.root, src, m.add.mode)
		}
		var cmd tea.Cmd
		m.add.source, cmd = m.add.source.Update(msg)
		return m, cmd

	case addInspecting:
		if key == "esc" {
			// Cancel the in-progress inspection by moving on: the generation bump means
			// its result can never be applied, and the bounded context closes the client.
			m.add.gen++
			m.add.busy = false
			m.add.stage = addForm
			m.add.err = "inspection cancelled"
		}
		return m, nil

	case addPick:
		switch key {
		case "esc":
			m.add.stage = addForm
		case "up", "k":
			if m.add.cursor > 0 {
				m.add.cursor--
			}
		case "down", "j":
			if m.add.cursor < len(m.add.files)-1 {
				m.add.cursor++
			}
		case " ":
			if len(m.add.files) > 0 {
				path := m.add.files[m.add.cursor].Path
				m.add.picked[path] = !m.add.picked[path]
			}
		case "a":
			for _, f := range m.add.files {
				m.add.picked[f.Path] = true
			}
		case "n":
			m.add.picked = map[string]bool{}
		case "enter":
			if len(m.selectedPaths()) == 0 {
				m.add.err = "select at least one file: an empty selection is never treated as \"all files\""
				return m, nil
			}
			m.add.err = ""
			m.add.stage = addReview
		}
		return m, nil

	case addReview:
		switch key {
		case "esc":
			m.add.stage = addPick
		case "enter":
			if m.add.busy {
				return m, nil // single flight: a second enter cannot add another entry
			}
			m.add.busy = true
			m.add.err = ""
			return m, m.admitCmd(m.add.gen, m.queue.root, m.add.source.Value(), m.add.mode, m.selectedPaths())
		}
		return m, nil
	}
	return m, nil
}

// selectedPaths returns the exact relative paths that are selected, in torrent order.
func (m *model) selectedPaths() []string {
	out := make([]string, 0, len(m.add.picked))
	for _, f := range m.add.files {
		if m.add.picked[f.Path] {
			out = append(out, f.Path)
		}
	}
	return out
}

func (m *model) selectedTotals() (int, int64) {
	var total int64
	n := 0
	for _, f := range m.add.files {
		if m.add.picked[f.Path] {
			n++
			total += f.Length
		}
	}
	return n, total
}

func nextDiscovery(d bittorrent.Discovery) bittorrent.Discovery {
	switch d {
	case bittorrent.DiscoveryPublic:
		return bittorrent.DiscoveryTracker
	case bittorrent.DiscoveryTracker:
		return bittorrent.DiscoveryDirect
	default:
		return bittorrent.DiscoveryPublic
	}
}

// sanitizeAddError removes the raw source from an error, because a magnet can carry
// tracker credentials and an error string is rendered to the screen.
func sanitizeAddError(err error, source string) string {
	msg := err.Error()
	if source != "" {
		msg = strings.ReplaceAll(msg, source, "<source>")
	}
	if i := strings.Index(msg, "magnet:?"); i >= 0 {
		end := strings.IndexAny(msg[i:], " \n")
		if end < 0 {
			msg = msg[:i] + "<magnet>"
		} else {
			msg = msg[:i] + "<magnet>" + msg[i+end:]
		}
	}
	return msg
}

func (m *model) viewTorrentAdd() string {
	var b strings.Builder
	width := m.width
	if width <= 0 {
		width = 100
	}
	switch m.add.stage {
	case addForm:
		fmt.Fprintf(&b, "Add torrent — %s\n\n", m.queue.root)
		fmt.Fprintf(&b, "source (masked): %s\n", m.add.source.View())
		fmt.Fprintf(&b, "discovery: %s   (ctrl+d cycles; a local .torrent is read offline)\n", m.add.mode)
		b.WriteString("\nenter inspect • esc back\n")
	case addInspecting:
		fmt.Fprintf(&b, "Inspecting %s…\n\nmetadata only; no payload is requested.\nesc cancels\n", m.add.mode)
	case addPick:
		info := m.add.info
		fmt.Fprintf(&b, "Pick files — %s (%s)\n\n", safeName(info), humanBytes(info.TotalLength))
		rows := m.pickWindow()
		for i, f := range rows {
			idx := m.add.offset + i
			mark := "[ ]"
			if m.add.picked[f.Path] {
				mark = "[x]"
			}
			cursor := "  "
			if idx == m.add.cursor {
				cursor = "> "
			}
			fmt.Fprintf(&b, "%s%s %-60s %10s\n", cursor, mark, trim(f.Path, 60), humanBytes(f.Length))
		}
		n, total := m.selectedTotals()
		fmt.Fprintf(&b, "\n%d/%d files selected, %s\n", n, len(m.add.files), humanBytes(total))
		b.WriteString("space toggle • a all • n none • ↑/↓ move • enter review • esc back\n")
	case addReview:
		n, total := m.selectedTotals()
		b.WriteString("Add to queue — review\n\n")
		fmt.Fprintf(&b, "queue root:  %s\n", m.queue.root)
		fmt.Fprintf(&b, "source:      %s\n", safeSource(m.add.info))
		fmt.Fprintf(&b, "discovery:   %s\n", m.add.mode)
		fmt.Fprintf(&b, "selected:    %d of %d files, %s\n", n, len(m.add.files), humanBytes(total))
		b.WriteString("state:       will be added paused (nothing starts downloading)\n")
		b.WriteString("\nenter confirm • esc back to the file list\n")
	}
	if m.add.busy && m.add.stage != addInspecting {
		b.WriteString("\nadding…\n")
	}
	if m.add.err != "" {
		fmt.Fprintf(&b, "\n%s\n", m.add.err)
	}
	return clipLines(b.String(), width)
}

// pickWindow keeps the highlighted file visible without scrolling the whole list.
func (m *model) pickWindow() []bittorrent.File {
	height := m.height - 8
	if height < 5 {
		height = 5
	}
	if m.add.cursor < m.add.offset {
		m.add.offset = m.add.cursor
	}
	if m.add.cursor >= m.add.offset+height {
		m.add.offset = m.add.cursor - height + 1
	}
	if m.add.offset < 0 {
		m.add.offset = 0
	}
	end := m.add.offset + height
	if end > len(m.add.files) {
		end = len(m.add.files)
	}
	if m.add.offset > end {
		m.add.offset = end
	}
	return m.add.files[m.add.offset:end]
}

func safeName(info *bittorrent.Info) string {
	if info == nil || info.Name == "" {
		return "<unicorn>"
	}
	return info.Name
}

// safeSource is the sanitised identity shown in review: an infohash or the sanitised
// source string the inspection produced, never the raw input.
func safeSource(info *bittorrent.Info) string {
	if info == nil {
		return "<unknown>"
	}
	if info.InfoHash != "" {
		return "btih:" + info.InfoHash
	}
	if info.InfoHashV2 != "" {
		return "btmh:" + info.InfoHashV2
	}
	if info.Source != "" {
		return info.Source
	}
	return "<unknown>"
}
