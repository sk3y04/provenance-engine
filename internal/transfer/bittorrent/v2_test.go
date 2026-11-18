package bittorrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
	"golang.org/x/time/rate"
)

// v2 fixtures.
//
// BEP 52 piece hashing, as the pinned library actually computes it (verified
// against v1.62.0's merkle.Hash and ValidatePieceLayers, and used to seed and
// download real torrents in these tests):
//
//   - files are laid out piece-aligned, but only the bytes that exist are hashed;
//   - within a piece, each 16 KiB block is hashed at its real length (a trailing
//     partial block is not zero-padded), and leaves are padded with zero hashes;
//   - every non-final piece of the torrent is padded to pieceLength/16KiB
//     leaves; the torrent's final piece is not padded at all;
//   - a single-piece file's "pieces root" is its piece root with no layer entry;
//     a longer file's root reduces its piece roots with HashForPiecePad and its
//     piece roots go in the top-level "piece layers" table.

// pieceRootFor computes one piece root the way the library does.
func pieceRootFor(span []byte, padTo int) [32]byte {
	var leaves [][32]byte
	for off := 0; off < len(span); off += merkle.BlockSize {
		end := off + merkle.BlockSize
		if end > len(span) {
			end = len(span)
		}
		leaves = append(leaves, sha256.Sum256(span[off:end]))
	}
	for len(leaves) < padTo {
		leaves = append(leaves, [32]byte{})
	}
	return merkle.RootWithPadHash(leaves, [32]byte{})
}

// v2Layout builds the file tree and piece layers for a v2-only torrent.
func v2Layout(pieceLen int64, files map[string][]byte) (metainfo.FileTree, map[string]string) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	aligned := func(n string) int64 {
		l := int64(len(files[n]))
		return (l + pieceLen - 1) / pieceLen * pieceLen
	}
	var torrentEnd, off int64
	for _, n := range names {
		if e := off + int64(len(files[n])); e > torrentEnd {
			torrentEnd = e
		}
		off += aligned(n)
	}

	layers := map[string]string{}
	dir := map[string]metainfo.FileTree{}
	offset := int64(0)
	for _, name := range names {
		data := files[name]
		var roots [][32]byte
		layer := ""
		for off := int64(0); off < aligned(name); off += pieceLen {
			end := off + pieceLen
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			var span []byte
			if off < int64(len(data)) {
				span = data[off:end]
			}
			pad := 0
			if lastPiece := offset+off+pieceLen >= torrentEnd; !lastPiece {
				pad = int(pieceLen / merkle.BlockSize)
			}
			root := pieceRootFor(span, pad)
			roots = append(roots, root)
			layer += string(root[:])
		}
		var fileRoot [32]byte
		if len(roots) == 1 {
			fileRoot = roots[0]
		} else {
			fileRoot = merkle.RootWithPadHash(roots, metainfo.HashForPiecePad(pieceLen))
			layers[string(fileRoot[:])] = layer
		}
		dir[name] = metainfo.FileTree{File: metainfo.FileTreeFile{
			Length: int64(len(data)), PiecesRoot: string(fileRoot[:]),
		}}
		offset += aligned(name)
	}
	return metainfo.FileTree{Dir: dir}, layers
}

// v2MetaInfo builds a v2-only metainfo. The info dict is encoded by hand because
// metainfo.Info's FileTree field only round-trips through the library's own
// marshaller.
func v2MetaInfo(t *testing.T, name string, pieceLen int64, files map[string][]byte) *metainfo.MetaInfo {
	t.Helper()
	ft, layers := v2Layout(pieceLen, files)
	ftBytes, err := ft.MarshalBencode()
	if err != nil {
		t.Fatalf("marshal file tree: %v", err)
	}
	infoBytes := bencode.MustMarshal(map[string]bencode.Bytes{
		"piece length": bencode.Bytes(bencode.MustMarshal(pieceLen)),
		"meta version": bencode.Bytes(bencode.MustMarshal(int64(2))),
		"name":         bencode.Bytes(bencode.MustMarshal(name)),
		"file tree":    bencode.Bytes(ftBytes),
	})
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes, PieceLayers: layers}
	mi.SetDefaults()
	return mi
}

// hybridMetaInfo builds a torrent that carries both structures. Every file must
// be a whole number of pieces so the v1 (continuous) and v2 (piece-aligned)
// layouts hash the same bytes; the builder enforces that.
func hybridMetaInfo(t *testing.T, name string, pieceLen int64, files map[string][]byte) *metainfo.MetaInfo {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var concatenated []byte
	fileInfos := make([]metainfo.FileInfo, 0, len(names))
	for _, n := range names {
		data := files[n]
		if int64(len(data))%pieceLen != 0 {
			t.Fatalf("hybrid fixture file %s is %d bytes, not a multiple of the piece length %d", n, len(data), pieceLen)
		}
		concatenated = append(concatenated, data...)
		fileInfos = append(fileInfos, metainfo.FileInfo{Length: int64(len(data)), Path: []string{n}})
	}

	var pieces []byte
	for off := int64(0); off < int64(len(concatenated)); off += pieceLen {
		sum := sha1.Sum(concatenated[off : off+pieceLen])
		pieces = append(pieces, sum[:]...)
	}

	ft, layers := v2Layout(pieceLen, files)
	ftBytes, err := ft.MarshalBencode()
	if err != nil {
		t.Fatalf("marshal file tree: %v", err)
	}
	infoBytes := bencode.MustMarshal(map[string]bencode.Bytes{
		"piece length": bencode.Bytes(bencode.MustMarshal(pieceLen)),
		"meta version": bencode.Bytes(bencode.MustMarshal(int64(2))),
		"name":         bencode.Bytes(bencode.MustMarshal(name)),
		"pieces":       bencode.Bytes(bencode.MustMarshal(pieces)),
		"files":        bencode.Bytes(bencode.MustMarshal(fileInfos)),
		"file tree":    bencode.Bytes(ftBytes),
	})
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes, PieceLayers: layers}
	mi.SetDefaults()
	return mi
}

// writeTorrentFile writes metainfo to a temp file and returns its path.
func writeTorrentFile(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fixture.torrent")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return path
}

// newMetaSeeder serves an arbitrary metainfo over loopback, so v2 and hybrid
// fixtures can be seeded without the library's v1-only builder.
func newMetaSeeder(t *testing.T, name string, files map[string][]byte, mi *metainfo.MetaInfo, rateLimit int64) *seeder {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := hermeticConfig(root, true)
	cfg.Seed = true
	if rateLimit > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(rateLimit), 64<<10)
	}
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("seeder client: %v", err)
	}
	spec := at.TorrentSpecFromMetaInfo(mi)
	tor, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		t.Fatalf("seed torrent: %v", err)
	}
	<-tor.GotInfo()
	if err := tor.VerifyData(); err != nil {
		t.Fatalf("seed verify: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !tor.Complete().Bool() {
		if time.Now().After(deadline) {
			t.Fatal("seeder did not reach complete")
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := &seeder{t: t, client: client, torrent: tor, root: root, name: name, port: client.LocalPort(), mi: mi}
	t.Cleanup(func() { client.Close() })
	return s
}

// TestV2OnlyInspectIsOffline: a v2-only .torrent is described from its own bytes,
// with no peer, no client and no writes.
func TestV2OnlyInspectIsOffline(t *testing.T) {
	a := patterned(40000, 'a')
	b := patterned(8000, 'b')
	mi := v2MetaInfo(t, "fixture", 32*1024, map[string][]byte{"a.bin": a, "b.bin": b})
	path := writeTorrentFile(t, mi)

	v2h := infohash_v2.HashBytes(mi.InfoBytes)
	wantV2 := v2h.HexString()
	info, err := Inspect(context.Background(), path, InspectConfig{Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.InfoHash != "" {
		t.Errorf("v2-only torrent reported a v1 infohash %q; the SHA-1 of its info bytes is not an identity", info.InfoHash)
	}
	if info.InfoHashV2 != wantV2 {
		t.Errorf("infohash_v2 = %s, want %s", info.InfoHashV2, wantV2)
	}
	if info.Source != "" {
		t.Errorf("v2-only torrent recorded a source %q; btmh magnets are out of scope", info.Source)
	}
	if len(info.Files) != 2 || info.Files[0].Path != "fixture/a.bin" || info.Files[0].Length != 40000 {
		t.Fatalf("files = %+v", info.Files)
	}
}

// TestV2OnlyFetchInterruptRestart is the v2 acceptance path: inspect offline,
// fetch one file from a loopback seeder, interrupt mid-transfer, reopen from the
// persisted metainfo (piece layers included), and finish.
func TestV2OnlyFetchInterruptRestart(t *testing.T) {
	a := patterned(256<<10, 'a') // 8 pieces at 32 KiB
	b := patterned(64<<10, 'b')
	files := map[string][]byte{"a.bin": a, "b.bin": b}
	mi := v2MetaInfo(t, "fixture", 32*1024, files)
	path := writeTorrentFile(t, mi)
	v2h := infohash_v2.HashBytes(mi.InfoBytes)
	wantV2 := v2h.HexString()
	sel := newMetaSeeder(t, "fixture", files, mi, 128<<10)

	// Offline inspection of the same file the fetch will use.
	offline, err := Inspect(context.Background(), path, InspectConfig{Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if offline.InfoHashV2 != wantV2 {
		t.Fatalf("inspect identity = %s", offline.InfoHashV2)
	}

	staging := t.TempDir()
	cfg := testConfig(t, staging, sel.peer())
	ctx, cancel := context.WithCancel(context.Background())
	cfg.OnUpdate = func(u Update) {
		if u.Written >= 96<<10 {
			cancel()
		}
	}
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := sess.Fetch(ctx, path, []string{"fixture/a.bin"}); err == nil {
		t.Fatal("interrupted v2 fetch reported success")
	}
	cancel()
	if err := sess.Close(); err != nil {
		t.Fatalf("close interrupted session: %v", err)
	}

	job := jobRecordFor(t, staging, wantV2)
	if job.JobID != "bt2-"+wantV2 {
		t.Errorf("job id = %q, want bt2-<v2>", job.JobID)
	}
	if job.InfoHash != "" || job.InfoHashV2 != wantV2 {
		t.Errorf("job identity fields = %q / %q", job.InfoHash, job.InfoHashV2)
	}
	if job.State.Payload != PayloadPending {
		t.Errorf("payload state after interruption = %q", job.State.Payload)
	}
	// The persisted metainfo must carry the piece layers, or the restart could
	// neither verify what it has nor fetch what it is missing.
	persisted, err := metainfo.LoadFromFile(filepath.Join(staging, ".provenance", "torrent", wantV2+".torrent"))
	if err != nil {
		t.Fatalf("persisted metainfo: %v", err)
	}
	if len(persisted.PieceLayers) == 0 {
		t.Fatal("persisted v2 metainfo lost its piece layers")
	}

	// Restart: a new session, no peer needed to reopen and verify.
	sess2 := newTestSession(t, staging, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := sess2.Fetch(ctx2, path, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("restart fetch: %v", err)
	}
	if len(cap.Assets) != 1 || cap.Assets[0].SHA256 != sha256Hex(a) {
		t.Fatalf("capture = %+v, want a.bin at %s", cap.Assets, sha256Hex(a))
	}
	if cap.InfoHash != "" || cap.InfoHashV2 != wantV2 {
		t.Errorf("capture identity = %q / %q", cap.InfoHash, cap.InfoHashV2)
	}
	if cap.BytesRead <= 0 {
		t.Error("restart read nothing; the interrupted payload was not resumed")
	}
	if cap.BytesRead >= int64(len(a)) {
		t.Errorf("restart read %d bytes of a %d byte selection; verified pieces were not reused", cap.BytesRead, len(a))
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests = %d, want 1", n)
	}

	// Identity survives status.
	status, err := ReadJobStatus(staging, "bt2-"+wantV2)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.JobID != "bt2-"+wantV2 || status.InfoHashV2 != wantV2 || status.InfoHash != "" {
		t.Errorf("status identity = %+v", status)
	}
}

// TestHybridIdentityCanonicalJob: a hybrid keeps the v1 job id and capture key,
// resolves by its full v2 hash, and never forks into a second job.
func TestHybridIdentityCanonicalJob(t *testing.T) {
	// Whole-piece files keep the v1 and v2 layouts identical.
	a := patterned(64<<10, 'a')
	b := patterned(32<<10, 'b')
	files := map[string][]byte{"a.bin": a, "b.bin": b}
	mi := hybridMetaInfo(t, "fixture", 32*1024, files)

	// Independently computed identities over the exact info bytes.
	wantV1 := fmt.Sprintf("%x", sha1.Sum(mi.InfoBytes))
	wantV2 := fmt.Sprintf("%x", sha256.Sum256(mi.InfoBytes))
	if got := metainfo.HashBytes(mi.InfoBytes).HexString(); got != wantV1 {
		t.Fatalf("library v1 hash %s != sha1 %s", got, wantV1)
	}
	v2got := infohash_v2.HashBytes(mi.InfoBytes)
	if got := v2got.HexString(); got != wantV2 {
		t.Fatalf("library v2 hash %s != sha256 %s", got, wantV2)
	}

	path := writeTorrentFile(t, mi)
	info, err := Inspect(context.Background(), path, InspectConfig{Discovery: DiscoveryDirect})
	if err != nil {
		t.Fatalf("inspect hybrid: %v", err)
	}
	if info.InfoHash != wantV1 || info.InfoHashV2 != wantV2 {
		t.Fatalf("hybrid identity = %q / %q, want %s / %s", info.InfoHash, info.InfoHashV2, wantV1, wantV2)
	}

	sel := newMetaSeeder(t, "fixture", files, mi, 0)
	staging := t.TempDir()
	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	cap, err := sess.Fetch(ctx, path, []string{"fixture/a.bin"})
	cancel()
	if err != nil {
		t.Fatalf("hybrid fetch: %v", err)
	}
	if cap.JobID != "bt-"+wantV1 {
		t.Errorf("job id = %q, want the v1 id", cap.JobID)
	}
	if cap.InfoHash != wantV1 || cap.InfoHashV2 != wantV2 {
		t.Errorf("capture identity = %q / %q", cap.InfoHash, cap.InfoHashV2)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Fatalf("run manifests = %d, want 1", n)
	}

	// Alias lookup by the full v2 hash resolves the canonical job, read-only.
	before := runManifestDigests(t, staging)
	byV1, err := ReadJobStatus(staging, "bt-"+wantV1)
	if err != nil {
		t.Fatalf("status by v1: %v", err)
	}
	byV2, err := ReadJobStatus(staging, wantV2)
	if err != nil {
		t.Fatalf("status by v2 alias: %v", err)
	}
	if byV2.JobID != byV1.JobID || byV2.Capture == nil || byV1.Capture == nil || byV2.Capture.CaptureKey != byV1.Capture.CaptureKey {
		t.Errorf("alias resolved to a different job: %+v vs %+v", byV1, byV2)
	}
	if diff := diffSnapshots(before, runManifestDigests(t, staging)); diff != "" {
		t.Errorf("alias lookup changed publication state:\n%s", diff)
	}
	if files, err := os.ReadDir(filepath.Join(staging, ".provenance", "torrent")); err == nil {
		if len(files) != 3 { // metainfo + job record + lock
			var names []string
			for _, f := range files {
				names = append(names, f.Name())
			}
			t.Errorf("alias lookup left %v, want exactly metainfo, job record and lock", names)
		}
	}

	// Restart: one job, one capture, no duplicate.
	sess2 := newTestSession(t, staging, sel.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	again, err := sess2.Fetch(ctx2, path, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("hybrid restart: %v", err)
	}
	if !again.Reconciled || again.CaptureKey != cap.CaptureKey {
		t.Errorf("restart did not reconcile the same capture: %+v", again)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests after restart = %d, want 1", n)
	}

	// The v2-only identifier form resolves to the same hybrid job: one job, one
	// identity alias, nothing created.
	alias, err := ReadJobStatus(staging, "bt2-"+wantV2)
	if err != nil {
		t.Fatalf("status by bt2-<v2> alias: %v", err)
	}
	if alias.JobID != byV1.JobID || alias.InfoHashV2 != wantV2 || alias.InfoHash != wantV1 {
		t.Errorf("bt2 alias resolved to %+v, want the hybrid job", alias)
	}
	if diff := diffSnapshots(before, runManifestDigests(t, staging)); diff != "" {
		t.Errorf("bt2 alias lookup changed publication state:\n%s", diff)
	}

	// A mismatched identity cannot be adopted: flip one byte of the v2 hash.
	mismatched := []byte(wantV2)
	if mismatched[0] == 'a' {
		mismatched[0] = 'b'
	} else {
		mismatched[0] = 'a'
	}
	if _, err := ReadJobStatus(staging, "bt2-"+string(mismatched)); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("a mismatched v2 identity resolved: %v", err)
	}
}

// TestV2MissingPieceLayersFailsClosed: without piece layers a v2 torrent cannot
// be verified, so the fetch must fail diagnosably and publish nothing.
func TestV2MissingPieceLayersFailsClosed(t *testing.T) {
	a := patterned(96<<10, 'a') // 3 pieces, so a layer entry is required
	files := map[string][]byte{"a.bin": a}
	mi := v2MetaInfo(t, "fixture", 32*1024, files)
	// A torrent file that lost its piece layers: the info dict still names the
	// file tree, but no piece hash is available for verification.
	mi.PieceLayers = nil
	path := writeTorrentFile(t, mi)
	miWithLayers := v2MetaInfo(t, "fixture", 32*1024, files)
	sel := newMetaSeeder(t, "fixture", files, miWithLayers, 0)

	staging := t.TempDir()
	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := sess.Fetch(ctx, path, []string{"fixture/a.bin"})
	if err == nil {
		t.Fatal("fetch succeeded without piece layers")
	}
	if !errors.Is(err, ErrUnsupportedIdentity) {
		t.Errorf("error = %v, want ErrUnsupportedIdentity naming the missing v2 hash", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("piece layers")) {
		t.Errorf("error does not explain the missing piece hashes: %v", err)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("run manifests = %d, want 0", n)
	}
	missingID := infohash_v2.HashBytes(mi.InfoBytes)
	if job := jobRecordFor(t, staging, "bt2-"+missingID.HexString()); job.State.Payload == PayloadReady {
		t.Errorf("unverifiable pieces were marked ready: %+v", job.State)
	}
}

// TestV2IdentityConflictRejected: two different v2 torrents with the same name in
// one staging root would share the library's payload directory, so the second is
// refused instead of silently reusing bytes.
func TestV2IdentityConflictRejected(t *testing.T) {
	a := patterned(64<<10, 'a')
	b := patterned(32<<10, 'b')
	files := map[string][]byte{"a.bin": a, "b.bin": b}
	first := v2MetaInfo(t, "fixture", 32*1024, files)
	second := v2MetaInfo(t, "fixture", 32*1024, map[string][]byte{"a.bin": b, "b.bin": a})

	selFirst := newMetaSeeder(t, "fixture", files, first, 0)
	staging := t.TempDir()
	sess := newTestSession(t, staging, selFirst.peer())
	ctx, cancel := waitCtx(t)
	if _, err := sess.Fetch(ctx, writeTorrentFile(t, first), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	cancel()
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	selSecond := newMetaSeeder(t, "fixture", map[string][]byte{"a.bin": b, "b.bin": a}, second, 0)
	sess2 := newTestSession(t, staging, selSecond.peer())
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	_, err := sess2.Fetch(ctx2, writeTorrentFile(t, second), []string{"fixture/a.bin"})
	if err == nil {
		t.Fatal("a second torrent reused the same payload directory")
	}
	if !errors.Is(err, ErrIdentityConflict) {
		t.Errorf("error = %v, want ErrIdentityConflict", err)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests = %d, want the original 1", n)
	}
}
