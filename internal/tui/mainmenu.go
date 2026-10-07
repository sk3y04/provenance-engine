package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance/internal/archive"
	"github.com/sk3y04/provenance/internal/catalog"
	"github.com/sk3y04/provenance/internal/config"
	"github.com/sk3y04/provenance/internal/dispatcher"
	"github.com/sk3y04/provenance/internal/encode"
	"github.com/sk3y04/provenance/internal/extractor"
	"github.com/sk3y04/provenance/internal/importers"
	"github.com/sk3y04/provenance/internal/manifest"
	"github.com/sk3y04/provenance/internal/session"
)

// ---------------------------------------------------------------------------
// Main menu
// ---------------------------------------------------------------------------

var mainItems = []string{
	"New grab",
	"Scan & pick",
	"Sessions",
	"Watches",
	"Collections",
	"History",
	"Archive search",
	"Vault",
	"Archive",
	"Manifest",
	"Install yt-dlp",
	"Vault init",
	"Encode videos",
	"Quit",
}

func (m *model) updateMain(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.mainCursor > 0 {
			m.mainCursor--
		}
	case "down", "j":
		if m.mainCursor < len(mainItems)-1 {
			m.mainCursor++
		}
	case "R":
		// One-key resume of the most recent unfinished session.
		if m.resumeCandidate != "" {
			return m.startSessionResume(m.resumeCandidate, false)
		}
	case "q":
		return m, tea.Quit
	case "enter":
		switch mainItems[m.mainCursor] {
		case "New grab":
			m.view = viewNewDownload
			m.formStep = 0
			m.form = newDownloadForm()
			m.cfg.applyFormValues(&m.form)
			m.preview = scanPreview{}
			return m, nil
		case "Scan & pick":
			m.view = viewScanPick
			s := scanState{
				checked:      map[int]bool{},
				filter:       filterState{input: newFilterInput()},
				urlInput:     m.scan.urlInput,
				awaitURL:     true,
				advForm:      newAdvOptsForm(),
				showAdvanced: false,
			}
			m.cfg.applyScanValues(&s.advForm)
			m.scan = s
			m.scan.urlInput.SetValue(m.cfg.lastScanURL())
			m.scan.urlInput.Focus()
			return m, nil
		case "Sessions":
			m.view = viewSessions
			return m, m.loadSessionsCmd()
		case "Watches":
			m.view = viewWatches
			return m, m.loadWatchesCmd()
		case "History":
			m.view = viewHistory
			return m, m.loadHistoryCmd()
		case "Collections":
			m.view = viewCollections
			return m, m.loadCollectionsCmd()
		case "Vault":
			m.view = viewVault
			return m, m.loadVaultCmd()
		case "Vault init":
			return m, m.vaultInitCmd()
		case "Archive":
			m.view = viewArchiveMenu
			m.archiveMenuCur = 0
			return m, nil
		case "Manifest":
			m.view = viewManifestShow
			m.manifestPath.SetValue("")
			m.manifestPath.Focus()
			m.manifestResult = nil
			return m, nil
		case "Encode videos":
			m.view = viewEncode
			m.encodeInput.SetValue("")
			m.encodeEncoder.SetValue("auto")
			m.encodeQuality.SetValue("30")
			m.cfg.applyEncodeValues(m)
			m.encodeInput.Focus()
			return m, nil
		case "Archive search":
			m.view = viewArchiveSearch
			m.archiveQuery.SetValue("")
			m.archiveQuery.Focus()
			m.archiveResults = nil
			return m, nil
		case "Install yt-dlp":
			return m, m.installCmd()
		case "Quit":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *model) installCmd() tea.Cmd {
	return func() tea.Msg {
		err := extractor.EnsureInstalled(m.ctx)
		return installDoneMsg{err: err}
	}
}

type installDoneMsg struct {
	err error
}

func (m *model) vaultInitCmd() tea.Cmd {
	return func() tea.Msg {
		dbURL := os.Getenv("PROVENANCE_DATABASE_URL")
		if dbURL == "" {
			return vaultInitDoneMsg{err: fmt.Errorf("PROVENANCE_DATABASE_URL environment variable is not set")}
		}
		store, err := catalog.NewPgStore(m.ctx, dbURL)
		if err != nil {
			return vaultInitDoneMsg{err: fmt.Errorf("connect to PostgreSQL: %w\nhint: set PROVENANCE_DATABASE_URL=postgres://user:pass@localhost/dbname", err)}
		}
		if err := store.Init(m.ctx); err != nil {
			_ = store.Close()
			return vaultInitDoneMsg{err: fmt.Errorf("init schema: %w", err)}
		}
		// Ownership of the store transfers to the global catalog; do not close it here.
		catalog.SetStore(store)
		return vaultInitDoneMsg{err: nil}
	}
}

type vaultInitDoneMsg struct {
	err error
}

func (m *model) startEncode() tea.Cmd {
	dir := strings.TrimSpace(m.encodeInput.Value())
	if dir == "" {
		m.err = "input directory is required"
		return nil
	}
	encoder := strings.TrimSpace(m.encodeEncoder.Value())
	if encoder == "" {
		encoder = "auto"
	}
	quality := 30
	qualityRaw := strings.TrimSpace(m.encodeQuality.Value())
	if v := qualityRaw; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 51 {
			quality = n
		}
	}
	m.encodeRunning = true
	m.encodeResult = nil
	return func() tea.Msg {
		opts := encode.Options{
			Dir:        dir,
			Recursive:  true,
			Ext:        []string{"mp4", "mkv", "mov", "avi", "ts", "webm"},
			Suffix:     "av1",
			Overwrite:  false,
			Encoder:    encoder,
			Quality:    quality,
			Preset:     "medium",
			GOPSeconds: 10,
			Lookahead:  100,
			BFrames:    7,
		}
		res, err := encode.Run(m.ctx, opts, nil)
		m.cfg.setEncodeValues(dir, encoder, qualityRaw)
		if err != nil {
			return encodeDoneMsg{err: err}
		}
		return encodeDoneMsg{result: &encodeResult{
			total:   res.Total,
			skipped: res.Skipped,
			ok:      res.Succeeded,
			failed:  res.Failed,
		}}
	}
}

type encodeDoneMsg struct {
	result *encodeResult
	err    error
}

func (m *model) archiveURLCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	url := strings.TrimSpace(m.archiveURLInput.Value())
	if coll == "" || url == "" {
		m.err = "collection and URL are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveURLFromModel(m, coll, url)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveSessionCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	name := strings.TrimSpace(m.archiveSessionName.Value())
	if coll == "" || name == "" {
		m.err = "collection and session name are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveSessionFromModel(m, coll, name)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	dir := strings.TrimSpace(m.archiveImportDir.Value())
	if coll == "" || dir == "" {
		m.err = "collection and directory are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveImportFromModel(m, coll, dir)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportPDFCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	path := strings.TrimSpace(m.archiveImportPDFPath.Value())
	if coll == "" || path == "" {
		m.err = "collection and PDF path are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveImportPDFFromModel(m, coll, path)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportGitCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	url := strings.TrimSpace(m.archiveImportGitURL.Value())
	ref := strings.TrimSpace(m.archiveImportGitRef.Value())
	if coll == "" || url == "" {
		m.err = "collection and repo URL are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveImportGitFromModel(m, coll, url, ref)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportDocsCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	url := strings.TrimSpace(m.archiveImportDocsURL.Value())
	scope := strings.TrimSpace(m.archiveImportDocsScope.Value())
	maxPages := strings.TrimSpace(m.archiveImportDocsMax.Value())
	if coll == "" || url == "" {
		m.err = "collection and site URL are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		var max int
		if maxPages != "" {
			_, err := fmt.Sscanf(maxPages, "%d", &max)
			_ = err
		}
		revID, entities, err := archiveImportDocsFromModel(m, coll, url, scope, max)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportOpenAPICmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	path := strings.TrimSpace(m.archiveImportOpenAPIPath.Value())
	if coll == "" || path == "" {
		m.err = "collection and spec path are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		revID, entities, err := archiveImportOpenAPIFromModel(m, coll, path)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

func (m *model) archiveImportWebCmd() tea.Cmd {
	coll := strings.TrimSpace(m.archiveCollection.Value())
	url := strings.TrimSpace(m.archiveImportWebURL.Value())
	scope := strings.TrimSpace(m.archiveImportWebScope.Value())
	maxPages := strings.TrimSpace(m.archiveImportWebMax.Value())
	chromePath := strings.TrimSpace(m.archiveImportWebChromePath.Value())
	if coll == "" || url == "" {
		m.err = "collection and URL are required"
		return nil
	}
	m.archiveRunning = true
	m.archiveResult = nil
	return func() tea.Msg {
		var max int
		if maxPages != "" {
			_, err := fmt.Sscanf(maxPages, "%d", &max)
			_ = err
		}
		revID, entities, err := archiveImportWebFromModel(m, coll, url, scope, max, m.archiveImportWebScreenshot, chromePath)
		return archiveDoneMsg{revID: revID, entities: entities, err: err}
	}
}

type archiveDoneMsg struct {
	revID    string
	entities int
	err      error
}

func (m *model) manifestShowCmd() tea.Cmd {
	path := strings.TrimSpace(m.manifestPath.Value())
	if path == "" {
		m.err = "manifest path is required"
		return nil
	}
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		if err != nil {
			return manifestDoneMsg{err: fmt.Errorf("read manifest: %w", err)}
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return manifestDoneMsg{pretty: string(data)}
		}
		return manifestDoneMsg{pretty: pretty.String()}
	}
}

func (m *model) manifestVerifyCmd() tea.Cmd {
	dir := strings.TrimSpace(m.manifestDir.Value())
	if dir == "" {
		m.err = "directory is required"
		return nil
	}
	return func() tea.Msg {
		results, err := manifest.VerifyCaptureDir(dir)
		if err != nil {
			return manifestDoneMsg{err: err}
		}
		if len(results) == 0 {
			return manifestDoneMsg{err: fmt.Errorf("no capture items found in %s", dir)}
		}
		ok, fail, missing := 0, 0, 0
		for _, r := range results {
			if r.Missing {
				missing++
			} else if r.OK {
				ok++
			} else {
				fail++
			}
		}
		return manifestDoneMsg{ok: ok, failed: fail, missing: missing}
	}
}

type manifestDoneMsg struct {
	pretty  string
	ok      int
	failed  int
	missing int
	err     error
}

func archiveURLFromModel(m *model, coll string, rawURL string) (string, int, error) {
	opts := dispatcher.Options{Config: config.Config{OutputDir: "./downloads"}}
	mf, err := dispatcher.Scan(m.ctx, rawURL, opts)
	if err != nil {
		return "", 0, fmt.Errorf("scan: %w", err)
	}

	sessName := fmt.Sprintf("archive-url-%d", time.Now().Unix())
	sess, err := session.OpenOrCreate(sessName, opts.Config)
	if err != nil {
		return "", 0, fmt.Errorf("create session: %w", err)
	}
	opts.Reporter = sess

	var urls []string
	for _, item := range mf.Items {
		if item.URL != "" {
			urls = append(urls, item.URL)
		}
	}
	for _, u := range urls {
		if err := dispatcher.Dispatch(m.ctx, u, opts); err != nil {
			return "", 0, fmt.Errorf("download: %w", err)
		}
	}

	ingestOpts := archive.IngestOptions{
		VaultRoot:      "./provenance-vault",
		CollectionName: coll,
		Source: archive.Source{
			URL:       rawURL,
			Kind:      archive.SourceURL,
			Reference: rawURL,
		},
		Tool: "provenance-tui",
	}
	return archive.IngestFromOutput(m.ctx, "./downloads", ingestOpts)
}

func archiveSessionFromModel(m *model, coll string, name string) (string, int, error) {
	s, err := session.Load(name)
	if err != nil {
		return "", 0, err
	}

	ingestOpts := archive.IngestOptions{
		VaultRoot:      "./provenance-vault",
		CollectionName: coll,
		Source: archive.Source{
			URL:       name,
			Kind:      archive.SourceSession,
			Reference: name,
		},
		Tool: "provenance-tui",
	}
	return archive.IngestFromOutput(m.ctx, s.Options.OutputDir, ingestOpts)
}

func archiveImportFromModel(m *model, coll string, dir string) (string, int, error) {
	ingestOpts := archive.IngestOptions{
		VaultRoot:      "./provenance-vault",
		CollectionName: coll,
		Source: archive.Source{
			URL:       dir,
			Kind:      archive.SourceImport,
			Reference: dir,
		},
		Tool: "provenance-tui",
	}
	return archive.IngestFromOutput(m.ctx, dir, ingestOpts)
}

func archiveImportPDFFromModel(m *model, coll, pdfPath string) (string, int, error) {
	rev, err := importers.ImportPDF("./provenance-vault", pdfPath, coll)
	if err != nil {
		return "", 0, err
	}
	rev.Source = archive.Source{URL: pdfPath, Kind: archive.SourceImport, Reference: coll}
	rev.Tool = "provenance-tui"
	if err := archive.WriteRevision("./provenance-vault", rev); err != nil {
		return "", 0, err
	}
	return rev.ID, len(rev.Entities), nil
}

func archiveImportGitFromModel(m *model, coll, repoURL, ref string) (string, int, error) {
	rev, err := importers.ImportGit("./provenance-vault", repoURL, ref, coll)
	if err != nil {
		return "", 0, err
	}
	rev.Source = archive.Source{URL: repoURL, Kind: archive.SourceImport, Reference: coll}
	rev.Tool = "provenance-tui"
	if err := archive.WriteRevision("./provenance-vault", rev); err != nil {
		return "", 0, err
	}
	return rev.ID, len(rev.Entities), nil
}

func archiveImportDocsFromModel(m *model, coll, siteURL, scope string, maxPages int) (string, int, error) {
	rev, err := importers.ImportDocs("./provenance-vault", siteURL, scope, maxPages, coll)
	if err != nil {
		return "", 0, err
	}
	rev.Source = archive.Source{URL: siteURL, Kind: archive.SourceImport, Reference: coll}
	rev.Tool = "provenance-tui"
	if err := archive.WriteRevision("./provenance-vault", rev); err != nil {
		return "", 0, err
	}
	return rev.ID, len(rev.Entities), nil
}

func archiveImportOpenAPIFromModel(m *model, coll, specPath string) (string, int, error) {
	rev, err := importers.ImportOpenAPI("./provenance-vault", specPath, coll)
	if err != nil {
		return "", 0, err
	}
	rev.Source = archive.Source{URL: specPath, Kind: archive.SourceImport, Reference: coll}
	rev.Tool = "provenance-tui"
	if err := archive.WriteRevision("./provenance-vault", rev); err != nil {
		return "", 0, err
	}
	return rev.ID, len(rev.Entities), nil
}

func archiveImportWebFromModel(m *model, coll, url, scope string, maxPages int, screenshot bool, chromePath string) (string, int, error) {
	rev, err := importers.ImportWeb("./provenance-vault", url, scope, maxPages, screenshot, coll, chromePath)
	if err != nil {
		return "", 0, err
	}
	rev.Source = archive.Source{URL: url, Kind: archive.SourceImport, Reference: coll}
	rev.Tool = "provenance-tui"
	if err := archive.WriteRevision("./provenance-vault", rev); err != nil {
		return "", 0, err
	}
	return rev.ID, len(rev.Entities), nil
}

func (m *model) refreshResumeCandidate() {
	m.resumeCandidate = ""
	m.resumePendingURL = 0
	for _, info := range m.sessions { // already sorted newest first
		if info.Counts.Pending+info.Counts.Failed+info.Counts.Running > 0 {
			m.resumeCandidate = info.Name
			m.resumePendingURL = info.Counts.Pending + info.Counts.Failed + info.Counts.Running
			return
		}
	}
}
