package bittorrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// seeder is an in-process BitTorrent peer serving a fixture directory over
// loopback. No DHT, tracker, or PEX: the downloader learns the peer address
// explicitly, so the tests never touch a public network.
type seeder struct {
	t       *testing.T
	client  *at.Client
	torrent *at.Torrent
	root    string
	name    string
	port    int
	mi      *metainfo.MetaInfo
}

func hermeticConfig(dataDir string, seed bool) *at.ClientConfig {
	cfg := at.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.Seed = seed
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.NoDefaultPortForwarding = true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	return cfg
}

// newSeeder writes name/files under a temp root, builds metainfo over that
// directory, and serves it until the test ends.
func newSeeder(t *testing.T, name string, pieceLen int64, files map[string][]byte) *seeder {
	t.Helper()
	return newSeederRateLimited(t, name, pieceLen, files, 0)
}

// newSeederRateLimited is newSeeder with an optional upload cap (bytes per
// second). Tests that must observe a transfer mid-flight use the cap so the
// observation does not race the loopback transfer.
func newSeederRateLimited(t *testing.T, name string, pieceLen int64, files map[string][]byte, uploadBytesPerSecond int64) *seeder {
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
	info := metainfo.Info{PieceLength: pieceLen}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatalf("build metainfo: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	mi.SetDefaults()

	cfg := hermeticConfig(root, true)
	if uploadBytesPerSecond > 0 {
		// The upload limiter is what makes an in-flight observation
		// deterministic: a loopback seeder otherwise finishes a small fixture
		// before the test can see it start.
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(uploadBytesPerSecond), 64<<10)
	}
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("seeder client: %v", err)
	}
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("seed torrent: %v", err)
	}
	<-tor.GotInfo()
	if err := tor.VerifyData(); err != nil {
		t.Fatalf("seed verify: %v", err)
	}
	// Verification marks pieces asynchronously; poll instead of asserting on
	// the return value alone.
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

func (s *seeder) magnet() string {
	return "magnet:?xt=urn:btih:" + s.mi.HashInfoBytes().HexString()
}

func (s *seeder) peer() string {
	return peerAddr{host: "127.0.0.1", port: s.port}.String()
}

func (s *seeder) infohash() string { return s.mi.HashInfoBytes().HexString() }

func (s *seeder) torrentFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.torrent")
	var buf bytes.Buffer
	if err := s.mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// testConfig is the hermetic downloader configuration: loopback, explicit
// peers, no discovery.
func testConfig(t *testing.T, staging string, peers ...string) Config {
	t.Helper()
	return Config{
		StagingDir:           staging,
		Peers:                peers,
		DisablePeerDiscovery: true,
		MetadataTimeout:      20 * time.Second,
		MaxPeers:             4,
	}
}

func newTestSession(t *testing.T, staging string, peers ...string) *Session {
	t.Helper()
	sess, err := New(testConfig(t, staging, peers...))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// patterned returns n bytes that are unlikely to collide across fixtures.
func patterned(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(int(seed) + i*7%251)
	}
	return b
}

func listPartFiles(t *testing.T, staging string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".part" {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// captureManifests reads every capture manifest a job wrote. One fetch writes
// one manifest, so a test that resumes expects more than one.
func captureManifests(t *testing.T, staging string) []*manifest.CaptureManifest {
	t.Helper()
	runs, err := manifest.ListRunManifests(staging)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatalf("list runs: %v", err)
	}
	out := make([]*manifest.CaptureManifest, 0, len(runs))
	for _, run := range runs {
		cm, err := manifest.ReadCaptureManifest(run)
		if err != nil {
			t.Fatalf("read capture manifest %s: %v", run, err)
		}
		out = append(out, cm)
	}
	return out
}

// waitFor polls cond until it reports success or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// jobRecordFor reads the persisted job record from a staging root.
func jobRecordFor(t *testing.T, staging, infohash string) *jobRecord {
	t.Helper()
	path := filepath.Join(staging, filepath.FromSlash(torrentStateDir), jobID(infohash)+".job.json")
	job, err := readJob(path)
	if err != nil {
		t.Fatalf("read job record: %v", err)
	}
	return job
}

// manifestsWithKey returns the capture manifests carrying an idempotency key.
func manifestsWithKey(t *testing.T, staging, key string) []*manifest.CaptureManifest {
	t.Helper()
	var out []*manifest.CaptureManifest
	for _, cm := range captureManifests(t, staging) {
		if cm.CaptureKey == key {
			out = append(out, cm)
		}
	}
	return out
}

// waitCtx returns a context bounded well under the package defaults so a
// protocol regression fails the test instead of hanging it.
func waitCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 60*time.Second)
}
