package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance/internal/catalog"
	"github.com/sk3y04/provenance/internal/collection"
)

func (m *model) loadCollectionsCmd() tea.Cmd {
	return func() tea.Msg {
		cols, err := collection.List()
		return collectionsLoadedMsg{cols: cols, err: err}
	}
}

type collectionsLoadedMsg struct {
	cols []collection.Collection
	err  error
}

func (m *model) updateCollections(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.collFilter.active {
		switch msg.String() {
		case "esc":
			m.collFilter.active = false
			m.collFilter.input.SetValue("")
			m.collFilter.input.Blur()
			m.collCur = 0
			m.collPending = pendingDelete{}
			return m, nil
		case "enter":
			m.collFilter.active = false
			m.collFilter.input.Blur()
			m.collFilterSaved = m.collFilter.input.Value()
			m.collPending = pendingDelete{}
			return m, nil
		default:
			var cmd tea.Cmd
			m.collFilter.input, cmd = m.collFilter.input.Update(msg)
			m.collCur = 0
			return m, cmd
		}
	}

	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewMain
		m.collFilter.active = false
		m.collPending = pendingDelete{}
		return m, nil
	case "/":
		m.collFilter.active = true
		m.collFilter.input.SetValue(m.collFilterSaved)
		m.collFilter.input.Focus()
		return m, nil
	case "up", "k":
		if m.collCur > 0 {
			m.collCur--
		}
	case "down", "j":
		visible := m.visibleCollections()
		if m.collCur < len(visible)-1 {
			m.collCur++
		}
	case "enter":
		visible := m.visibleCollections()
		if m.collCur < len(visible) {
			c := visible[m.collCur]
			m.collSelected = &c
			m.view = viewCollectionDetail
		}
	case "s":
		visible := m.visibleCollections()
		if m.collCur < len(visible) {
			c := visible[m.collCur]
			return m, m.syncCollectionCmd(c.Name)
		}
	case "S":
		return m, m.syncAllCmd()
	case "a":
		visible := m.visibleCollections()
		if m.collCur < len(visible) {
			c := visible[m.collCur]
			return m, m.archiveCollectionCmd(c)
		}
	case "d":
		visible := m.visibleCollections()
		if len(visible) == 0 {
			return m, nil
		}
		name := visible[m.collCur].Name
		if m.collPending.name == name && time.Since(m.collPending.at) < confirmWindow {
			if err := m.removeCollection(name); err != nil {
				m.err = err.Error()
			} else {
				m.info = "removed collection " + name
			}
			m.collPending = pendingDelete{}
			return m, m.loadCollectionsCmd()
		}
		m.collPending = pendingDelete{name: name, at: time.Now(), revs: 0}
		if catalog.HasStore() {
			return m, m.loadCollectionRevsCmd(name)
		}
		m.info = fmt.Sprintf("press d again within %ds to remove %q", int(confirmWindow.Seconds()), name)
		return m, nil
	}
	return m, nil
}

func (m *model) updateCollectionDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewCollections
		m.collSelected = nil
		m.collPending = pendingDelete{}
		return m, m.loadCollectionsCmd()
	case "s":
		if m.collSelected != nil {
			return m, m.syncCollectionCmd(m.collSelected.Name)
		}
	case "a":
		if m.collSelected != nil {
			return m, m.archiveCollectionCmd(*m.collSelected)
		}
	case "d":
		if m.collSelected == nil {
			return m, nil
		}
		name := m.collSelected.Name
		if m.collPending.name == name && time.Since(m.collPending.at) < confirmWindow {
			if err := m.removeCollection(name); err != nil {
				m.err = err.Error()
			} else {
				m.info = "removed collection " + name
			}
			m.collPending = pendingDelete{}
			m.view = viewCollections
			m.collSelected = nil
			return m, m.loadCollectionsCmd()
		}
		m.collPending = pendingDelete{name: name, at: time.Now(), revs: 0}
		if catalog.HasStore() {
			return m, m.loadCollectionRevsCmd(name)
		}
		m.info = fmt.Sprintf("press d again within %ds to remove %q", int(confirmWindow.Seconds()), name)
		return m, nil
	}
	return m, nil
}

func (m *model) visibleCollections() []collection.Collection {
	if !m.collFilter.active {
		return m.collections
	}
	q := strings.ToLower(strings.TrimSpace(m.collFilter.input.Value()))
	var filtered []collection.Collection
	for _, c := range m.collections {
		if strings.Contains(strings.ToLower(c.Name), q) || strings.Contains(strings.ToLower(c.URL), q) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func (m *model) viewCollections() string {
	var b strings.Builder
	b.WriteString(sectionHeader.Render("Collections") + "\n\n")

	if m.collFilter.active {
		b.WriteString(m.collFilter.input.View())
		b.WriteString("\n\n")
	}

	visible := m.visibleCollections()
	if len(visible) == 0 {
		if len(m.collections) == 0 {
			b.WriteString(dim.Render("No collections. Use 'provenance collect add ...' first."))
		} else {
			b.WriteString(dim.Render("(no matches)"))
		}
		return b.String()
	}

	fmt.Fprintf(&b, "  %-24s %-19s %6s %6s %s\n", "Name", "Last sync", "New", "Skip", "URL")
	for i, c := range visible {
		cursor := "  "
		if i == m.collCur {
			cursor = highlight.Render("▶ ")
		}
		last := "never"
		newCnt, skipCnt := "-", "-"
		if !c.LastSync.IsZero() {
			last = c.LastSync.Format("2006-01-02 15:04")
		}
		if c.LastResult != nil {
			newCnt = fmt.Sprintf("%d", c.LastResult.New)
			skipCnt = fmt.Sprintf("%d", c.LastResult.Skipped)
		}
		fmt.Fprintf(&b, "%s%-24s %-19s %6s %6s %s\n", cursor, c.Name, last, newCnt, skipCnt, trim(c.URL, 60))
	}
	if m.collPending.name != "" && time.Since(m.collPending.at) < confirmWindow {
		if m.collPending.revs > 0 {
			b.WriteString("\n" + warnStyle.Render(fmt.Sprintf("⚠  press d again to remove %q (+ %d vault revision(s))", m.collPending.name, m.collPending.revs)) + "\n")
		} else {
			b.WriteString("\n" + warnStyle.Render(fmt.Sprintf("⚠  press d again to confirm remove of %q", m.collPending.name)) + "\n")
		}
	}
	return b.String()
}

func (m *model) viewCollectionDetail() string {
	if m.collSelected == nil {
		return dim.Render("(no collection selected)")
	}
	c := m.collSelected
	var b strings.Builder
	b.WriteString(sectionHeader.Render("Collection: "+c.Name) + "\n\n")
	fmt.Fprintf(&b, "URL:      %s\n", c.URL)
	fmt.Fprintf(&b, "Site:     %s\n", c.Site)
	fmt.Fprintf(&b, "Output:   %s\n", c.Options.OutputDir)
	if c.Options.CookiesFile != "" {
		fmt.Fprintf(&b, "Cookies:  %s\n", c.Options.CookiesFile)
	}
	fmt.Fprintf(&b, "Created:  %s\n", c.CreatedAt.Format("2006-01-02 15:04"))
	lastSync := "never"
	if !c.LastSync.IsZero() {
		lastSync = c.LastSync.Format("2006-01-02 15:04")
	}
	fmt.Fprintf(&b, "LastSync: %s\n", lastSync)
	fmt.Fprintf(&b, "Seen:     %d item(s)\n", len(c.SeenIDs))
	if c.LastResult != nil {
		fmt.Fprintf(&b, "\nLast result (%s):\n", c.LastResult.At.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "  Total:   %d\n", c.LastResult.Total)
		fmt.Fprintf(&b, "  New:     %d\n", c.LastResult.New)
		fmt.Fprintf(&b, "  Skipped: %d\n", c.LastResult.Skipped)
		fmt.Fprintf(&b, "  Failed:  %d\n", c.LastResult.Failed)
	}
	if m.collPending.name != "" && time.Since(m.collPending.at) < confirmWindow {
		if m.collPending.revs > 0 {
			b.WriteString("\n" + warnStyle.Render(fmt.Sprintf("⚠  press d again to remove %q (+ %d vault revision(s))", m.collPending.name, m.collPending.revs)) + "\n")
		} else {
			b.WriteString("\n" + warnStyle.Render(fmt.Sprintf("⚠  press d again to confirm remove of %q", m.collPending.name)) + "\n")
		}
	}
	b.WriteString("\n" + dim.Render("[s] sync  [a] archive  [d] remove  [esc] back"))
	return b.String()
}

func (m *model) syncCollectionCmd(name string) tea.Cmd {
	return func() tea.Msg {
		opts := collection.SyncOptions{RateLimiter: m.rateLimiter, Record: true}
		result, err := collection.Sync(context.Background(), name, opts)
		return collectionSyncedMsg{name: name, result: result, err: err}
	}
}

func (m *model) syncAllCmd() tea.Cmd {
	return func() tea.Msg {
		opts := collection.SyncOptions{RateLimiter: m.rateLimiter, Record: true}
		err := collection.SyncAll(context.Background(), opts)
		return collectionSyncedMsg{name: "all", err: err}
	}
}

func (m *model) archiveCollectionCmd(c collection.Collection) tea.Cmd {
	return func() tea.Msg {
		return archiveCollectionMsg{name: c.Name, outputDir: c.Options.OutputDir, url: c.URL, site: c.Site}
	}
}

func (m *model) loadCollectionRevsCmd(name string) tea.Cmd {
	return func() tea.Msg {
		ids, err := catalog.Store().ListRevisionIDsByCollection(context.Background(), name)
		return collectionRevsMsg{name: name, revIDs: ids, err: err}
	}
}

func (m *model) removeCollection(name string) error {
	if catalog.HasStore() {
		if err := catalog.Store().DeleteRevisionsByCollection(context.Background(), name); err != nil {
			return fmt.Errorf("delete vault revisions: %w", err)
		}
	}
	return collection.Remove(name)
}

type collectionSyncedMsg struct {
	name   string
	result *collection.SyncResult
	err    error
}

type archiveCollectionMsg struct {
	name      string
	outputDir string
	url       string
	site      string
}

type collectionRevsMsg struct {
	name   string
	revIDs []string
	err    error
}
