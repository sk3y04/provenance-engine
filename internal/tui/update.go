package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance/internal/catalog"
	"github.com/sk3y04/provenance/internal/diagnose"
	"github.com/sk3y04/provenance/internal/watch"
)

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.view == viewRunner && !m.runner.done && m.runner.cancel != nil {
				m.runner.cancel()
				m.appendLog("[provenance] cancel requested...")
				return m, nil
			}
			return m, tea.Quit
		case "Q":
			return m, tea.Quit
		}
		return m.updateView(msg)

	case tea.MouseEvent:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			return m.handleMouseClick(msg.Y)
		}

	case sessionsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.sessions = msg.infos
			if m.sessCursor >= len(m.visibleSessions()) {
				m.sessCursor = 0
			}
			m.refreshResumeCandidate()
		}
		return m, nil

	case sessionLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.sessSelected = msg.s
		m.view = viewSessionDetail
		return m, nil

	case watchesLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.watches = msg.subs
			if m.watchesCur >= len(m.visibleWatches()) {
				m.watchesCur = 0
			}
		}
		return m, nil

	case historyLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.history = msg.runs
			if m.historyCur >= len(m.visibleHistory()) {
				m.historyCur = 0
			}
		}
		return m, nil

	case scanPreviewTick:
		// Debounce: only run if the URL field still matches.
		if strings.TrimSpace(m.form.url.Value()) != msg.url || msg.url == "" {
			return m, nil
		}
		if m.preview.loading && m.preview.url == msg.url {
			return m, nil
		}
		m.preview = scanPreview{url: msg.url, loading: true}
		return m, m.scanPreviewCmd(msg.url)

	case scanPreviewMsg:
		if msg.canceled || strings.TrimSpace(m.form.url.Value()) != msg.url {
			return m, nil
		}
		m.preview.loading = false
		m.preview.url = msg.url
		m.preview.count = msg.count
		m.preview.size = msg.size
		m.preview.site = msg.site
		if msg.err != nil {
			m.preview.err = msg.err.Error()
		} else {
			m.preview.err = ""
		}
		return m, nil

	case scanLoadedMsg:
		m.scan.loading = false
		m.scan.sourceURL = msg.sourceURL
		if msg.err != nil {
			m.scan.err = msg.err.Error()
			return m, nil
		}
		m.scan.err = ""
		m.scan.items = msg.manifest.Items
		m.scan.site = msg.manifest.Site
		m.scan.checked = make(map[int]bool, len(m.scan.items))
		for i := range m.scan.items {
			m.scan.checked[i] = true
		}
		m.scan.cursor = 0
		return m, nil

	case runnerEventMsg:
		switch msg.kind {
		case "queue":
			m.runner.queued++
		case "start":
			m.runner.running++
			m.appendLog(fmt.Sprintf("▶ %s", trim(msg.url, 100)))
		case "ok":
			if m.runner.running > 0 {
				m.runner.running--
			}
			m.runner.ok++
			m.appendLog(fmt.Sprintf("✓ %s", trim(msg.url, 100)))
		case "fail":
			if m.runner.running > 0 {
				m.runner.running--
			}
			m.runner.failed++
			line := fmt.Sprintf("✗ %s", trim(msg.url, 100))
			if msg.note != "" {
				line += " - " + trim(msg.note, 120)
			}
			m.appendLog(line)
		case "skip":
			m.runner.skipped++
			m.appendLog(fmt.Sprintf("⤼ skip %s (%s)", trim(msg.url, 80), msg.note))
		}
		return m, nil

	case runnerLogMsg:
		m.appendLog(msg.line)
		return m, nil

	case fileStartMsg:
		if m.runner.files == nil {
			m.runner.files = map[string]*fileProgress{}
		}
		if _, exists := m.runner.files[msg.url]; !exists {
			m.runner.fileOrder = append(m.runner.fileOrder, msg.url)
		}
		m.runner.files[msg.url] = &fileProgress{
			url:       msg.url,
			dest:      msg.dest,
			total:     msg.total,
			startedAt: time.Now(),
		}
		return m, nil

	case fileProgressMsg:
		if fp, ok := m.runner.files[msg.url]; ok {
			fp.written = msg.written
			if msg.total > 0 {
				fp.total = msg.total
			}
		}
		return m, nil

	case fileDoneMsg:
		if fp, ok := m.runner.files[msg.url]; ok {
			fp.done = true
			fp.doneAt = time.Now()
			fp.err = msg.err
			if msg.err == nil && fp.total > 0 {
				fp.written = fp.total
			}
		}
		return m, nil

	case cookiesFoundMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.cookiePick.files = msg.files
		m.cookiePick.cursor = 0
		m.cookiePick.active = true
		return m, nil

	case runnerDoneMsg:
		duration := time.Since(m.runner.startAt)
		m.runner.done = true
		m.runner.err = msg.err
		if msg.err != nil {
			m.appendLog("[provenance] FAILED: " + msg.err.Error())
			if hint := diagnose.Hint(msg.err); hint != "" {
				m.appendLog("[provenance] hint: " + hint)
			}
		} else {
			m.appendLog("[provenance] finished")
		}
		if !m.runner.notified {
			m.runner.notified = true
			go notifyComplete(m.runner)
		}
		if err := m.saveHistoryRun(); err != nil {
			m.appendLog("[provenance] WARNING: could not save history: " + err.Error())
		}
		var status string
		if msg.err != nil {
			status = "failed"
		} else {
			status = "success"
		}
		if m.runner.watchName != "" {
			_ = watch.MarkRunWithStatus(m.runner.watchName, duration, status)
		}
		return m, nil

	case tickMsg:
		if m.view == viewRunner && !m.runner.done {
			m.sampleThroughput()
			m.pruneFinishedFiles(20)
			return m, tickEvery(time.Second)
		}
		return m, nil

	case archiveSearchMsg:
		if !catalog.HasStore() {
			m.err = "Vault not initialized — run 'provenance vault init' first"
			return m, nil
		}
		m.archiveLoading = true
		return m, func() tea.Msg {
			result, err := catalog.Store().Search(context.Background(), msg.query, catalog.SearchOptions{Limit: 20})
			return archiveResultsMsg{query: msg.query, result: result, err: err}
		}

	case archiveResultsMsg:
		m.archiveLoading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.archiveResults = make([]archiveSearchHit, 0)
		if msg.result != nil {
			for _, h := range msg.result.Hits {
				m.archiveResults = append(m.archiveResults, archiveSearchHit{
					Title:      h.Title,
					Headline:   h.Headline,
					URL:        h.URL,
					Collection: h.CollectionName,
					Revision:   h.RevisionID,
					Date:       h.CapturedAt.Format("2006-01-02"),
				})
			}
		}
		m.archiveCur = 0
		return m, nil

	case collectionsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.collections = msg.cols
		return m, nil

	case collectionSyncedMsg:
		m.info = fmt.Sprintf("Synced %s — %d new, %d skipped", msg.name, msg.result.New, msg.result.Skipped)
		return m, m.loadCollectionsCmd()

	case archiveCollectionMsg:
		m.info = fmt.Sprintf("Archiving collection %s... (see provenance archive collection %s)", msg.name, msg.name)
		return m, nil

	case collectionRevsMsg:
		if msg.err == nil {
			m.collPending.revs = len(msg.revIDs)
			if m.collPending.name == msg.name {
				if len(msg.revIDs) > 0 {
					m.info = fmt.Sprintf("press d again to remove %q (+ %d vault revision(s))", msg.name, len(msg.revIDs))
				} else {
					m.info = fmt.Sprintf("press d again within %ds to remove %q", int(confirmWindow.Seconds()), msg.name)
				}
			}
		}
		return m, nil

	case vaultLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.vaultCols = msg.cols
		return m, nil

	case vaultRevisionsMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.vaultRevisions = msg.revisions
		m.vaultRevCur = 0
		return m, nil

	case vaultDiffMsg:
		m.vaultDiffLoading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.vaultDiff = msg.result
		m.vaultRevPhase = 2
		return m, nil

	case installDoneMsg:
		if msg.err != nil {
			m.err = "install failed: " + msg.err.Error()
		} else {
			m.ytDlpInstalled = true
			m.info = "yt-dlp and ffmpeg installed successfully"
		}
		return m, nil

	case vaultInitDoneMsg:
		if msg.err != nil {
			m.err = "vault init failed: " + msg.err.Error()
		} else {
			m.vaultInit = true
			m.info = "vault initialized successfully"
			m.view = viewVault
			return m, m.loadVaultCmd()
		}
		return m, nil

	case encodeDoneMsg:
		m.encodeRunning = false
		if msg.err != nil {
			m.encodeResult = &encodeResult{err: msg.err}
		} else {
			m.encodeResult = msg.result
		}
		return m, nil
	}

	return m, nil
}

type archiveResultsMsg struct {
	query  string
	result *catalog.SearchResult
	err    error
}

type archiveSearchMsg struct {
	query string
}

func (m *model) updateView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.view {
	case viewMain:
		return m.updateMain(msg)
	case viewSessions:
		return m.updateSessions(msg)
	case viewSessionDetail:
		return m.updateSessionDetail(msg)
	case viewWatches:
		return m.updateWatches(msg)
	case viewHistory:
		return m.updateHistory(msg)
	case viewNewDownload:
		return m.updateNewDownload(msg)
	case viewScanPick:
		return m.updateScanPick(msg)
	case viewRunner:
		return m.updateRunner(msg)
	case viewArchiveSearch:
		return m.updateArchiveSearch(msg)
	case viewCollections:
		return m.updateCollections(msg)
	case viewCollectionDetail:
		return m.updateCollectionDetail(msg)
	case viewVault:
		return m.updateVault(msg)
	case viewVaultRevisions:
		return m.updateVaultRevisions(msg)
	case viewEncode:
		return m.updateEncode(msg)
	case viewArchiveMenu:
		return m.updateArchiveMenu(msg)
	case viewArchiveURL:
		return m.updateArchiveURL(msg)
	case viewArchiveSession:
		return m.updateArchiveSession(msg)
	case viewArchiveImport:
		return m.updateArchiveImport(msg)
	case viewArchiveImportPDF:
		return m.updateArchiveImportPDF(msg)
	case viewArchiveImportGit:
		return m.updateArchiveImportGit(msg)
	case viewArchiveImportDocs:
		return m.updateArchiveImportDocs(msg)
	case viewArchiveImportOpenAPI:
		return m.updateArchiveImportOpenAPI(msg)
	case viewArchiveImportWeb:
		return m.updateArchiveImportWeb(msg)
	case viewManifestShow:
		return m.updateManifestShow(msg)
	case viewManifestVerify:
		return m.updateManifestVerify(msg)
	}
	return m, nil
}

func (m *model) updateArchiveSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.view = viewMain
		m.archiveQuery.Blur()
		m.archiveResults = nil
		return m, nil
	case "up", "k":
		if m.archiveSearchHistoryIdx > 0 {
			m.archiveSearchHistoryIdx--
			m.archiveQuery.SetValue(m.archiveSearchHistory[m.archiveSearchHistoryIdx])
			return m, nil
		}
		if m.archiveCur > 0 {
			m.archiveCur--
		}
	case "down", "j":
		if m.archiveSearchHistoryIdx < len(m.archiveSearchHistory)-1 {
			m.archiveSearchHistoryIdx++
			m.archiveQuery.SetValue(m.archiveSearchHistory[m.archiveSearchHistoryIdx])
			return m, nil
		}
		if m.archiveResults != nil && m.archiveCur < len(m.archiveResults)-1 {
			m.archiveCur++
		}
	case "enter":
		if !m.archiveLoading {
			query := strings.TrimSpace(m.archiveQuery.Value())
			if query != "" {
				m.addToSearchHistory(query)
			}
			return m, m.performArchiveSearch()
		}
	default:
		m.archiveSearchHistoryIdx = -1
		var cmd tea.Cmd
		m.archiveQuery, cmd = m.archiveQuery.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) addToSearchHistory(query string) {
	// Remove duplicate if it exists
	for i, h := range m.archiveSearchHistory {
		if h == query {
			m.archiveSearchHistory = append(m.archiveSearchHistory[:i], m.archiveSearchHistory[i+1:]...)
			break
		}
	}
	// Add to front
	m.archiveSearchHistory = append([]string{query}, m.archiveSearchHistory...)
	// Limit to 20
	if len(m.archiveSearchHistory) > 20 {
		m.archiveSearchHistory = m.archiveSearchHistory[:20]
	}
	m.archiveSearchHistoryIdx = -1
}

func (m *model) performArchiveSearch() tea.Cmd {
	return func() tea.Msg {
		return archiveSearchMsg{query: m.archiveQuery.Value()}
	}
}

func (m *model) updateEncode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.encodeRunning {
		return m, nil
	}

	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewMain
		m.encodeResult = nil
		return m, nil
	case "tab", "down":
		if m.encodeInput.Focused() {
			m.encodeInput.Blur()
			m.encodeEncoder.Focus()
		} else if m.encodeEncoder.Focused() {
			m.encodeEncoder.Blur()
			m.encodeQuality.Focus()
		} else {
			m.encodeQuality.Blur()
			m.encodeInput.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.encodeInput.Focused() {
			m.encodeInput.Blur()
			m.encodeQuality.Focus()
		} else if m.encodeEncoder.Focused() {
			m.encodeEncoder.Blur()
			m.encodeInput.Focus()
		} else {
			m.encodeQuality.Blur()
			m.encodeEncoder.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.startEncode()
	case "enter":
		if m.encodeInput.Focused() {
			m.encodeInput.Blur()
			m.encodeEncoder.Focus()
		} else if m.encodeEncoder.Focused() {
			m.encodeEncoder.Blur()
			m.encodeQuality.Focus()
		} else {
			return m, m.startEncode()
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch {
	case m.encodeInput.Focused():
		m.encodeInput, cmd = m.encodeInput.Update(msg)
	case m.encodeEncoder.Focused():
		m.encodeEncoder, cmd = m.encodeEncoder.Update(msg)
	case m.encodeQuality.Focused():
		m.encodeQuality, cmd = m.encodeQuality.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewMain
		return m, nil
	case "up", "k":
		if m.archiveMenuCur > 0 {
			m.archiveMenuCur--
		}
	case "down", "j":
		if m.archiveMenuCur < 7 {
			m.archiveMenuCur++
		}
	case "enter":
		switch m.archiveMenuCur {
		case 0:
			m.view = viewArchiveURL
			m.archiveCollection.SetValue("")
			m.archiveURLInput.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 1:
			m.view = viewArchiveSession
			m.archiveCollection.SetValue("")
			m.archiveSessionName.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 2:
			m.view = viewArchiveImport
			m.archiveCollection.SetValue("")
			m.archiveImportDir.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 3:
			m.view = viewArchiveImportPDF
			m.archiveCollection.SetValue("")
			m.archiveImportPDFPath.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 4:
			m.view = viewArchiveImportGit
			m.archiveCollection.SetValue("")
			m.archiveImportGitURL.SetValue("")
			m.archiveImportGitRef.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 5:
			m.view = viewArchiveImportDocs
			m.archiveCollection.SetValue("")
			m.archiveImportDocsURL.SetValue("")
			m.archiveImportDocsScope.SetValue("")
			m.archiveImportDocsMax.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 6:
			m.view = viewArchiveImportOpenAPI
			m.archiveCollection.SetValue("")
			m.archiveImportOpenAPIPath.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		case 7:
			m.view = viewArchiveImportWeb
			m.archiveCollection.SetValue("")
			m.archiveImportWebURL.SetValue("")
			m.archiveImportWebScope.SetValue("")
			m.archiveImportWebMax.SetValue("")
			m.archiveImportWebScreenshot = false
			m.archiveImportWebChromePath.SetValue("")
			m.archiveResult = nil
			m.archiveCollection.Focus()
		}
		return m, nil
	}
	return m, nil
}

func (m *model) updateArchiveURL(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}

	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveURLInput.Focus()
		} else {
			m.archiveURLInput.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveURLInput.Focus()
		} else {
			m.archiveURLInput.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveURLCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveURLInput.Focus()
		} else {
			return m, m.archiveURLCmd()
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else {
		m.archiveURLInput, cmd = m.archiveURLInput.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}

	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveSessionName.Focus()
		} else {
			m.archiveSessionName.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveSessionName.Focus()
		} else {
			m.archiveSessionName.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveSessionCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveSessionName.Focus()
		} else {
			return m, m.archiveSessionCmd()
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else {
		m.archiveSessionName, cmd = m.archiveSessionName.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImport(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}

	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDir.Focus()
		} else {
			m.archiveImportDir.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDir.Focus()
		} else {
			m.archiveImportDir.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveImportCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDir.Focus()
		} else {
			return m, m.archiveImportCmd()
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else {
		m.archiveImportDir, cmd = m.archiveImportDir.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImportPDF(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportPDFPath.Focus()
		} else {
			m.archiveImportPDFPath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportPDFPath.Focus()
		} else {
			m.archiveImportPDFPath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveImportPDFCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportPDFPath.Focus()
		} else {
			return m, m.archiveImportPDFCmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else {
		m.archiveImportPDFPath, cmd = m.archiveImportPDFPath.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImportGit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportGitURL.Focus()
		} else if m.archiveImportGitURL.Focused() {
			m.archiveImportGitURL.Blur()
			m.archiveImportGitRef.Focus()
		} else {
			m.archiveImportGitRef.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportGitURL.Focus()
		} else if m.archiveImportGitURL.Focused() {
			m.archiveImportGitURL.Blur()
			m.archiveImportGitRef.Focus()
		} else {
			m.archiveImportGitRef.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveImportGitCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportGitURL.Focus()
		} else if m.archiveImportGitURL.Focused() {
			m.archiveImportGitURL.Blur()
			m.archiveImportGitRef.Focus()
		} else {
			return m, m.archiveImportGitCmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else if m.archiveImportGitURL.Focused() {
		m.archiveImportGitURL, cmd = m.archiveImportGitURL.Update(msg)
	} else {
		m.archiveImportGitRef, cmd = m.archiveImportGitRef.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImportDocs(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDocsURL.Focus()
		} else if m.archiveImportDocsURL.Focused() {
			m.archiveImportDocsURL.Blur()
			m.archiveImportDocsScope.Focus()
		} else if m.archiveImportDocsScope.Focused() {
			m.archiveImportDocsScope.Blur()
			m.archiveImportDocsMax.Focus()
		} else {
			m.archiveImportDocsMax.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDocsURL.Focus()
		} else if m.archiveImportDocsURL.Focused() {
			m.archiveImportDocsURL.Blur()
			m.archiveImportDocsScope.Focus()
		} else if m.archiveImportDocsScope.Focused() {
			m.archiveImportDocsScope.Blur()
			m.archiveImportDocsMax.Focus()
		} else {
			m.archiveImportDocsMax.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveImportDocsCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportDocsURL.Focus()
		} else if m.archiveImportDocsURL.Focused() {
			m.archiveImportDocsURL.Blur()
			m.archiveImportDocsScope.Focus()
		} else if m.archiveImportDocsScope.Focused() {
			m.archiveImportDocsScope.Blur()
			m.archiveImportDocsMax.Focus()
		} else {
			return m, m.archiveImportDocsCmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else if m.archiveImportDocsURL.Focused() {
		m.archiveImportDocsURL, cmd = m.archiveImportDocsURL.Update(msg)
	} else if m.archiveImportDocsScope.Focused() {
		m.archiveImportDocsScope, cmd = m.archiveImportDocsScope.Update(msg)
	} else {
		m.archiveImportDocsMax, cmd = m.archiveImportDocsMax.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImportOpenAPI(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportOpenAPIPath.Focus()
		} else {
			m.archiveImportOpenAPIPath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportOpenAPIPath.Focus()
		} else {
			m.archiveImportOpenAPIPath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "ctrl+s":
		return m, m.archiveImportOpenAPICmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportOpenAPIPath.Focus()
		} else {
			return m, m.archiveImportOpenAPICmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else {
		m.archiveImportOpenAPIPath, cmd = m.archiveImportOpenAPIPath.Update(msg)
	}
	return m, cmd
}

func (m *model) updateArchiveImportWeb(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveRunning {
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewArchiveMenu
		m.archiveResult = nil
		return m, nil
	case "tab", "down":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportWebURL.Focus()
		} else if m.archiveImportWebURL.Focused() {
			m.archiveImportWebURL.Blur()
			m.archiveImportWebScope.Focus()
		} else if m.archiveImportWebScope.Focused() {
			m.archiveImportWebScope.Blur()
			m.archiveImportWebMax.Focus()
		} else if m.archiveImportWebMax.Focused() {
			m.archiveImportWebMax.Blur()
			// Screenshot toggle is not a text input, handled separately
			return m, nil
		} else {
			m.archiveImportWebChromePath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportWebURL.Focus()
		} else if m.archiveImportWebURL.Focused() {
			m.archiveImportWebURL.Blur()
			m.archiveImportWebScope.Focus()
		} else if m.archiveImportDocsScope.Focused() {
			m.archiveImportWebScope.Blur()
			m.archiveImportWebMax.Focus()
		} else if m.archiveImportWebMax.Focused() {
			m.archiveImportWebMax.Blur()
			m.archiveImportWebChromePath.Focus()
		} else {
			m.archiveImportWebChromePath.Blur()
			m.archiveCollection.Focus()
		}
		return m, nil
	case "space":
		if m.archiveImportWebMax.Focused() || m.archiveImportWebChromePath.Focused() {
			m.archiveImportWebScreenshot = !m.archiveImportWebScreenshot
			return m, nil
		}
	case "ctrl+s":
		return m, m.archiveImportWebCmd()
	case "enter":
		if m.archiveCollection.Focused() {
			m.archiveCollection.Blur()
			m.archiveImportWebURL.Focus()
		} else if m.archiveImportWebURL.Focused() {
			m.archiveImportWebURL.Blur()
			m.archiveImportWebScope.Focus()
		} else if m.archiveImportWebScope.Focused() {
			m.archiveImportWebScope.Blur()
			m.archiveImportWebMax.Focus()
		} else if m.archiveImportWebMax.Focused() {
			m.archiveImportWebMax.Blur()
			m.archiveImportWebChromePath.Focus()
		} else {
			return m, m.archiveImportWebCmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.archiveCollection.Focused() {
		m.archiveCollection, cmd = m.archiveCollection.Update(msg)
	} else if m.archiveImportWebURL.Focused() {
		m.archiveImportWebURL, cmd = m.archiveImportWebURL.Update(msg)
	} else if m.archiveImportWebScope.Focused() {
		m.archiveImportWebScope, cmd = m.archiveImportWebScope.Update(msg)
	} else if m.archiveImportWebMax.Focused() {
		m.archiveImportWebMax, cmd = m.archiveImportWebMax.Update(msg)
	} else {
		m.archiveImportWebChromePath, cmd = m.archiveImportWebChromePath.Update(msg)
	}
	return m, cmd
}

func (m *model) updateManifestShow(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "h":
		m.view = viewMain
		m.manifestResult = nil
		return m, nil
	case "up", "k":
		if m.manifestView > 0 {
			m.manifestView--
		}
	case "down", "j":
		if m.manifestView < 1 {
			m.manifestView++
		}
	case "enter":
		if m.manifestView == 0 {
			return m, m.manifestShowCmd()
		}
		return m, m.manifestVerifyCmd()
	}

	var cmd tea.Cmd
	if m.manifestView == 0 {
		m.manifestPath, cmd = m.manifestPath.Update(msg)
	} else {
		m.manifestDir, cmd = m.manifestDir.Update(msg)
	}
	return m, cmd
}

func (m *model) updateManifestVerify(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.updateManifestShow(msg)
}

// handleMouseClick updates the cursor position for list views when the user clicks on a list item.
// The Y coordinate from the mouse event maps to list rows (0-indexed from the top of the terminal).
// Most list views have their items starting at row 3 or 4 (header at row 0, blank lines at 1-2).
func (m *model) handleMouseClick(row int) (tea.Model, tea.Cmd) {
	listStart := 3 // header + 2 blank lines (typical for list views)
	itemIdx := row - listStart
	if itemIdx < 0 {
		return m, nil
	}

	switch m.view {
	case viewMain:
		if itemIdx >= 0 && itemIdx < len(mainItems) {
			m.mainCursor = itemIdx
		}
	case viewSessions:
		if m.sessCursor >= 0 && itemIdx >= 0 && itemIdx < len(m.visibleSessions()) {
			m.sessCursor = itemIdx
		}
	case viewWatches:
		if itemIdx >= 0 && itemIdx < len(m.visibleWatches()) {
			m.watchesCur = itemIdx
		}
	case viewHistory:
		if itemIdx >= 0 && itemIdx < len(m.visibleHistory()) {
			m.historyCur = itemIdx
		}
	case viewCollections:
		if itemIdx >= 0 && itemIdx < len(m.visibleCollections()) {
			m.collCur = itemIdx
		}
	case viewVault:
		if itemIdx >= 0 && itemIdx < len(m.vaultCols) {
			m.vaultCur = itemIdx
		}
	case viewArchiveMenu:
		if itemIdx >= 0 && itemIdx < 8 {
			m.archiveMenuCur = itemIdx
		}
	case viewNewDownload, viewScanPick:
		// For forms, clicking moves to the corresponding step
		if itemIdx >= 0 && itemIdx <= 5 {
			if m.view == viewNewDownload {
				m.formStep = itemIdx
			} else {
				m.scan.cursor = itemIdx
			}
		}
	case viewEncode:
		switch itemIdx {
		case 0:
			m.encodeInput.Focus()
		case 1:
			m.encodeEncoder.Focus()
		case 2:
			m.encodeQuality.Focus()
		}
	}
	return m, nil
}
