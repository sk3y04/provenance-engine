package bittorrent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// spyStorage wraps the real file storage and counts OpenTorrent calls, which is
// the moment payload storage exists. It answers "did this job ever reach the
// point of touching payload?" without inferring from files on disk.
type spyStorage struct {
	inner storage.ClientImpl
	opens atomic.Int32
}

func (s *spyStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	s.opens.Add(1)
	return s.inner.OpenTorrent(ctx, info, ih)
}

func newSpyStorage(t *testing.T, staging string) *spyStorage {
	t.Helper()
	inner := storage.NewFile(staging)
	t.Cleanup(func() { _ = inner.Close() })
	return &spyStorage{inner: inner}
}

// writeMetaInfo writes a metainfo to a temp file.
func writeMetaInfo(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.torrent")
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent file: %v", err)
	}
	return path
}

// TestInspectLocalTorrentBuildsNoClient: a .torrent on disk is metadata already,
// so inspection parses it in memory. No client, no listener, no temp dir.
func TestInspectLocalTorrentBuildsNoClient(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": patterned(5000, 'a'),
		"b.bin": patterned(3000, 'b'),
	})
	path := s.torrentFile(t)

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	clients := 0
	info, err := Inspect(context.Background(), path, InspectConfig{
		Discovery: DiscoveryDirect,
		onClient:  func() { clients++ },
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if clients != 0 {
		t.Errorf("Inspecting a local .torrent constructed %d client(s), want 0", clients)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("inspection left files in the temp dir: %v (%v)", entries, err)
	}

	if info.InfoHash != s.infohash() {
		t.Errorf("infohash = %s, want %s", info.InfoHash, s.infohash())
	}
	if info.Name != "fixture" || len(info.Files) != 2 {
		t.Fatalf("info = %+v", info)
	}
	if info.Files[0].Path != "fixture/a.bin" || info.Files[0].Length != 5000 || info.Files[0].Offset != 0 {
		t.Errorf("file[0] = %+v", info.Files[0])
	}
	if info.Files[1].Path != "fixture/b.bin" || info.Files[1].Offset != 5000 {
		t.Errorf("file[1] = %+v", info.Files[1])
	}
	if info.Source != "magnet:?xt=urn:btih:"+s.infohash() {
		t.Errorf("source = %q, want a sanitised magnet", info.Source)
	}
	if info.TotalLength != 8000 {
		t.Errorf("total length = %d, want 8000", info.TotalLength)
	}
}

// TestInspectMagnetRequestsNoPayload is the negative proof for a magnet: the
// seeder's own counters must show metadata traffic and no piece data at all,
// while no staging or temporary artefact is created.
func TestInspectMagnetRequestsNoPayload(t *testing.T) {
	content := patterned(120000, 'p')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": patterned(4000, 'a'),
		"b.bin": content,
	})

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	info, err := Inspect(context.Background(), s.magnet(), InspectConfig{
		Peers:           []string{s.peer()},
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(info.Files) != 2 || info.Files[1].Length != int64(len(content)) {
		t.Fatalf("info = %+v", info)
	}

	// The seeder is the only peer. If the reader had requested a piece, or
	// served one, its counters would move. Metadata is not piece data, so a
	// non-zero count here is a real leak, not an accounting artefact.
	stats := s.torrent.Stats()
	if got := stats.BytesWrittenData.Int64(); got != 0 {
		t.Errorf("seeder wrote %d payload bytes to an inspector, want 0", got)
	}
	if got := stats.ChunksWritten.Int64(); got != 0 {
		t.Errorf("seeder served %d chunks to an inspector, want 0", got)
	}

	// Nothing may be left behind anywhere: the inspection temp dir is removed,
	// and no completion database or payload directory was created in it.
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("inspection left a temporary directory: %v (%v)", entries, err)
	}
}

// TestInspectedMagnetCanThenBeFetched: inspection must not have consumed or
// locked anything the fetch needs.
func TestInspectedMagnetCanThenBeFetched(t *testing.T) {
	content := patterned(9000, 'f')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": content})
	staging := t.TempDir()

	if _, err := inspectForTest(t, s.magnet(), s.peer()); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	sess := newTestSession(t, staging, s.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	cap, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("fetch after inspect: %v", err)
	}
	if len(cap.Assets) != 1 || cap.Assets[0].SHA256 != sha256Hex(content) {
		t.Fatalf("capture = %+v", cap)
	}
}

// TestFetchRejectsBeforeOpeningStorage: admission failures must happen before
// payload storage exists, not after. The spy counts the exact moment.
func TestFetchRejectsBeforeOpeningStorage(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(4000, 'a')})

	t.Run("unknown selection", func(t *testing.T) {
		staging := t.TempDir()
		spy := newSpyStorage(t, staging)
		sess := newTestSessionWithStorage(t, staging, spy, s.peer())
		ctx, cancel := waitCtx(t)
		defer cancel()

		if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/missing.bin"}); !errors.Is(err, ErrSelectionNotFound) {
			t.Fatalf("Fetch error = %v, want ErrSelectionNotFound", err)
		}
		if n := spy.opens.Load(); n != 0 {
			t.Errorf("payload storage opened %d time(s) for a rejected selection", n)
		}
		if _, err := os.Stat(filepath.Join(staging, "fixture")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("payload directory exists after a rejected selection: %v", err)
		}
	})

	t.Run("path collision", func(t *testing.T) {
		// Two metainfo entries that collapse onto one path: the second write
		// would silently replace the first.
		info := metainfo.Info{
			Name:        "fixture",
			PieceLength: 16 * 1024,
			Files: []metainfo.FileInfo{
				{Length: 10, Path: []string{"same.bin"}},
				{Length: 10, Path: []string{"same.bin"}},
			},
		}
		if err := info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(make([]byte, fi.Length))), nil
		}); err != nil {
			t.Fatalf("generate pieces: %v", err)
		}
		mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
		mi.SetDefaults()
		path := writeMetaInfo(t, mi)

		staging := t.TempDir()
		spy := newSpyStorage(t, staging)
		sess := newTestSessionWithStorage(t, staging, spy, s.peer())
		ctx, cancel := waitCtx(t)
		defer cancel()

		if _, err := sess.Fetch(ctx, path, []string{"fixture/same.bin"}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Fetch error = %v, want ErrUnsafePath", err)
		}
		if n := spy.opens.Load(); n != 0 {
			t.Errorf("payload storage opened %d time(s) for a colliding layout", n)
		}
	})

	t.Run("existing symlink", func(t *testing.T) {
		staging := t.TempDir()
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(staging, "fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(staging, "fixture", "a.bin")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		spy := newSpyStorage(t, staging)
		sess := newTestSessionWithStorage(t, staging, spy, s.peer())
		ctx, cancel := waitCtx(t)
		defer cancel()

		if _, err := sess.Fetch(ctx, s.magnet(), []string{"fixture/a.bin"}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Fetch error = %v, want ErrUnsafePath", err)
		}
		if n := spy.opens.Load(); n != 0 {
			t.Errorf("payload storage opened %d time(s) over a symlink", n)
		}
	})

	t.Run("admitted selection opens storage once", func(t *testing.T) {
		content := patterned(6000, 'z')
		okSeeder := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": content})
		staging := t.TempDir()
		spy := newSpyStorage(t, staging)
		sess := newTestSessionWithStorage(t, staging, spy, okSeeder.peer())
		ctx, cancel := waitCtx(t)
		defer cancel()

		cap, err := sess.Fetch(ctx, okSeeder.magnet(), []string{"fixture/a.bin"})
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if n := spy.opens.Load(); n != 1 {
			t.Errorf("payload storage opened %d time(s), want exactly 1", n)
		}
		if len(cap.Assets) != 1 || cap.Assets[0].SHA256 != sha256Hex(content) {
			t.Errorf("capture = %+v", cap)
		}
	})
}

// TestInspectMagnetTimeoutAndCancelCleanUp: a bounded or cancelled metadata wait
// must leave no client, temporary directory or lock behind.
func TestInspectMagnetTimeoutAndCancelCleanUp(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)

	t.Run("timeout", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		start := time.Now()
		_, err := Inspect(context.Background(), magnet, InspectConfig{
			Discovery:       DiscoveryDirect,
			MetadataTimeout: 250 * time.Millisecond,
		})
		if !errors.Is(err, ErrMetadataTimeout) {
			t.Fatalf("error = %v, want ErrMetadataTimeout", err)
		}
		if waited := time.Since(start); waited > 15*time.Second {
			t.Errorf("timeout took %s", waited)
		}
		if entries, rerr := os.ReadDir(tmp); rerr != nil || len(entries) != 0 {
			t.Errorf("timeout left a temporary directory: %v (%v)", entries, rerr)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		_, err := Inspect(ctx, magnet, InspectConfig{
			Discovery:       DiscoveryDirect,
			MetadataTimeout: 30 * time.Second,
		})
		if !errors.Is(err, ErrMetadataTimeout) {
			t.Fatalf("error = %v, want ErrMetadataTimeout", err)
		}
		if entries, rerr := os.ReadDir(tmp); rerr != nil || len(entries) != 0 {
			t.Errorf("cancellation left a temporary directory: %v (%v)", entries, rerr)
		}
	})

	// A later inspection over a real peer still works: nothing was left open.
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(1000, 'a')})
	if _, err := inspectForTest(t, s.magnet(), s.peer()); err != nil {
		t.Fatalf("inspect after timeout: %v", err)
	}
}

// TestInspectRejectsMalformedV2Identity: metainfo that claims v2 without a file
// tree is malformed and is refused before any layout is used. A well-formed v2
// torrent is accepted (see TestV2OnlyInspectIsOffline); v2 magnets stay unsupported.
func TestInspectRejectsMalformedV2Identity(t *testing.T) {
	v1 := metainfoFor(t, "fixture", []string{"a.bin"}, patterned(1000, 'a'))
	info, err := v1.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.MetaVersion = 2
	v2 := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	v2.SetDefaults()
	path := writeMetaInfo(t, v2)

	if _, err := Inspect(context.Background(), path, InspectConfig{Discovery: DiscoveryDirect}); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Errorf("malformed v2 metainfo error = %v, want ErrUnsupportedIdentity", err)
	}

	// The same refusal for a fetch, and before anything is added to a client.
	staging := t.TempDir()
	spy := newSpyStorage(t, staging)
	sess := newTestSessionWithStorage(t, staging, spy, "")
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, path, []string{"fixture/a.bin"}); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Errorf("malformed v2 fetch error = %v, want ErrUnsupportedIdentity", err)
	}
	if n := spy.opens.Load(); n != 0 {
		t.Errorf("payload storage opened %d time(s) for a malformed v2 torrent", n)
	}
	if _, err := os.Stat(filepath.Join(staging, "fixture")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("malformed v2 fetch created a payload directory: %v", err)
	}

	// A btmh magnet is now accepted for resolution: with no peers it must fail
	// as a bounded metadata wait, not as an identity rejection.
	v2magnet := "magnet:?xt=urn:btmh:1220" + strings.Repeat("cd", 32)
	if _, err := Inspect(context.Background(), v2magnet, InspectConfig{
		Discovery: DiscoveryDirect, MetadataTimeout: 200 * time.Millisecond,
	}); !errors.Is(err, ErrMetadataTimeout) {
		t.Errorf("btmh magnet error = %v, want ErrMetadataTimeout", err)
	}
	// A malformed multihash is still refused before any client work.
	bad := "magnet:?xt=urn:btmh:12" + strings.Repeat("cd", 32)
	if _, err := Inspect(context.Background(), bad, InspectConfig{Discovery: DiscoveryDirect}); !errors.Is(err, ErrUnsupportedSource) {
		t.Errorf("malformed btmh magnet error = %v, want ErrUnsupportedSource", err)
	}
}

// TestClientConstructionCreatesDataDir documents the upstream behaviour this
// slice works around: at.NewClient with the default storage materialises its
// data directory (and, with a database-backed piece completion, its completion
// DB) immediately, independently of DeferStorageOpen and OpenStorage. That is
// why inspection never points a client at a staging root.
func TestClientConstructionCreatesDataDir(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "fresh")
	cfg := hermeticConfig(dataDir, false)
	cfg.DataDir = dataDir
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("client construction did not create its data dir: %v", err)
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) > 0 {
		t.Logf("client construction created %d entries in the data dir (e.g. a piece-completion DB)", len(entries))
	}
}
