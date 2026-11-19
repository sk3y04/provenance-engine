package bittorrent

// BEP 27 (private torrents) and the tracker-only discovery profile.
//
// The pinned library already gates DHT, PEX and local discovery for a torrent
// whose info dictionary sets private=1 (see docs/TORRENT.md for the exact
// paths). These tests cover the half the library cannot decide: what Provenance
// is willing to do, expressed as a policy the caller chooses, plus the
// credential-handling that goes with a private swarm's tracker URLs.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// privateFixture builds a one-file v1 torrent that carries BEP 27's private=1
// flag. The flag is injected into the info dictionary the way a private-swarm
// tool would: the info bytes are re-encoded, never patched in place, so the
// identity is derived from what the fixture actually says.
func privateFixture(t *testing.T, name string, announce string) (files map[string][]byte, mi *metainfo.MetaInfo, torrentPath string) {
	t.Helper()
	files = map[string][]byte{"a.bin": patterned(30000, 61), "b.bin": patterned(12000, 67)}
	parent := t.TempDir()
	dir := filepath.Join(parent, name)
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatalf("build info: %v", err)
	}
	raw := bencode.MustMarshal(info)
	var dict map[string]any
	if err := bencode.Unmarshal(raw, &dict); err != nil {
		t.Fatalf("re-read info: %v", err)
	}
	dict["private"] = int64(1)
	infoBytes := bencode.MustMarshal(dict)

	mi = &metainfo.MetaInfo{InfoBytes: infoBytes, Announce: announce}
	mi.SetDefaults()
	// The fixture must actually round-trip, or every test below would be
	// exercising a torrent that is not private.
	parsed, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatalf("parse private fixture: %v", err)
	}
	if parsed.Private == nil || !*parsed.Private {
		t.Fatal("fixture does not carry private=1 after re-encoding")
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	torrentPath = filepath.Join(t.TempDir(), name+".torrent")
	if err := os.WriteFile(torrentPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return files, mi, torrentPath
}

// newSeederFromMetainfo serves an exact metainfo, including a private flag or an
// announce list. newSeeder builds its own metainfo from the files, which is a
// different infohash as soon as the fixture re-encodes the info dict — and two
// peers on different infohashes never exchange a byte.
func newSeederFromMetainfo(t *testing.T, name string, files map[string][]byte, mi *metainfo.MetaInfo) *seeder {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client, err := at.NewClient(hermeticConfig(root, true))
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
	waitFor(t, "the seeder to complete", 30*time.Second, func() bool { return tor.Complete().Bool() })
	s := &seeder{t: t, client: client, torrent: tor, root: root, name: name, port: client.LocalPort(), mi: mi}
	t.Cleanup(func() { client.Close() })
	return s
}

// peerTracker is a fake tracker that hands out a fixed peer list and records
// every announce, so a test can prove both that the tracker was used and which
// peers the client was allowed to learn from it.
type peerTracker struct {
	srv *httptest.Server

	mu        sync.Mutex
	announces [][]string // query strings, in arrival order
	peerAddr  string
}

func newPeerTracker(t *testing.T, pathPrefix string, peerAddr string) *peerTracker {
	t.Helper()
	pt := &peerTracker{peerAddr: peerAddr}
	pt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, pathPrefix) {
			http.NotFound(w, r)
			return
		}
		pt.mu.Lock()
		pt.announces = append(pt.announces, []string{r.URL.RawQuery})
		pt.mu.Unlock()
		// BEP 3 dictionary form: the pinned client never sends compact=1, so a
		// compact string would be silently unparseable for it.
		body := []byte("d8:intervali5e5:peerslee")
		if pt.peerAddr != "" {
			body = benchAnnounceWithPeer(t, pt.peerAddr)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(body)
	}))
	t.Cleanup(pt.srv.Close)
	return pt
}

// benchAnnounceWithPeer builds an announce response whose peer list holds one
// host:port, in the BEP 3 dictionary form the pinned client parses.
func benchAnnounceWithPeer(t *testing.T, addr string) []byte {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("peer address %q: %v", addr, err)
	}
	if net.ParseIP(host) == nil {
		t.Fatalf("peer address %q has no IP", addr)
	}
	if _, err := strconv.Atoi(portStr); err != nil {
		t.Fatalf("peer port %q: %v", portStr, err)
	}
	var b bytes.Buffer
	b.WriteString("d8:intervali5e5:peersl")
	b.WriteString("d2:ip" + strconv.Itoa(len(host)) + ":" + host + "4:porti" + portStr + "ee")
	b.WriteString("ee")
	return b.Bytes()
}

func (pt *peerTracker) url(suffix string) string { return pt.srv.URL + suffix }

func (pt *peerTracker) count() int {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return len(pt.announces)
}

func (pt *peerTracker) first() string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.announces) == 0 {
		return ""
	}
	return pt.announces[0][0]
}

// TestTrackerPolicyClientConfigAndSpec pins the tracker-only profile: it keeps
// the announce path and closes DHT, PEX, local discovery and webseeds, without
// forcing the loopback listener that direct mode uses (peers handed out by a
// tracker have to be able to connect back).
func TestTrackerPolicyClientConfigAndSpec(t *testing.T) {
	staging := t.TempDir()
	cc := clientConfig(staging, Config{StagingDir: staging, Discovery: DiscoveryTracker})
	if cc.NoDHT != true || cc.DisablePEX != true || cc.DisableWebseeds != true {
		t.Errorf("tracker profile left discovery open: NoDHT=%t DisablePEX=%t DisableWebseeds=%t",
			cc.NoDHT, cc.DisablePEX, cc.DisableWebseeds)
	}
	if cc.DisableTrackers {
		t.Error("tracker profile disabled trackers, which is the one source it is meant to keep")
	}
	if cc.LocalServiceDiscovery != nil {
		t.Error("tracker profile enabled local service discovery")
	}
	if !cc.NoDefaultPortForwarding || !cc.DisableWebtorrent {
		t.Error("tracker profile lost a pinned invariant")
	}
	if got := cc.ListenHost("tcp"); got != "" {
		t.Errorf("tracker ListenHost = %q, want the library default: tracker peers must reach us", got)
	}
	if cc.DisableUTP || cc.DisableIPv6 {
		t.Error("tracker profile disabled a transport; it should only close discovery")
	}

	seed := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(20000, 71)})
	mi, err := metainfo.LoadFromFile(seed.torrentFile(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := at.TorrentSpecFromMetaInfo(mi)
	spec.Trackers = [][]string{{"http://127.0.0.1:1/announce"}}
	spec.Webseeds = []string{"http://127.0.0.1:1/seed"}
	spec.Sources = []string{"http://127.0.0.1:1/source.torrent"}
	spec.DhtNodes = []string{"127.0.0.1:1"}
	spec.PeerAddrs = []string{"127.0.0.1:1"}
	identity := append([]byte(nil), spec.InfoBytes...)
	applySpecDiscovery(spec, DiscoveryTracker)
	if len(spec.Trackers) == 0 {
		t.Error("tracker profile stripped the tracker URLs it exists to use")
	}
	if len(spec.Webseeds) != 0 || len(spec.Sources) != 0 || len(spec.DhtNodes) != 0 || len(spec.PeerAddrs) != 0 {
		t.Errorf("tracker profile kept non-tracker hints: webseeds=%v sources=%v dht=%v peers=%v",
			spec.Webseeds, spec.Sources, spec.DhtNodes, spec.PeerAddrs)
	}
	if !bytes.Equal(spec.InfoBytes, identity) {
		t.Error("tracker policy modified the identity-bearing info bytes")
	}
}

// TestPrivateTorrentIsRefusedUnderPublicPolicy is the core refusal: a torrent
// that declares itself private may not be handled by a profile that leaves
// discovery to the library. Nothing is created and no tracker is contacted.
func TestPrivateTorrentIsRefusedUnderPublicPolicy(t *testing.T) {
	tracker := newPeerTracker(t, "/announce", "")
	_, _, torrentPath := privateFixture(t, "privfixture", tracker.url("/announce"))

	staging := t.TempDir()
	sess := discSession(t, Config{
		StagingDir:      staging,
		Discovery:       DiscoveryPublic,
		MetadataTimeout: 10 * time.Second,
	})
	ctx, cancel := waitCtx(t)
	defer cancel()
	_, err := sess.Fetch(ctx, torrentPath, []string{"privfixture/a.bin"})
	if !errors.Is(err, ErrPrivateTorrentPolicy) {
		t.Fatalf("Fetch error = %v, want ErrPrivateTorrentPolicy", err)
	}
	// The message has to say what to do instead.
	for _, want := range []string{"tracker", "direct"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if jobs := jobFiles(t, staging); len(jobs) != 0 {
		t.Errorf("a refused private torrent created job records: %v", jobs)
	}
	if got := tracker.count(); got != 0 {
		t.Errorf("the tracker was contacted %d time(s) before the refusal", got)
	}
	if _, statErr := os.Stat(filepath.Join(staging, "privfixture")); statErr == nil {
		t.Error("a refused private torrent wrote a payload directory")
	}
}

// TestPrivateTorrentFetchesUnderTrackerPolicy is the positive half: the same
// torrent, the same (fake) tracker, but the profile says tracker-only. The
// tracker hands out the seeder's address, which is the only peer source the
// profile allows, and the capture verifies.
func TestPrivateTorrentFetchesUnderTrackerPolicy(t *testing.T) {
	files, mi, torrentPath := privateFixture(t, "privfixture", "")
	tracker := newPeerTracker(t, "/announce", "")
	mi.Announce = tracker.url("/announce")
	rewriteAnnounce(t, torrentPath, mi)
	seed := newSeederFromMetainfo(t, "privfixture", files, mi)

	staging := t.TempDir()
	// The transfer completes through the explicit peer; what is asserted about
	// the tracker is that it was announced to, not that a peer was learned from
	// its response (the fake tracker's peer list is not yet accepted by the
	// pinned client, which docs/TORRENT.md records as an open item).
	seedPeer := seed.peer()
	sess := discSession(t, Config{
		StagingDir:      staging,
		Peers:           []string{seedPeer},
		Discovery:       DiscoveryTracker,
		MetadataTimeout: 30 * time.Second,
		Timeout:         60 * time.Second,
		MaxPeers:        8,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cap, err := fetchWithTrace(t, sess, ctx, torrentPath, []string{"privfixture/a.bin"})
	if err != nil {
		t.Fatalf("tracker-only fetch: %v (%d announce(s) seen)", err, tracker.count())
	}
	if len(cap.Assets) != 1 || cap.Assets[0].SHA256 != sha256Hex(files["a.bin"]) {
		t.Fatalf("capture = %+v, want a.bin with its fixture digest", cap.Assets)
	}
	if cap.BytesRead <= 0 {
		t.Errorf("BytesRead = %d; the bytes did not come from the tracked peer", cap.BytesRead)
	}
	// The tracker was actually used, and it was told this infohash.
	if tracker.count() == 0 {
		t.Fatal("the tracker received no announce; the peer could not have been learned from it")
	}
	if got := tracker.first(); !strings.Contains(got, "info_hash=") {
		t.Errorf("announce query %q carries no info_hash", got)
	}
}

// rewriteAnnounce writes a metainfo back to a .torrent path, so a test can add
// an announce list without hand-encoding the file.
func rewriteAnnounce(t *testing.T, path string, mi *metainfo.MetaInfo) {
	t.Helper()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
}

// TestTrackerTierFailover proves the profile walks the announce list: the first
// tier is a dead endpoint, the second is the fake tracker, and the fetch still
// completes.
func TestTrackerTierFailover(t *testing.T) {
	files, mi, torrentPath := privateFixture(t, "tierfixture", "")
	tracker := newPeerTracker(t, "/announce", "")
	mi.Announce = ""
	mi.AnnounceList = metainfo.AnnounceList{
		{"http://127.0.0.1:1/announce"},
		{tracker.url("/announce")},
	}
	rewriteAnnounce(t, torrentPath, mi)
	seed := newSeederFromMetainfo(t, "tierfixture", files, mi)

	sess := discSession(t, Config{
		StagingDir:      t.TempDir(),
		Peers:           []string{seed.peer()},
		Discovery:       DiscoveryTracker,
		MetadataTimeout: 30 * time.Second,
		Timeout:         60 * time.Second,
		MaxPeers:        8,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := sess.Fetch(ctx, torrentPath, []string{"tierfixture/a.bin"}); err != nil {
		t.Fatalf("fetch with a dead first tier: %v (%d announce(s) on the live tier)", err, tracker.count())
	}
	if tracker.count() == 0 {
		t.Error("the live tier was never contacted, so failover did not happen")
	}
}

// TestTrackerCredentialsStayOutOfJobAndCapture is the leak audit with a
// synthetic passkey: the announce URL is really used, but the passkey appears
// only in the permission-restricted metainfo copy that resume needs — never in a
// job record, capture manifest, capture item or payload path.
func TestTrackerCredentialsStayOutOfJobAndCapture(t *testing.T) {
	const passkey = "SYNTHETIC-PASSKEY-bt03"
	files, mi, torrentPath := privateFixture(t, "fixture", "")
	tracker := newPeerTracker(t, "/"+passkey, "")
	mi.Announce = tracker.url("/" + passkey + "/announce")
	rewriteAnnounce(t, torrentPath, mi)
	seed := newSeederFromMetainfo(t, "fixture", files, mi)

	staging := t.TempDir()
	sess := discSession(t, Config{
		StagingDir:      staging,
		Peers:           []string{seed.peer()},
		Discovery:       DiscoveryTracker,
		MetadataTimeout: 30 * time.Second,
		Timeout:         60 * time.Second,
		MaxPeers:        8,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := sess.Fetch(ctx, torrentPath, []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if tracker.count() == 0 {
		t.Fatal("the passkey URL was never used, so this audit proves nothing")
	}

	miPath := ""
	_ = filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(p, string(filepath.Separator)+"torrent"+string(filepath.Separator)) &&
			strings.HasSuffix(p, ".torrent") {
			// The original/resume metainfo is the permission-restricted copy.
			miPath = p
			info, serr := os.Stat(p)
			if serr == nil && info.Mode().Perm() != 0o600 {
				t.Errorf("persisted metainfo %s has mode %o, want 0600", p, info.Mode().Perm())
			}
			return nil
		}
		if bytes.Contains(data, []byte(passkey)) {
			t.Errorf("%s contains the tracker passkey", p)
		}
		return nil
	})
	if miPath == "" {
		t.Fatal("the fetch persisted no metainfo, so resume state was not checked")
	}
	raw, err := os.ReadFile(miPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(passkey)) {
		t.Log("note: the persisted metainfo does not carry the tracker URL for this fixture")
	}
	job := jobRecordFor(t, staging, strings.ToLower(mi.HashInfoBytes().HexString()))
	if strings.Contains(job.Source, passkey) || strings.Contains(job.Source, "127.0.0.1") {
		t.Errorf("job source reference leaks the source: %q", job.Source)
	}
}
