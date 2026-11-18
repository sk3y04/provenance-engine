package bittorrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
)

// Interoperability with independently generated hybrid metainfo.
//
// The fixture in testdata/ was produced by torrenttools v0.6.2 (Rust), not by
// this package's hand-built builder: see testdata/RECIPE.md. It has a file that
// ends mid-piece (a.bin), a following real file (b.bin), and a file spanning
// several pieces (c.bin), so the v1 continuous layout and the v2 piece-aligned
// layout have different piece boundaries. That is the case a single builder can
// agree with itself on by accident.

const interopTorrent = "testdata/hybrid-torrenttools.torrent"

// interopPayload is the exact byte content recorded in the fixture's recipe.
func interopPayload() map[string][]byte {
	return map[string][]byte{
		"a.bin": bytes.Repeat([]byte{'a'}, 70000),
		"b.bin": bytes.Repeat([]byte{'b'}, 40000),
		"c.bin": bytes.Repeat([]byte{'c'}, 100000),
	}
}

// loadInteropFixture reads the committed metainfo and recomputes both identities
// from the original bencoded info bytes.
// sha1SumBytes is the v1 piece hash of a raw piece.
func sha1SumBytes(b []byte) []byte {
	sum := sha1.Sum(b)
	return sum[:]
}

func loadInteropFixture(t *testing.T) (mi *metainfo.MetaInfo, wantV1, wantV2 string) {
	t.Helper()
	mi, err := metainfo.LoadFromFile(interopTorrent)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	v1 := metainfo.HashBytes(mi.InfoBytes).HexString()
	v2 := infohash_v2.HashBytes(mi.InfoBytes)
	return mi, v1, v2.HexString()
}

// TestHybridInteropMetainfo: the fixture really is a hybrid, and inspection
// reports both full identities and the real files only.
func TestHybridInteropMetainfo(t *testing.T) {
	mi, wantV1, wantV2 := loadInteropFixture(t)
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatalf("unmarshal info: %v", err)
	}
	if !info.HasV1() || !info.HasV2() {
		t.Fatalf("fixture is not hybrid: v1=%v v2=%v", info.HasV1(), info.HasV2())
	}
	if len(mi.PieceLayers) != 3 {
		t.Errorf("piece layers = %d, want 3 (one per file longer than a piece)", len(mi.PieceLayers))
	}
	// Protocol padding entries, if a generator emits them, carry attr "p".
	// torrenttools v0.6.2 emits none; say so rather than pretending otherwise.
	paddingEntries := 0
	for _, f := range info.UpvertedFiles() {
		if isPaddingFile(f) {
			paddingEntries++
		}
	}
	if paddingEntries != 0 {
		t.Logf("fixture carries %d protocol padding entries", paddingEntries)
	}

	inspected, err := Inspect(context.Background(), interopTorrent, InspectConfig{Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.InfoHash != wantV1 || inspected.InfoHashV2 != wantV2 {
		t.Fatalf("identity = %q / %q, want %s / %s", inspected.InfoHash, inspected.InfoHashV2, wantV1, wantV2)
	}
	if len(inspected.Files) != 3 {
		t.Fatalf("files = %+v, want the three real files", inspected.Files)
	}
	wantPaths := []string{"fixture/a.bin", "fixture/b.bin", "fixture/c.bin"}
	for i, want := range wantPaths {
		if inspected.Files[i].Path != want {
			t.Errorf("file[%d] = %q, want %q", i, inspected.Files[i].Path, want)
		}
	}
	for _, f := range inspected.Files {
		if strings.Contains(f.Path, "_padding") {
			t.Errorf("padding offered as a user file: %q", f.Path)
		}
	}
}

// TestHybridInteropGeneratorLimitation documents what the independent generator
// actually produces: torrenttools v0.6.2 emits a hybrid whose v1 "pieces" cover
// the continuous layout, with no BEP 47 padding entries. The pinned library lays
// files out piece-aligned for any v2 torrent and verifies against the v2 piece
// layers, so the two disagree and the seeder cannot even verify its own data.
//
// This is a generator/interoperability limitation, not an adapter defect: the
// payload on disk is exactly what the recipe says, and the failure happens in
// pure library verification before Provenance is involved. Until a generator
// that emits pad-aligned hybrid metainfo is available, fetch-level independent
// interoperability stays unproven and padded hybrids are not claimed as tested.
func TestHybridInteropGeneratorLimitation(t *testing.T) {
	mi, _, _ := loadInteropFixture(t)
	files := interopPayload()

	root := t.TempDir()
	dir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := hermeticConfig(root, true)
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("seeder client: %v", err)
	}
	defer client.Close()
	verr := func() error {
		tor, _, aerr := client.AddTorrentSpec(at.TorrentSpecFromMetaInfo(mi))
		if aerr != nil {
			return aerr
		}
		<-tor.GotInfo()
		return tor.VerifyData()
	}()
	if verr == nil {
		// If a future generator/toolchain makes this fixture verifiable, the
		// fetch-level interop test should be reinstated rather than silently
		// passing here.
		t.Skip("the independently generated hybrid verifies with this library; add a fetch-level interop test")
	}
	if !strings.Contains(verr.Error(), "expected hash") {
		t.Fatalf("unexpected verification failure: %v", verr)
	}
	t.Logf("pinned library rejects the generator's hybrid at verification: %v", verr)
}

// TestPaddingEntriesAreNotUserFiles: the adapter's BEP 47 padding policy, checked
// directly because the independent generator emits no padding entries. A padding
// entry is declared by its "attr" containing "p", not by its name.
func TestPaddingEntriesAreNotUserFiles(t *testing.T) {
	payload := patterned(40000, 'x')
	padLen := int64(25536) // to the next 64 KiB piece boundary
	second := patterned(1000, 'y')

	// The info dict is built by hand: metainfo.FileInfo does not round-trip the
	// BEP 47 "attr" field through bencode in this library version.
	content := append(append(append([]byte{}, payload...), make([]byte, padLen)...), second...)
	var pieces []byte
	for off := 0; off+16*1024 <= len(content); off += 16 * 1024 {
		sum := sha1SumBytes(content[off : off+16*1024])
		pieces = append(pieces, sum...)
	}
	infoBytes := bencode.MustMarshal(map[string]any{
		"name":         "fixture",
		"piece length": int64(16 * 1024),
		"pieces":       pieces,
		"files": []any{
			map[string]any{"length": int64(len(payload)), "path": []string{"a.bin"}},
			map[string]any{"length": padLen, "path": []string{"_padding", "a.bin"}, "attr": "p"},
			map[string]any{"length": int64(len(second)), "path": []string{"b.bin"}},
		},
	})
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	mi.SetDefaults()
	path := writeTorrentFile(t, mi)

	// Sanity: the entry really is marked padding once parsed.
	parsed, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entries := parsed.UpvertedFiles()
	if len(entries) != 3 || !isPaddingFile(entries[1]) {
		t.Fatalf("fixture does not carry a padding entry: %+v", entries)
	}

	inspected, err := Inspect(context.Background(), path, InspectConfig{Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, f := range inspected.Files {
		if strings.Contains(f.Path, "padding") {
			t.Errorf("padding entry offered as a user file: %q", f.Path)
		}
	}
	if len(inspected.Files) != 2 {
		t.Errorf("files = %d (%+v), want the two real files", len(inspected.Files), inspected.Files)
	}
}
