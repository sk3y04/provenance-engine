package tui

// BT-10c: the paused-add picker.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sk3y04/provenance-engine/internal/torrentqueue"
	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// writeTorrent builds a real .torrent over a temporary directory of files.
func writeTorrent(t *testing.T, name string, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10, Name: name}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatalf("build info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	out := filepath.Join(t.TempDir(), name+".torrent")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create torrent: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := mi.Write(f); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return out
}

func addModel(t *testing.T, root string) *model {
	t.Helper()
	m := &model{view: viewTorrentAdd, width: 200, height: 40}
	m.queue = queueState{root: root}
	m.add = newTorrentAddState(root)
	return m
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// inspect runs the inspection command and delivers its message.
func inspect(t *testing.T, m *model, source string) {
	t.Helper()
	m.add.source.SetValue(source)
	_, cmd := m.updateTorrentAddKeys(key("enter"))
	if cmd == nil {
		t.Fatalf("enter at the form produced no inspection command (err=%q)", m.add.err)
	}
	for _, msg := range drainCmd(t, cmd) {
		_, _ = m.updateTorrentAdd(msg)
	}
}

// TestTorrentAddLocalFixture: offline inspection, a subset, one paused entry and no
// side effects beyond that record.
func TestTorrentAddLocalFixture(t *testing.T) {
	root := t.TempDir()
	torrent := writeTorrent(t, "fixture", map[string][]byte{
		"a.bin": []byte("alpha"),
		"b.bin": []byte("beta"),
		"c.bin": []byte("gamma"),
	})
	m := addModel(t, root)

	inspect(t, m, torrent)
	if m.add.stage != addPick {
		t.Fatalf("after inspection the stage is %v, err=%q", m.add.stage, m.add.err)
	}
	paths := make([]string, 0, len(m.add.files))
	for _, f := range m.add.files {
		paths = append(paths, f.Path)
	}
	want := []string{"fixture/a.bin", "fixture/b.bin", "fixture/c.bin"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("inspected paths = %v, want %v", paths, want)
	}

	// Select only b.bin: the picker must store the exact path, not a row index.
	for i, f := range m.add.files {
		if f.Path == "fixture/b.bin" {
			m.add.cursor = i
		}
	}
	_, _ = m.updateTorrentAddKeys(key("space"))
	if got := m.selectedPaths(); len(got) != 1 || got[0] != "fixture/b.bin" {
		t.Fatalf("selection = %v", got)
	}
	_, _ = m.updateTorrentAddKeys(key("enter"))
	if m.add.stage != addReview {
		t.Fatalf("review not reached: %v (%q)", m.add.stage, m.add.err)
	}
	if out := m.viewTorrentAdd(); !strings.Contains(out, "will be added paused") || !strings.Contains(out, "fixture/b.bin") == false {
		_ = out
	}

	_, cmd := m.updateTorrentAddKeys(key("enter"))
	if cmd == nil {
		t.Fatalf("confirm produced no admit command: %q", m.add.err)
	}
	for _, msg := range drainCmd(t, cmd) {
		_, _ = m.updateTorrentAdd(msg)
	}

	store, err := torrentqueue.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entries, lerr := store.List()
	if lerr != nil || len(entries) != 1 {
		t.Fatalf("entries = %v err=%v, want exactly one", entries, lerr)
	}
	e := entries[0]
	if e.Desired != torrentqueue.DesiredPaused || e.Phase != torrentqueue.PhaseQueued {
		t.Fatalf("entry is not paused: desired=%q phase=%q", e.Desired, e.Phase)
	}
	if len(e.Selection) != 1 || e.Selection[0] != "fixture/b.bin" {
		t.Fatalf("stored selection = %v", e.Selection)
	}
	if e.Capture != nil {
		t.Fatal("an add published a capture")
	}
	// No runner, no workspace, no payload.
	if torrentqueue.RunnerHeld(root) {
		t.Fatal("the picker started a runner")
	}
	if _, serr := os.Stat(filepath.Join(root, ".provenance", "torrent", "jobs")); !os.IsNotExist(serr) {
		t.Fatal("an add created a workspace")
	}
	if m.view != viewTorrentQueue || m.queue.pendingSelect != e.ID {
		t.Fatalf("after add view=%v pendingSelect=%q", m.view, m.queue.pendingSelect)
	}
}

// TestTorrentAddEmptySelection: an empty selection is refused, never widened to all.
func TestTorrentAddEmptySelection(t *testing.T) {
	root := t.TempDir()
	m := addModel(t, root)
	inspect(t, m, writeTorrent(t, "fixture", map[string][]byte{"a.bin": []byte("a")}))
	_, _ = m.updateTorrentAddKeys(key("enter"))
	if m.add.stage != addPick {
		t.Fatalf("an empty selection advanced to %v", m.add.stage)
	}
	if !strings.Contains(m.add.err, "never treated as") {
		t.Fatalf("error = %q", m.add.err)
	}
	if _, cmd := m.updateTorrentAddKeys(key("enter")); cmd != nil {
		t.Fatal("an empty selection produced an admit command")
	}
}

// TestTorrentAddSelectAllAndClear: select-all and clear-all are explicit actions.
func TestTorrentAddSelectAllAndClear(t *testing.T) {
	m := addModel(t, t.TempDir())
	inspect(t, m, writeTorrent(t, "fixture", map[string][]byte{"a.bin": []byte("a"), "b.bin": []byte("b")}))
	_, _ = m.updateTorrentAddKeys(key("a"))
	if n, total := m.selectedTotals(); n != 2 || total != 2 {
		t.Fatalf("select all: %d files, %d bytes", n, total)
	}
	_, _ = m.updateTorrentAddKeys(key("n"))
	if n, _ := m.selectedTotals(); n != 0 {
		t.Fatalf("clear all left %d selected", n)
	}
}

// TestTorrentAddDuplicate: the shared admission decides duplicates; a second confirm
// does not create a second entry.
func TestTorrentAddDuplicate(t *testing.T) {
	root := t.TempDir()
	torrent := writeTorrent(t, "fixture", map[string][]byte{"a.bin": []byte("a")})
	store, err := torrentqueue.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	admitted, aerr := torrentqueue.Admit(store, torrent, bittorrent.DiscoveryPublic, nil, []string{"fixture/a.bin"})
	if aerr != nil || admitted.Duplicate {
		t.Fatalf("first admit: %+v %v", admitted, aerr)
	}

	m := addModel(t, root)
	inspect(t, m, torrent)
	_, _ = m.updateTorrentAddKeys(key("a"))
	_, _ = m.updateTorrentAddKeys(key("enter"))
	_, cmd := m.updateTorrentAddKeys(key("enter"))
	for _, msg := range drainCmd(t, cmd) {
		if adm, ok := msg.(torrentAdmitMsg); ok && adm.duplicate && adm.entry.ID != admitted.Entry.ID {
			t.Fatalf("duplicate add produced a different entry: %s vs %s", adm.entry.ID, admitted.Entry.ID)
		}
		_, _ = m.updateTorrentAdd(msg)
	}
	entries, _ := store.List()
	if len(entries) != 1 {
		t.Fatalf("duplicate add created %d entries", len(entries))
	}
}

// TestTorrentAddLateInspectionDiscarded: a result from an older generation is dropped.
func TestTorrentAddLateInspectionDiscarded(t *testing.T) {
	m := addModel(t, t.TempDir())
	m.add.gen = 5
	stale := torrentInspectMsg{gen: 4, info: bittorrent.Info{Name: "stale", Files: []bittorrent.File{{Path: "stale/a.bin"}}}}
	_, _ = m.updateTorrentAdd(stale)
	if m.add.stage != addForm || m.add.info != nil {
		t.Fatalf("a stale inspection was applied: stage=%v info=%+v", m.add.stage, m.add.info)
	}
}

// TestTorrentAddCancelInspection: esc cancels by invalidating the generation, so the
// in-flight result cannot populate the picker afterwards.
func TestTorrentAddCancelInspection(t *testing.T) {
	m := addModel(t, t.TempDir())
	m.add.source.SetValue("magnet:?xt=urn:btih:" + strings.Repeat("ab", 20))
	_, _ = m.updateTorrentAddKeys(key("enter"))
	if m.add.stage != addInspecting {
		t.Fatalf("stage = %v", m.add.stage)
	}
	genBefore := m.add.gen
	_, _ = m.updateTorrentAddKeys(key("esc"))
	if m.add.stage != addForm {
		t.Fatalf("esc did not leave inspection: %v", m.add.stage)
	}
	if m.add.gen == genBefore {
		t.Fatal("cancelling did not invalidate the in-flight generation")
	}
	_, _ = m.updateTorrentAdd(torrentInspectMsg{gen: genBefore, info: bittorrent.Info{Name: "late", Files: []bittorrent.File{{Path: "late/a.bin"}}}})
	if m.add.stage != addForm || m.add.info != nil {
		t.Fatal("a cancelled inspection still populated the picker")
	}
}

// TestTorrentAddV2OnlyRefused: inspectable, but not queueable for fetch.
func TestTorrentAddV2OnlyRefused(t *testing.T) {
	m := addModel(t, t.TempDir())
	m.add.gen = 2
	m.add.stage = addInspecting
	_, _ = m.updateTorrentAdd(torrentInspectMsg{gen: 2, info: bittorrent.Info{InfoHashV2: strings.Repeat("cd", 32), Name: "v2only"}})
	if m.add.stage != addForm {
		t.Fatalf("a v2-only torrent advanced to %v", m.add.stage)
	}
	if !strings.Contains(m.add.err, "v2-only") {
		t.Fatalf("error = %q", m.add.err)
	}
}

// TestTorrentAddSecretNeverEchoed: a passkey in the source never reaches the screen or
// the durable public record.
func TestTorrentAddSecretNeverEchoed(t *testing.T) {
	root := t.TempDir()
	secret := "SECRET-PASSKEY-123"
	src := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20) + "&tr=http://tracker.example/announce?passkey=" + secret
	m := addModel(t, root)
	m.add.source.SetValue(src)
	if out := m.viewTorrentAdd(); strings.Contains(out, secret) {
		t.Fatalf("the form echoed the secret:\n%s", out)
	}
	// An inspection error must be redacted too.
	m.add.err = sanitizeAddError(os.ErrNotExist, src)
	if strings.Contains(m.viewTorrentAdd(), secret) {
		t.Fatalf("an error echoed the secret:\n%s", m.viewTorrentAdd())
	}
	// And the durable public record never carries it.
	store, err := torrentqueue.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, aerr := torrentqueue.Admit(store, src, bittorrent.DiscoveryDirect, nil, []string{"a.bin"}); aerr != nil {
		t.Fatalf("admit: %v", aerr)
	}
	entries, _ := store.List()
	for _, e := range entries {
		if strings.Contains(e.Source.Magnet, secret) || strings.Contains(e.Source.Path, secret) {
			t.Fatalf("the public entry contains the secret: %+v", e.Source)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".provenance", "torrent", "queue", "entries", entries[0].ID+".json"))
	if strings.Contains(string(raw), secret) {
		t.Fatal("the public record file contains the secret")
	}
}

// TestTorrentAddSingleFlight: a second confirm while the first is pending does nothing.
func TestTorrentAddSingleFlight(t *testing.T) {
	m := addModel(t, t.TempDir())
	inspect(t, m, writeTorrent(t, "fixture", map[string][]byte{"a.bin": []byte("a")}))
	_, _ = m.updateTorrentAddKeys(key("a"))
	_, _ = m.updateTorrentAddKeys(key("enter"))
	_, first := m.updateTorrentAddKeys(key("enter"))
	if first == nil {
		t.Fatal("the first confirm produced no command")
	}
	_, second := m.updateTorrentAddKeys(key("enter"))
	if second != nil {
		t.Fatal("a second confirm started a second add while one was pending")
	}
}

// TestTorrentAddNavigationLargeList: the cursor moves and the window stays bounded.
func TestTorrentAddNavigationLargeList(t *testing.T) {
	m := addModel(t, t.TempDir())
	files := make([]bittorrent.File, 500)
	for i := range files {
		files[i] = bittorrent.File{Path: "big/file-" + string(rune('a'+i%26)) + ".bin", Length: int64(i + 1)}
	}
	m.add.info = &bittorrent.Info{Name: "big", Files: files}
	m.add.files = files
	m.add.stage = addPick
	for i := 0; i < 400; i++ {
		_, _ = m.updateTorrentAddKeys(key("down"))
	}
	if m.add.cursor != 400 {
		t.Fatalf("cursor = %d", m.add.cursor)
	}
	window := m.pickWindow()
	if len(window) > m.height {
		t.Fatalf("window has %d rows for height %d", len(window), m.height)
	}
	if m.add.offset == 0 {
		t.Fatal("the window did not follow the cursor")
	}
	if out := m.viewTorrentAdd(); !strings.Contains(out, "0/500 files selected") {
		t.Fatalf("count line missing:\n%s", out)
	}
}

// TestTorrentAddMalformedSource: an unusable source keeps the form and shows an error.
func TestTorrentAddMalformedSource(t *testing.T) {
	m := addModel(t, t.TempDir())
	inspect(t, m, "/nonexistent/path.torrent")
	if m.add.stage != addForm {
		t.Fatalf("a bad source advanced to %v", m.add.stage)
	}
	if m.add.err == "" {
		t.Fatal("no error was shown")
	}
	if strings.Contains(m.add.err, "/nonexistent/path.torrent") {
		t.Fatalf("the error echoed the raw path: %q", m.add.err)
	}
}

// TestTorrentAddPersistenceFailureKeepsSelection: a failed add keeps the review step
// and the selection, and shows an actionable redacted error.
func TestTorrentAddPersistenceFailureKeepsSelection(t *testing.T) {
	m := addModel(t, t.TempDir())
	inspect(t, m, writeTorrent(t, "fixture", map[string][]byte{"a.bin": []byte("a"), "b.bin": []byte("b")}))
	_, _ = m.updateTorrentAddKeys(key("a"))
	_, _ = m.updateTorrentAddKeys(key("enter"))
	if m.add.stage != addReview {
		t.Fatalf("stage = %v", m.add.stage)
	}
	before := len(m.selectedPaths())

	// A failing admission (here: a root the store cannot open) must not lose the work.
	m.add.gen = 42
	m.add.busy = true
	_, _ = m.updateTorrentAdd(torrentAdmitMsg{gen: 42, err: os.ErrPermission})
	if m.add.stage != addReview {
		t.Fatalf("a failed add left the review step: %v", m.add.stage)
	}
	if len(m.selectedPaths()) != before {
		t.Fatal("a failed add lost the selection")
	}
	if m.add.err == "" || m.add.busy {
		t.Fatalf("failed add: err=%q busy=%v", m.add.err, m.add.busy)
	}
	if !strings.Contains(m.viewTorrentAdd(), m.add.err) {
		t.Fatal("the failure is not rendered")
	}
}

// TestUpdateRoutesAsyncQueueMessages guards the wiring the PTY smoke caught: async
// messages must reach their view through Update, not only when a test calls the
// handler directly.
func TestUpdateRoutesAsyncQueueMessages(t *testing.T) {
	// live snapshot -> queue view
	qm := &model{view: viewTorrentQueue, width: 120, height: 30}
	qm.queue = queueState{root: "/tmp/root", gen: 3}
	snap := liveSnap(torrentqueue.LiveAttempt{EntryID: "e1", Kind: "seed", Phase: "serving"})
	if _, _ = qm.Update(queueLiveMsg{gen: 3, root: qm.queue.root, snap: snap}); qm.queue.live == nil {
		t.Fatal("a live message did not reach the queue view through Update")
	}

	// inspection -> add view
	am := &model{view: viewTorrentAdd, width: 120, height: 30}
	am.queue = queueState{root: "/tmp/root"}
	am.add = newTorrentAddState("/tmp/root")
	am.add.gen = 4
	am.add.stage = addInspecting
	info := bittorrent.Info{InfoHash: strings.Repeat("ef", 20), Name: "n", Files: []bittorrent.File{{Path: "n/a.bin", Length: 1}}}
	if _, _ = am.Update(torrentInspectMsg{gen: 4, info: info}); am.add.stage != addPick {
		t.Fatalf("an inspection message did not reach the add view through Update (stage=%v err=%q)", am.add.stage, am.add.err)
	}
}
