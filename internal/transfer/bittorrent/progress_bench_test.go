package bittorrent

// BT-13: cost of the selected-verified sampling path, measured before adding more
// telemetry on top of it.
//
// The measured function is selectedVerified (BT-12), which the transfer's 100ms poll
// calls once per attempt. Fixtures are built outside the timed loop.

import (
	"os"
	"path/filepath"
	"testing"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// benchFilesFor returns validatedFile entries for every file of a torrent.
func benchFilesFor(tor *at.Torrent) []validatedFile {
	out := make([]validatedFile, 0, len(tor.Files()))
	for _, f := range tor.Files() {
		out = append(out, validatedFile{rel: f.Path(), file: f})
	}
	return out
}

// completeFixtureClient builds a verified, fully complete torrent with real payload:
// size bytes in one file, split into 16 KiB pieces.
func completeFixtureClient(b *testing.B, size int, pieceLen int64) (*at.Torrent, int) {
	b.Helper()
	root := b.TempDir()
	dir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatalf("setup: mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), patterned(size, 7), 0o644); err != nil {
		b.Fatalf("setup: write: %v", err)
	}
	info := metainfo.Info{PieceLength: pieceLen, Name: "fixture"}
	if err := info.BuildFromFilePath(dir); err != nil {
		b.Fatalf("setup: build info: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	client, err := at.NewClient(ctlConfig(root))
	if err != nil {
		b.Fatalf("setup: client: %v", err)
	}
	b.Cleanup(func() { _ = client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		b.Fatalf("setup: add: %v", err)
	}
	if err := tor.VerifyDataContext(b.Context()); err != nil {
		b.Fatalf("setup: verify: %v", err)
	}
	return tor, info.NumPieces()
}

// syntheticTorrent builds a torrent with a chosen piece count and no payload at all:
// storage is deferred, so no file is created and every piece stays incomplete.
func syntheticTorrent(b *testing.B, total int64, pieceLen int64) (*at.Torrent, int) {
	b.Helper()
	info := metainfo.Info{
		Name:        "synthetic",
		PieceLength: pieceLen,
		Length:      total,
		Pieces:      make([]byte, 20*int((total+pieceLen-1)/pieceLen)),
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	dataDir := b.TempDir()
	client, err := at.NewClient(ctlConfig(dataDir))
	if err != nil {
		b.Fatalf("setup: client: %v", err)
	}
	b.Cleanup(func() { _ = client.Close() })
	spec := at.TorrentSpecFromMetaInfo(mi)
	spec.DeferStorageOpen = true
	tor, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		b.Fatalf("setup: add spec: %v", err)
	}
	if tor.Info() == nil {
		b.Fatalf("setup: synthetic torrent has no info")
	}
	// No payload may be created: the point of this fixture is piece count without a
	// proportional file on disk.
	if entries, derr := os.ReadDir(filepath.Join(dataDir, "synthetic")); derr == nil && len(entries) > 0 {
		b.Fatalf("setup: the deferred-storage fixture created %d payload entries", len(entries))
	}
	return tor, info.NumPieces()
}

func benchSelectedVerified(b *testing.B, files []validatedFile) {
	b.Helper()
	b.ReportAllocs()
	var sink int64
	for i := 0; i < b.N; i++ {
		sink = selectedVerified(files)
	}
	if sink < 0 {
		b.Fatalf("negative verified bytes")
	}
}

// 8 MiB in 16 KiB pieces: 512 pieces, every piece verified complete.
func BenchmarkSelectedVerifiedComplete512(b *testing.B) {
	tor, pieces := completeFixtureClient(b, 8<<20, 16<<10)
	files := benchFilesFor(tor)
	if got := selectedVerified(files); got != int64(8<<20) {
		b.Fatalf("setup: verified = %d, want %d", got, 8<<20)
	}
	b.Logf("files=%d pieces=%d verified=%d", len(files), pieces, selectedVerified(files))
	benchSelectedVerified(b, files)
}

// 32 MiB in 16 KiB pieces: 2048 pieces, every piece verified complete.
func BenchmarkSelectedVerifiedComplete2048(b *testing.B) {
	tor, pieces := completeFixtureClient(b, 32<<20, 16<<10)
	files := benchFilesFor(tor)
	b.Logf("files=%d pieces=%d verified=%d", len(files), pieces, selectedVerified(files))
	benchSelectedVerified(b, files)
}

// 1 GiB in 16 KiB pieces: 65,536 pieces, none complete, no payload on disk.
func BenchmarkSelectedVerifiedIncomplete65536(b *testing.B) {
	tor, pieces := syntheticTorrent(b, 1<<30, 16<<10)
	files := benchFilesFor(tor)
	if got := selectedVerified(files); got != 0 {
		b.Fatalf("setup: verified = %d, want 0", got)
	}
	b.Logf("files=%d pieces=%d verified=0", len(files), pieces)
	benchSelectedVerified(b, files)
}

// 4 GiB in 16 KiB pieces: 262,144 pieces, none complete, no payload on disk.
func BenchmarkSelectedVerifiedIncomplete262144(b *testing.B) {
	tor, pieces := syntheticTorrent(b, 4<<30, 16<<10)
	files := benchFilesFor(tor)
	b.Logf("files=%d pieces=%d verified=0", len(files), pieces)
	benchSelectedVerified(b, files)
}
