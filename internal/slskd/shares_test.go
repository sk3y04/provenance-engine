package slskd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// shareFixture builds a source tree, a curated root and a manager over them.
func shareFixture(t *testing.T, files map[string]string) (*ShareManager, string, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	root := filepath.Join(base, "share-root")
	for _, d := range []string{src, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range files {
		full := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenShareStore(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return &ShareManager{Root: root, Store: store}, src, root
}

// TestSharePreviewExcludesSecrets is the pre-share confirmation: it must report
// exact counts and exclude everything the denylist names.
func TestSharePreviewExcludesSecrets(t *testing.T) {
	m, src, _ := shareFixture(t, map[string]string{
		"Album/track1.flac":     "aaaa",
		"Album/track2.flac":     "bb",
		"Album/cover.jpg":       "c",
		"Album/Thumbs.db":       "x",
		"Album/.DS_Store":       "x",
		"Album/notes.ini":       "x",
		"Album/settings.json":   "x",
		"Album/secret.env":      "x",
		"Album/key.pem":         "x",
		"Album/backup.sqlite":   "x",
		"Album/readme.pdf":      "x",
		"Album/.hidden.flac":    "x",
		"Album/nested/deep.wav": "dd",
	})
	preview, err := m.Preview(src)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	want := []string{"Album/cover.jpg", "Album/nested/deep.wav", "Album/track1.flac", "Album/track2.flac"}
	if len(preview.Files) != len(want) {
		var got []string
		for _, f := range preview.Files {
			got = append(got, f.RelPath)
		}
		t.Fatalf("files = %v, want %v", got, want)
	}
	for i, f := range preview.Files {
		if f.RelPath != want[i] {
			t.Fatalf("file %d = %q, want %q", i, f.RelPath, want[i])
		}
	}
	if preview.TotalBytes != 4+2+1+2 {
		t.Fatalf("total bytes = %d", preview.TotalBytes)
	}
	// Thumbs.db, .DS_Store, notes.ini, settings.json, secret.env, key.pem,
	// backup.sqlite, readme.pdf, .hidden.flac
	if preview.Excluded != 9 {
		t.Fatalf("excluded = %d, want 9", preview.Excluded)
	}
	if preview.Extensions[".flac"] != 2 || preview.Extensions[".jpg"] != 1 {
		t.Fatalf("extension histogram = %v", preview.Extensions)
	}
}

// TestSharePreviewCountsSymlinksWithoutFollowing proves a symlink is reported
// and never enumerated as shareable content.
func TestSharePreviewCountsSymlinksWithoutFollowing(t *testing.T) {
	m, src, _ := shareFixture(t, map[string]string{"Album/track.flac": "aaaa"})
	outside := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "Album", "link.flac")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	preview, err := m.Preview(src)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Symlinks != 1 {
		t.Fatalf("symlinks = %d", preview.Symlinks)
	}
	for _, f := range preview.Files {
		if strings.Contains(f.RelPath, "link.flac") {
			t.Fatal("a symlink was offered for sharing")
		}
	}
}

// TestShareAddCreatesHardlinksAndRescans proves opt-in materialises hardlinks
// (same inode, so zero bytes are copied) and only then asks slskd to rescan.
func TestShareAddCreatesHardlinksAndRescans(t *testing.T) {
	var rescans int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/api/v0/shares") {
			atomic.AddInt32(&rescans, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})

	m, src, root := shareFixture(t, map[string]string{"Album/track.flac": "aaaa"})
	m.Client = client

	rule, preview, err := m.Add(context.Background(), src, "MyMusic")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if rule.ID != "mymusic" || rule.AliasFolder != "MyMusic" || rule.FileCount != 1 {
		t.Fatalf("rule = %+v", rule)
	}
	if preview.TotalBytes != 4 {
		t.Fatalf("preview bytes = %d", preview.TotalBytes)
	}
	if atomic.LoadInt32(&rescans) != 1 {
		t.Fatal("slskd was not asked to rescan")
	}

	srcInfo, err := os.Stat(filepath.Join(src, "Album", "track.flac"))
	if err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Stat(filepath.Join(root, "MyMusic", "Album", "track.flac"))
	if err != nil {
		t.Fatalf("link not created: %v", err)
	}
	if !os.SameFile(srcInfo, linkInfo) {
		t.Fatal("the share copy is not a hardlink to the source (bytes were copied)")
	}

	stored, err := m.Store.List()
	if err != nil || len(stored) != 1 || stored[0].SourcePath != src {
		t.Fatalf("stored rules = %+v, %v", stored, err)
	}

	// Idempotent: adding again must not fail on the existing links.
	if _, _, err := m.Add(context.Background(), src, "MyMusic"); err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if got := atomic.LoadInt32(&rescans); got != 2 {
		t.Fatalf("rescans = %d", got)
	}
}

// TestShareAddFailsFastOnCrossDevice proves EXDEV produces an actionable error
// naming both roots instead of a half-materialised share.
func TestShareAddFailsFastOnCrossDevice(t *testing.T) {
	m, src, root := shareFixture(t, map[string]string{"Album/track.flac": "aaaa"})
	m.link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	_, _, err := m.Add(context.Background(), src, "MyMusic")
	if err == nil {
		t.Fatal("cross-device link reported as success")
	}
	if !strings.Contains(err.Error(), "different filesystems") || !strings.Contains(err.Error(), root) {
		t.Fatalf("error is not actionable: %v", err)
	}
}

// TestShareAddRefusesOverlappingRoots keeps a share from containing itself.
func TestShareAddRefusesOverlappingRoots(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "media")
	if err := os.MkdirAll(filepath.Join(src, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Album", "t.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &ShareManager{Root: filepath.Join(src, "share-root")}
	if _, _, err := m.Add(context.Background(), src, "a"); err == nil {
		t.Fatal("share root inside the source was accepted")
	}
}

// TestShareRemoveDeletesLinksAndRescans proves opt-out removes the materialised
// tree (and only that) and asks slskd to rescan.
func TestShareRemoveDeletesLinksAndRescans(t *testing.T) {
	var rescans int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/api/v0/shares") {
			atomic.AddInt32(&rescans, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})

	m, src, root := shareFixture(t, map[string]string{"Album/track.flac": "aaaa"})
	m.Client = client
	if _, _, err := m.Add(context.Background(), src, "MyMusic"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), "mymusic"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "MyMusic")); !os.IsNotExist(err) {
		t.Fatal("share links still exist after removal")
	}
	if _, err := os.Stat(filepath.Join(src, "Album", "track.flac")); err != nil {
		t.Fatalf("removal deleted the SOURCE file: %v", err)
	}
	if rules, _ := m.Store.List(); len(rules) != 0 {
		t.Fatalf("rule record survived removal: %+v", rules)
	}
	if atomic.LoadInt32(&rescans) != 2 {
		t.Fatalf("rescans = %d, want 2 (add + remove)", atomic.LoadInt32(&rescans))
	}
}

// TestShareAuditRejectsPlantedSymlink is the guard that matters: slskd follows
// symlinks out of the share root, so the tree must be link-free.
func TestShareAuditRejectsPlantedSymlink(t *testing.T) {
	m, src, root := shareFixture(t, map[string]string{"Album/track.flac": "aaaa"})
	if _, _, err := m.Add(context.Background(), src, "MyMusic"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Audit(); err != nil {
		t.Fatalf("clean audit failed: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "MyMusic", "evil.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := m.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symlinks) != 1 {
		t.Fatalf("audit found %d symlinks", len(res.Symlinks))
	}
}

// TestSharePreviewReportsMissingSource proves a bad path is an error, not an
// empty preview that looks shareable.
func TestSharePreviewReportsMissingSource(t *testing.T) {
	m := &ShareManager{Root: t.TempDir()}
	if _, err := m.Preview(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing source accepted")
	}
}

// TestShareBrowsePaginatesLargeCatalogs is the 100k-file guard: a big share must
// be paginated here rather than shipped whole.
func TestShareBrowsePaginatesLargeCatalogs(t *testing.T) {
	const total = 10000
	files := make([]any, 0, total)
	for i := 0; i < total; i++ {
		files = append(files, map[string]any{
			"filename": fmt.Sprintf("track-%05d.flac", i), "size": 1000, "extension": "flac",
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v0/shares/contents") {
			writeJSON(w, 200, []any{map[string]any{"name": `Provenance\Album`, "fileCount": total, "files": files}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})
	m := &ShareManager{Root: t.TempDir(), Client: client}

	page, err := m.Browse(context.Background(), 2, 100, "")
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if page.TotalFiles != total || page.Pages != total/100 {
		t.Fatalf("page = %+v", page)
	}
	if len(page.Entries) != 100 {
		t.Fatalf("%d entries on the page", len(page.Entries))
	}
	if !strings.Contains(page.Entries[0], "track-00100.flac") {
		t.Fatalf("page 2 starts at %q", page.Entries[0])
	}

	filtered, err := m.Browse(context.Background(), 1, 10, "track-0004")
	if err != nil {
		t.Fatal(err)
	}
	if filtered.TotalFiles != 10 {
		t.Fatalf("filtered total = %d", filtered.TotalFiles)
	}
}

// TestShareMisconfigurationWarning covers a silent zero-share state (#1663).
func TestShareMisconfigurationWarning(t *testing.T) {
	if w := MisconfigurationWarnings(ShareWarningContext{EnabledRules: 0, ShareFiles: 0}); len(w) != 0 {
		t.Fatal("warned with no rules")
	}
	if w := MisconfigurationWarnings(ShareWarningContext{EnabledRules: 2, ShareFiles: 5}); len(w) != 0 {
		t.Fatal("warned while files are shared")
	}
	w := MisconfigurationWarnings(ShareWarningContext{EnabledRules: 2, ShareFiles: 0, OptionsReadable: true, NoShareScan: true})
	if len(w) != 1 || w[0].Code != "#1663" || !strings.Contains(w[0].Message, "no_share_scan") {
		t.Fatalf("warning = %+v", w)
	}
	w = MisconfigurationWarnings(ShareWarningContext{EnabledRules: 2, ShareFiles: 0})
	if len(w) != 1 || w[0].Code != "#1663" {
		t.Fatalf("warning = %+v", w)
	}
}

// TestShareStatusFlattensAliasMap proves the "local" map key is not mistaken for
// an alias.
func TestShareStatusFlattensAliasMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v0/shares") {
			_, _ = w.Write([]byte(`{"local":[{"id":"X","alias":"Provenance","isExcluded":false,"localPath":"/shared","raw":"[Provenance]/shared","remotePath":"Provenance","directories":1,"files":5}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client, _ := New(Config{BaseURL: srv.URL, APIKey: "k"})
	m := &ShareManager{Root: t.TempDir(), Client: client}
	statuses, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Alias != "Provenance" || statuses[0].Files != 5 {
		t.Fatalf("status = %+v", statuses)
	}
}

// TestShareAddRejectsEmptyShare proves a folder with only denied files is
// refused instead of materialising an empty, useless share.
func TestShareAddRejectsEmptyShare(t *testing.T) {
	m, src, _ := shareFixture(t, map[string]string{".DS_Store": "x", "Thumbs.db": "x"})
	if _, _, err := m.Add(context.Background(), src, "nothing"); err == nil {
		t.Fatal("empty share accepted")
	}
}
