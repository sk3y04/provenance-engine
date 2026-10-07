package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/catalog"
)

func (m *model) loadVaultCmd() tea.Cmd {
	return func() tea.Msg {
		hasVault := catalog.HasStore()
		var cols []archive.ArchiveCollection
		if hasVault {
			c, _ := catalog.Store().ListCollections(context.Background())
			cols = c
		}
		return vaultLoadedMsg{initialized: hasVault, cols: cols}
	}
}

type vaultLoadedMsg struct {
	initialized bool
	cols        []archive.ArchiveCollection
	err         error
}

type vaultRevisionsMsg struct {
	collection string
	revisions  []archive.Revision
	err        error
}

type vaultDiffMsg struct {
	result *archive.DiffResult
	err    error
}

func (m *model) updateVault(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h", "q":
		m.view = viewMain
		return m, nil
	case "up", "k":
		if m.vaultCur > 0 {
			m.vaultCur--
		}
	case "down", "j":
		if len(m.vaultCols) > 0 && m.vaultCur < len(m.vaultCols)-1 {
			m.vaultCur++
		}
	case "enter":
		if len(m.vaultCols) == 0 || m.vaultCur >= len(m.vaultCols) {
			return m, nil
		}
		col := m.vaultCols[m.vaultCur]
		m.vaultSelectedCol = &col
		m.vaultRevisions = nil
		m.vaultRevCur = 0
		m.vaultRevPhase = 0
		m.vaultRevA = ""
		m.vaultRevB = ""
		m.vaultDiff = nil
		m.vaultDiffLoading = false
		return m, m.loadRevisionsCmd(col.Name)
	}
	return m, nil
}

func (m *model) loadRevisionsCmd(collectionName string) tea.Cmd {
	return func() tea.Msg {
		if !catalog.HasStore() {
			return vaultRevisionsMsg{collection: collectionName, err: fmt.Errorf("vault not initialized")}
		}
		revisions, err := catalog.Store().ListRevisions(context.Background(), collectionName)
		if err != nil {
			return vaultRevisionsMsg{collection: collectionName, err: err}
		}
		return vaultRevisionsMsg{collection: collectionName, revisions: revisions}
	}
}

func (m *model) updateVaultRevisions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h", "q":
		m.view = viewVault
		m.vaultSelectedCol = nil
		m.vaultRevisions = nil
		m.vaultRevCur = 0
		m.vaultRevPhase = 0
		m.vaultRevA = ""
		m.vaultRevB = ""
		m.vaultDiff = nil
		return m, nil
	case "up", "k":
		if m.vaultRevCur > 0 {
			m.vaultRevCur--
		}
	case "down", "j":
		if len(m.vaultRevisions) > 0 && m.vaultRevCur < len(m.vaultRevisions)-1 {
			m.vaultRevCur++
		}
	case "1":
		if m.vaultRevPhase < 1 {
			if len(m.vaultRevisions) == 0 {
				return m, nil
			}
			m.vaultRevA = m.vaultRevisions[m.vaultRevCur].ID
			m.vaultRevPhase = 1
			m.vaultRevB = ""
			m.vaultDiff = nil
			return m, nil
		}
		// In phase 1, '1' reselects A
		if len(m.vaultRevisions) == 0 {
			return m, nil
		}
		m.vaultRevA = m.vaultRevisions[m.vaultRevCur].ID
		return m, nil
	case "2":
		if m.vaultRevPhase >= 1 {
			if len(m.vaultRevisions) == 0 {
				return m, nil
			}
			m.vaultRevB = m.vaultRevisions[m.vaultRevCur].ID
			if m.vaultRevA != "" && m.vaultRevB != "" && m.vaultRevA != m.vaultRevB {
				m.vaultDiffLoading = true
				return m, m.computeDiffCmd()
			}
			m.vaultDiff = nil
			return m, nil
		}
		// In phase 0, '2' goes straight to phase 1 (marks as B, then re-selects as A)
		if len(m.vaultRevisions) == 0 {
			return m, nil
		}
		m.vaultRevA = m.vaultRevisions[m.vaultRevCur].ID
		m.vaultRevB = m.vaultRevisions[m.vaultRevCur].ID
		m.vaultRevPhase = 1
		return m, nil
	}
	return m, nil
}

func (m *model) computeDiffCmd() tea.Cmd {
	return func() tea.Msg {
		if !catalog.HasStore() {
			return vaultDiffMsg{err: fmt.Errorf("vault not initialized")}
		}
		revA, errA := catalog.Store().GetRevision(context.Background(), m.vaultRevA)
		if errA != nil {
			return vaultDiffMsg{err: fmt.Errorf("load revision A: %w", errA)}
		}
		revB, errB := catalog.Store().GetRevision(context.Background(), m.vaultRevB)
		if errB != nil {
			return vaultDiffMsg{err: fmt.Errorf("load revision B: %w", errB)}
		}
		diff := archive.DiffRevisions(revA, revB)
		return vaultDiffMsg{result: diff}
	}
}

func (m *model) viewVaultRevisions() string {
	var b strings.Builder
	b.WriteString(sectionHeader.Render("Vault") + "\n\n")

	if m.vaultSelectedCol == nil {
		b.WriteString(dim.Render("No collection selected."))
		return b.String()
	}

	fmt.Fprintf(&b, "Collection: %s\n\n", m.vaultSelectedCol.Name)

	switch m.vaultRevPhase {
	case 0:
		b.WriteString(highlight.Render("Select revision A (press 1 to mark):") + "\n\n")
	case 1:
		fmt.Fprintf(&b, "  marked revision A: %s...\n\n", m.vaultRevA[:12])
		b.WriteString(highlight.Render("Select revision B (press 2 to compare):") + "\n\n")
	case 2:
		fmt.Fprintf(&b, "  marked revision A: %s...\n\n", m.vaultRevA[:12])
		fmt.Fprintf(&b, "  marked revision B: %s...\n\n", m.vaultRevB[:12])
	}

	if m.vaultDiffLoading {
		b.WriteString("computing diff...\n")
		return b.String()
	}

	if m.vaultDiff != nil {
		return m.renderDiff(&b)
	}

	// Render revision list
	if len(m.vaultRevisions) == 0 {
		b.WriteString(dim.Render("No revisions found."))
		return b.String()
	}

	for i, rev := range m.vaultRevisions {
		cursor := "  "
		if i == m.vaultRevCur {
			cursor = highlight.Render("▶ ")
		}
		idShort := rev.ID
		if len(idShort) > 12 {
			idShort = idShort[:12]
		}
		dateStr := rev.CapturedAt.Format("2006-01-02 15:04")
		entityCount := len(rev.Entities)

		statusMark := ""
		if m.vaultRevPhase >= 0 && rev.ID == m.vaultRevA {
			statusMark = " ← A"
		}
		if m.vaultRevPhase >= 1 && rev.ID == m.vaultRevB {
			statusMark = " ← B"
		}

		fmt.Fprintf(&b, "%s%s %s %d entities%s\n", cursor, idShort, dateStr, entityCount, statusMark)
	}

	switch m.vaultRevPhase {
	case 0:
		b.WriteString("\n" + dim.Render("[1] mark A  [2] mark A+B  [esc] back"))
	case 1:
		b.WriteString("\n" + dim.Render("[1] reselect A  [2] mark B + diff  [esc] back"))
	default:
		b.WriteString("\n" + dim.Render("[1] reselect A  [2] reselect B  [esc] back"))
	}

	return b.String()
}

func (m *model) renderDiff(b *strings.Builder) string {
	d := m.vaultDiff
	idA := d.RevisionA
	idB := d.RevisionB
	if len(idA) > 12 {
		idA = idA[:12]
	}
	if len(idB) > 12 {
		idB = idB[:12]
	}

	b.WriteString(okStyle.Render(fmt.Sprintf("Diff %s -> %s\n", idA, idB)))
	b.WriteString(strings.Repeat("-", 60) + "\n\n")

	b.WriteString(fmt.Sprintf("  Added:    %d\n", len(d.Added)))
	b.WriteString(fmt.Sprintf("  Removed:  %d\n", len(d.Removed)))
	b.WriteString(fmt.Sprintf("  Changed:  %d\n", len(d.Changed)))
	b.WriteString(fmt.Sprintf("  Unchanged:%d\n\n", d.Unchanged))

	if len(d.Added) > 0 {
		b.WriteString(dim.Render("Added entities:") + "\n")
		for _, e := range d.Added {
			title := e.Title
			if title == "" {
				title = e.ExternalID
			}
			fmt.Fprintf(b, "  + %s (%s)\n", e.ExternalID, shortTitle(title, 50))
		}
		b.WriteString("\n")
	}

	if len(d.Removed) > 0 {
		b.WriteString(dim.Render("Removed entities:") + "\n")
		for _, e := range d.Removed {
			title := e.Title
			if title == "" {
				title = e.ExternalID
			}
			fmt.Fprintf(b, "  - %s (%s)\n", e.ExternalID, shortTitle(title, 50))
		}
		b.WriteString("\n")
	}

	if len(d.Changed) > 0 {
		b.WriteString(dim.Render("Changed entities:") + "\n")
		for _, e := range d.Changed {
			title := e.Title
			if title == "" {
				title = e.ExternalID
			}
			fmt.Fprintf(b, "  ~ %s (%s)\n", e.ExternalID, shortTitle(title, 45))
			for _, f := range e.Changes {
				fmt.Fprintf(b, "    %s: %s → %s\n", f.Field, shortVal(f.OldVal, 30), shortVal(f.NewVal, 30))
			}
		}
		b.WriteString("\n")
	}

	if len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0 && d.Unchanged == 0 {
		b.WriteString("  No differences.\n\n")
	}

	b.WriteString("\n" + dim.Render("[esc] back"))
	return b.String()
}

func shortTitle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func shortVal(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func (m *model) viewVault() string {
	var b strings.Builder
	b.WriteString(sectionHeader.Render("Vault") + "\n\n")

	if !catalog.HasStore() {
		b.WriteString(dim.Render("Vault not initialized."))
		b.WriteString("\nRun: provenance vault init")
		b.WriteString("\nSet: PROVENANCE_DATABASE_URL=postgres://user:pass@localhost/dbname")
		return b.String()
	}

	b.WriteString(okStyle.Render("vault ready") + "\n\n")

	if len(m.vaultCols) == 0 {
		b.WriteString(dim.Render("No archive collections."))
		b.WriteString("\n\nUse archive commands or collect sync --record to populate the vault.")
		return b.String()
	}

	fmt.Fprintf(&b, "  %-24s %s\n", "Collection", "Revisions")
	for i, c := range m.vaultCols {
		cursor := "  "
		if i == m.vaultCur {
			cursor = highlight.Render("▶ ")
		}
		revs, _ := catalog.ListRevisions(c.VaultRoot)
		fmt.Fprintf(&b, "%s%-24s %d\n", cursor, c.Name, len(revs))
	}
	b.WriteString("\n" + dim.Render("[enter] show revisions  [esc] back"))
	return b.String()
}
