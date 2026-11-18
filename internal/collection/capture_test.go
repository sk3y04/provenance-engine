package collection

import (
	"testing"
	"unicode/utf8"

	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// TestRecordCaptureItemsAreSortedByURL: capture manifests must be
// reproducible across runs, so items are persisted in a stable order
// instead of random map-iteration order.
func TestRecordCaptureItemsAreSortedByURL(t *testing.T) {
	dir := t.TempDir()
	c := Collection{
		Name:    "feed",
		URL:     "https://example.com/feed",
		Site:    "generic",
		Options: config.Config{OutputDir: dir},
	}
	urlToItem := map[string]manifest.Item{
		"https://cdn/z.jpg": {URL: "https://cdn/z.jpg", Filename: "z.jpg"},
		"https://cdn/a.jpg": {URL: "https://cdn/a.jpg", Filename: "a.jpg"},
		"https://cdn/m.jpg": {URL: "https://cdn/m.jpg", Filename: "m.jpg"},
		"https://cdn/b.jpg": {URL: "https://cdn/b.jpg", Filename: "b.jpg"},
	}
	if err := recordCapture(c, "sess-test", urlToItem); err != nil {
		t.Fatalf("recordCapture: %v", err)
	}
	runs, err := manifest.ListRunManifests(dir)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run manifests = %v (err=%v), want exactly 1", runs, err)
	}
	cm, err := manifest.ReadCaptureManifest(runs[0])
	if err != nil {
		t.Fatalf("read capture manifest: %v", err)
	}
	if len(cm.Items) != len(urlToItem) {
		t.Fatalf("items = %d, want %d", len(cm.Items), len(urlToItem))
	}
	for i := 1; i < len(cm.Items); i++ {
		if cm.Items[i-1].URL >= cm.Items[i].URL {
			t.Fatalf("items not sorted by URL at %d: %q then %q", i, cm.Items[i-1].URL, cm.Items[i].URL)
		}
	}
}

func TestTrimIsRuneSafe(t *testing.T) {
	// Multi-byte path: byte slicing would cut a rune in half and emit
	// invalid UTF-8 into the dry-run output.
	in := "https://例え.テスト/長いパス/ファイル.jpg"
	got := trim(in, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("trim produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 10 {
		t.Fatalf("trim rune count = %d, want <= 10", n)
	}
	if got == in {
		t.Fatal("trim did not shorten a long string")
	}
	if short := trim("abc", 10); short != "abc" {
		t.Fatalf("trim short string = %q, want unchanged", short)
	}
}
