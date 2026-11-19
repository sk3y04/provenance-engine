package bittorrent

// BT-12: what a progress sample actually measures. selectedVerified counts verified
// bytes of the selected files' own ranges, so a piece shared with an unselected
// neighbour contributes the selected share alone and a piece still downloading
// contributes nothing.
//
// These are deterministic checks against a local client's own piece states: no
// transfer, no peer, no timing.

import (
	"os"
	"path/filepath"
	"testing"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// progressFixture writes two 48 KiB files and returns their metainfo with 32 KiB
// pieces, so piece 1 spans the boundary and carries 16 KiB of each file.
func progressFixture(t *testing.T) (*metainfo.MetaInfo, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	for name, seed := range map[string]byte{"a.bin": 3, "b.bin": 11} {
		if err := os.WriteFile(filepath.Join(dir, name), patterned(48<<10, seed), 0o644); err != nil {
			t.Fatalf("setup: write %s: %v", name, err)
		}
	}
	info := metainfo.Info{PieceLength: 32 << 10, Name: "fixture"}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatalf("setup: build info: %v", err)
	}
	return &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}, root
}

// verifiedByRel maps a file's staging-relative path to its verified byte count.
func verifiedByRel(t *testing.T, tor *at.Torrent, rels ...string) map[string]int64 {
	t.Helper()
	all := map[string]int64{}
	for _, f := range tor.Files() {
		all[filepath.ToSlash(f.Path())] = f.Length()
	}
	out := map[string]int64{}
	for _, rel := range rels {
		var match *at.File
		for _, f := range tor.Files() {
			if filepath.ToSlash(f.Path()) == rel {
				match = f
			}
		}
		if match == nil {
			t.Fatalf("fixture has no file %q (have %v)", rel, all)
		}
		out[rel] = selectedVerified([]validatedFile{{rel: rel, file: match}})
	}
	return out
}

func TestSelectedVerifiedCompleteTorrent(t *testing.T) {
	mi, root := progressFixture(t)
	peer := ctlServe(t, root, "fixture", mi)
	tor := peer.tor

	got := verifiedByRel(t, tor, "fixture/a.bin", "fixture/b.bin")
	// Piece 0 is 32 KiB of a; piece 1 is 16 KiB of a and 16 KiB of b. Counting whole
	// pieces would report 64 KiB for a single file, which is why the per-file share is
	// the only correct projection.
	if got["fixture/a.bin"] != 48<<10 {
		t.Fatalf("a.bin verified = %d, want %d (a whole-piece count would be %d)",
			got["fixture/a.bin"], 48<<10, 64<<10)
	}
	if got["fixture/b.bin"] != 48<<10 {
		t.Fatalf("b.bin verified = %d, want %d", got["fixture/b.bin"], 48<<10)
	}
	selected := []validatedFile{}
	for _, f := range tor.Files() {
		selected = append(selected, validatedFile{rel: filepath.ToSlash(f.Path()), file: f})
	}
	if total := selectedVerified(selected); total != 96<<10 {
		t.Fatalf("both files verified = %d, want %d", total, 96<<10)
	}
	t.Logf("complete: a=%d b=%d", got["fixture/a.bin"], got["fixture/b.bin"])
}

func TestSelectedVerifiedPartialData(t *testing.T) {
	mi, root := progressFixture(t)
	// Only a.bin's first 32 KiB exists: piece 0 can verify, piece 1 (16 KiB of a plus
	// 16 KiB of b) cannot.
	partial := t.TempDir()
	dir := filepath.Join(partial, "fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	head := patterned(32<<10, 3)
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), head, 0o644); err != nil {
		t.Fatalf("setup: write partial a.bin: %v", err)
	}

	client, err := at.NewClient(ctlConfig(partial))
	if err != nil {
		t.Fatalf("setup: client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("setup: add torrent: %v", err)
	}
	if err := tor.VerifyDataContext(t.Context()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	var aFile *at.File
	for _, f := range tor.Files() {
		if filepath.Base(f.Path()) == "a.bin" {
			aFile = f
		}
	}
	if aFile == nil {
		t.Fatal("fixture has no a.bin")
	}
	verified := selectedVerified([]validatedFile{{rel: "fixture/a.bin", file: aFile}})
	if verified != 32<<10 {
		t.Fatalf("verified = %d, want only piece 0 (%d): an incomplete piece must not count", verified, 32<<10)
	}
	if verified == aFile.Length() {
		t.Fatalf("verified reached the file length with piece 1 incomplete")
	}
	t.Logf("partial: verified=%d of %d", verified, aFile.Length())
	_ = root
}
