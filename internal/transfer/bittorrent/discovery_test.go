package bittorrent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// Discovery profiles decide which network branches a run may use. The tests
// below pin three separate things:
//
//   - the mapping from a profile name onto the library client config and onto
//     the TorrentSpec it is given (where trackers, webseeds, metainfo sources,
//     magnet-supplied peer addresses and DHT nodes live);
//   - the observable difference at runtime: a direct client opens no UDP
//     socket, listens on loopback only, and never dials anything discovery
//     could have produced;
//   - the tripwires those assertions depend on, armed with a real announce and
//     a real webseed URL, so a quiet tracker is evidence rather than an
//     assumption.

// trackerTripwire is a local HTTP endpoint standing in for a tracker announce
// URL. It counts requests and answers with a valid, empty announce response so
// a client that does contact it is not erroring out for other reasons.
type trackerTripwire struct {
	srv  *httptest.Server
	hits int64
	// announced keeps the first request path so a failure message can say what
	// the client actually asked for.
	announced atomic.Value
}

func newTrackerTripwire(t *testing.T) *trackerTripwire {
	t.Helper()
	tw := &trackerTripwire{}
	tw.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&tw.hits, 1)
		tw.announced.CompareAndSwap(nil, r.URL.String())
		w.WriteHeader(http.StatusOK)
		// Bencode dict with sorted keys: interval, peers.
		_, _ = w.Write([]byte("d8:intervali1800e5:peers0:e"))
	}))
	t.Cleanup(tw.srv.Close)
	return tw
}

func (tw *trackerTripwire) announceURL() string { return tw.srv.URL + "/announce" }

func (tw *trackerTripwire) count() int64 { return atomic.LoadInt64(&tw.hits) }

func (tw *trackerTripwire) String() string {
	if v, ok := tw.announced.Load().(string); ok {
		return v
	}
	return ""
}

// waitNoHits is a bounded negative observation: it fails as soon as a hit
// arrives and otherwise returns after the window. A negative assertion needs a
// window; it does not need a sleep long enough to look like one.
func waitNoHits(t *testing.T, what string, window time.Duration, count func() int64) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if n := count(); n != 0 {
			t.Fatalf("%s: received %d request(s) (first %s); none was expected", what, n, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParseDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want Discovery
	}{
		{"", DiscoveryPublic},
		{"public", DiscoveryPublic},
		{" direct ", DiscoveryDirect},
		{"DIRECT", DiscoveryDirect},
		{"Public", DiscoveryPublic},
	} {
		got, err := ParseDiscovery(tc.in)
		if err != nil {
			t.Errorf("ParseDiscovery(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDiscovery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"swarm", "offline", "public,direct", "none", "tracker"} {
		if _, err := ParseDiscovery(bad); err == nil {
			t.Errorf("ParseDiscovery(%q) accepted an unknown profile", bad)
		} else if !errors.Is(err, ErrInvalidDiscovery) {
			t.Errorf("ParseDiscovery(%q) error = %v, want ErrInvalidDiscovery", bad, err)
		}
	}
	// The zero value is public, and the printed form is never empty.
	if Discovery("").String() != "public" {
		t.Errorf("zero Discovery prints %q, want public", Discovery("").String())
	}
	if Discovery("").IsDirect() {
		t.Error("zero Discovery claimed to be direct")
	}
}

// TestClientConfigDiscoveryPolicy pins what each profile does to the library
// client config. This is the "before construction" half: every switch below is
// set on the config New hands to at.NewClient, so no listener or discoverer can
// exist that the policy did not allow.
func TestClientConfigDiscoveryPolicy(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	pub := clientConfig(staging, Config{StagingDir: staging})
	if pub.DataDir != staging {
		t.Errorf("DataDir = %q, want the staging root %q", pub.DataDir, staging)
	}
	if pub.NoDHT || pub.DisableTrackers || pub.DisablePEX {
		t.Errorf("public discovery profile disabled a discovery branch: NoDHT=%t DisableTrackers=%t DisablePEX=%t",
			pub.NoDHT, pub.DisableTrackers, pub.DisablePEX)
	}
	if got := pub.ListenHost("tcp"); got != "" {
		t.Errorf("public ListenHost = %q, want the library default (all interfaces)", got)
	}
	if pub.LocalServiceDiscovery != nil {
		t.Error("public profile enabled local service discovery; it is opt-in and Provenance never sets it")
	}
	// Invariants that hold in both profiles, because they are never wanted.
	if !pub.DisableWebseeds || !pub.DisableWebtorrent || !pub.NoDefaultPortForwarding {
		t.Errorf("public profile lost a pinned invariant: webseeds=%t webtorrent=%t port-forwarding=%t",
			!pub.DisableWebseeds, !pub.DisableWebtorrent, !pub.NoDefaultPortForwarding)
	}

	direct := clientConfig(staging, Config{StagingDir: staging, Discovery: DiscoveryDirect})
	// Reported by what the switch protects, not by the switch's name: a false
	// value here means the branch is still open.
	for name, enabled := range map[string]bool{
		"DHT":                       !direct.NoDHT,
		"tracker announces/scrapes": !direct.DisableTrackers,
		"PEX":                       !direct.DisablePEX,
		"webseeds":                  !direct.DisableWebseeds,
		"WebTorrent":                !direct.DisableWebtorrent,
		"uTP":                       !direct.DisableUTP,
		"IPv6":                      !direct.DisableIPv6,
		"UPnP/NAT-PMP port mapping": !direct.NoDefaultPortForwarding,
	} {
		if enabled {
			t.Errorf("direct profile left %s enabled", name)
		}
	}
	if got := direct.ListenHost("tcp"); got != directListenHost {
		t.Errorf("direct ListenHost(tcp) = %q, want %q", got, directListenHost)
	}
	if got := direct.ListenHost("udp"); got != directListenHost {
		t.Errorf("direct ListenHost(udp) = %q, want %q", got, directListenHost)
	}
	if direct.LocalServiceDiscovery != nil {
		t.Error("direct profile enabled local service discovery")
	}
	if direct.ListenPort != 0 || direct.Seed {
		t.Errorf("direct profile changed unrelated defaults: ListenPort=%d Seed=%t", direct.ListenPort, direct.Seed)
	}
}

// TestSpecDiscoveryPolicy pins the per-torrent half: a direct run must not take
// tracker URLs, webseed URLs, metainfo sources, magnet-supplied peer addresses
// or DHT node hints from the source, because the library would dial or fetch
// them. The identity bytes must come out untouched.
func TestSpecDiscoveryPolicy(t *testing.T) {
	t.Parallel()
	s := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(40000, 11)})
	src := s.torrentFile(t)
	mi, err := metainfo.LoadFromFile(src)
	if err != nil {
		t.Fatal(err)
	}
	build := func() *at.TorrentSpec {
		spec := at.TorrentSpecFromMetaInfo(mi)
		spec.Trackers = [][]string{{"http://127.0.0.1:1/announce"}}
		spec.Webseeds = []string{"http://127.0.0.1:1/seed"}
		spec.Sources = []string{"http://127.0.0.1:1/source.torrent"}
		spec.DhtNodes = []string{"127.0.0.1:1"}
		spec.PeerAddrs = []string{"127.0.0.1:1"}
		return spec
	}

	pub := build()
	identity := append([]byte(nil), pub.InfoBytes...)
	applySpecDiscovery(pub, DiscoveryPublic)
	if len(pub.Trackers) == 0 || len(pub.Webseeds) == 0 || len(pub.Sources) == 0 ||
		len(pub.DhtNodes) == 0 || len(pub.PeerAddrs) == 0 {
		t.Fatalf("public profile dropped source hints: %+v", pub)
	}

	direct := build()
	applySpecDiscovery(direct, DiscoveryDirect)
	if len(direct.Trackers) != 0 || len(direct.Webseeds) != 0 || len(direct.Sources) != 0 ||
		len(direct.DhtNodes) != 0 || len(direct.PeerAddrs) != 0 {
		t.Errorf("direct profile kept source hints: trackers=%v webseeds=%v sources=%v dht=%v peers=%v",
			direct.Trackers, direct.Webseeds, direct.Sources, direct.DhtNodes, direct.PeerAddrs)
	}
	if string(direct.InfoBytes) != string(identity) {
		t.Error("applySpecDiscovery modified the identity-bearing info bytes")
	}
	if direct.InfoHash != pub.InfoHash || direct.InfoHashV2 != pub.InfoHashV2 {
		t.Error("applySpecDiscovery modified the torrent identity")
	}
	if len(direct.PieceLayers) != len(pub.PieceLayers) {
		t.Error("applySpecDiscovery modified the piece layers")
	}
}

// TestDirectPolicyAdmitsNoUDPAndBindsLoopback checks the runtime consequence of
// the config: with DHT, uTP and IPv6 off, the client has no UDP socket at all,
// and every listener is loopback. The public client is the control — if the
// assertion could not tell the two apart, it proves nothing.
func TestDirectPolicyAdmitsNoUDPAndBindsLoopback(t *testing.T) {
	staging := t.TempDir()
	pubClient, err := at.NewClient(clientConfig(t.TempDir(), Config{StagingDir: staging}))
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	defer func() { _ = pubClient.Close() }()
	// No torrent is added to the control client, so nothing is announced and no
	// bootstrap node is looked up: this only observes the sockets it opened.
	var pubUDP, pubPublic int
	for _, l := range pubClient.Listeners() {
		if strings.HasPrefix(l.Addr().Network(), "udp") {
			pubUDP++
		}
		if ip := listenerIP(l.Addr()); ip != nil && !ip.IsLoopback() {
			pubPublic++
		}
	}
	if pubUDP == 0 {
		t.Fatalf("control: public client opened no UDP socket, so the direct assertion cannot discriminate (listeners: %v)", listenerAddrs(pubClient))
	}
	if pubPublic == 0 {
		t.Fatalf("control: public client bound no non-loopback listener, so the direct assertion cannot discriminate (listeners: %v)", listenerAddrs(pubClient))
	}

	sess := discSession(t, Config{StagingDir: t.TempDir(), Discovery: DiscoveryDirect})
	listeners := sess.client.Listeners()
	if len(listeners) == 0 {
		t.Fatal("direct client opened no listeners at all")
	}
	for _, l := range listeners {
		addr := l.Addr()
		if strings.HasPrefix(addr.Network(), "udp") {
			t.Errorf("direct client opened a UDP socket at %s (network %s)", addr, addr.Network())
		}
		ip := listenerIP(addr)
		if ip == nil {
			t.Errorf("direct listener %s has no IP address", addr)
			continue
		}
		if !ip.IsLoopback() {
			t.Errorf("direct listener bound %s, which is not loopback", addr)
		}
	}
}

// TestTrackerTripwireIsArmed is the control for every "no tracker contact"
// assertion below: with trackers enabled the library announces promptly, so a
// zero count in direct mode is the policy working rather than an idle client.
func TestTrackerTripwireIsArmed(t *testing.T) {
	tw := newTrackerTripwire(t)
	cc := clientConfig(t.TempDir(), Config{})
	// Keep the control hermetic: trackers on (public), DHT off so no bootstrap
	// node is looked up, loopback listener so nothing is exposed.
	cc.NoDHT = true
	cc.ListenHost = func(string) string { return "127.0.0.1" }
	cc.DisableIPv6 = true
	client, err := at.NewClient(cc)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	defer func() { _ = client.Close() }()

	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20) + "&tr=" + url.QueryEscape(tw.announceURL())
	spec, err := at.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.AddTorrentSpec(spec); err != nil {
		t.Fatalf("add magnet: %v", err)
	}
	waitFor(t, "a tracker announce from the control client", 10*time.Second, func() bool { return tw.count() > 0 })
}

// TestDirectMagnetFailsBoundedWithoutContactingTracker is the direct-mode half
// of the tracker gate: no peers are configured and discovery is off, so the
// magnet cannot resolve metadata — and the failure must come from the bounded
// metadata wait, not from a tracker or a DHT bootstrap that quietly answered.
func TestDirectMagnetFailsBoundedWithoutContactingTracker(t *testing.T) {
	tw := newTrackerTripwire(t)
	staging := t.TempDir()
	sess := discSession(t, Config{
		StagingDir:      staging,
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 400 * time.Millisecond,
	})
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20) + "&tr=" + url.QueryEscape(tw.announceURL())
	ctx, cancel := waitCtx(t)
	defer cancel()
	started := time.Now()
	_, err := sess.Fetch(ctx, magnet, []string{"fixture/a.bin"})
	if !errors.Is(err, ErrMetadataTimeout) {
		t.Fatalf("Fetch error = %v, want ErrMetadataTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("metadata wait took %s; the failure was not bounded by MetadataTimeout", elapsed)
	}
	// Nothing was created for a torrent that never resolved.
	if jobs := jobFiles(t, staging); len(jobs) != 0 {
		t.Errorf("a job record was created for an unresolved magnet: %v", jobs)
	}
	waitNoHits(t, "direct-mode tracker tripwire", 400*time.Millisecond, tw.count)
}

// TestDirectIgnoresMagnetPeerAddresses covers the BEP 9 x.pe parameter: a
// magnet can name peer addresses, and in public mode the library dials them. A
// direct run must not — the peer list is the caller's, not the source's — so
// the same magnet that would resolve instantly over x.pe must time out.
func TestDirectIgnoresMagnetPeerAddresses(t *testing.T) {
	s := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(40000, 13)})
	magnet := s.magnet() + "&x.pe=" + url.QueryEscape(s.peer())

	sess := discSession(t, Config{
		StagingDir:      t.TempDir(),
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 500 * time.Millisecond,
	})
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, magnet, []string{"fixture/a.bin"}); !errors.Is(err, ErrMetadataTimeout) {
		t.Fatalf("Fetch error = %v, want ErrMetadataTimeout (x.pe must not be dialled in direct mode)", err)
	}

	// The same magnet with the peer given explicitly resolves, which proves the
	// seeder was reachable and only the source-supplied address was ignored.
	viaFlag := discSession(t, Config{
		StagingDir:      t.TempDir(),
		Peers:           []string{s.peer()},
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		MaxPeers:        4,
	})
	ctx2, cancel2 := waitCtx(t)
	defer cancel2()
	cap, err := viaFlag.Fetch(ctx2, magnet, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("Fetch with --peer: %v", err)
	}
	if len(cap.Assets) != 1 || cap.Assets[0].Path != "fixture/a.bin" {
		t.Fatalf("captured %+v, want one fixture/a.bin asset", cap.Assets)
	}
}

// webseedTripwire is a BEP 19 webseed standing in for a source's url-list. It
// counts requests and serves byte ranges out of the fixture, so a client that
// really does use it downloads correct bytes rather than just touching it.
type webseedTripwire struct {
	srv     *httptest.Server
	payload []byte
	hits    int64
}

func newWebseedTripwire(t *testing.T, payload []byte) *webseedTripwire {
	t.Helper()
	tw := &webseedTripwire{payload: payload}
	tw.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&tw.hits, 1)
		data := tw.payload
		if rng := r.Header.Get("Range"); rng != "" {
			start, end, ok := parseByteRange(rng, int64(len(data)))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(tw.srv.Close)
	return tw
}

func (tw *webseedTripwire) count() int64 { return atomic.LoadInt64(&tw.hits) }

// parseByteRange handles the single "bytes=a-b" form the library sends.
func parseByteRange(header string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end := size - 1
	if strings.TrimSpace(b) != "" {
		if end, err = strconv.ParseInt(strings.TrimSpace(b), 10, 64); err != nil {
			return 0, 0, false
		}
	}
	if start < 0 || start >= size || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

// TestWebseedTripwireIsArmed arms the second tripwire the direct mode gate
// names: with webseeds enabled the library really fetches the payload over
// HTTP, so "no webseed request" in a direct run is a fact about the policy and
// not about a client that could never have used one.
func TestWebseedTripwireIsArmed(t *testing.T) {
	payload := patterned(60000, 29)
	tw := newWebseedTripwire(t, payload)

	// Metainfo is built over a fixture directory, but the downloading client's
	// data directory is empty, so the payload can only come from the webseed.
	fixtureDir := t.TempDir()
	name := "fixture"
	dir := filepath.Join(fixtureDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), UrlList: []string{tw.srv.URL + "/"}}

	cc := hermeticConfig(t.TempDir(), false)
	cc.DisableWebseeds = false // the point of this control: webseeds allowed
	client, err := at.NewClient(cc)
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	defer func() { _ = client.Close() }()
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("add torrent: %v", err)
	}
	<-tor.GotInfo()
	tor.Files()[0].Download()
	waitFor(t, "a webseed request from the control client", 20*time.Second, func() bool { return tw.count() > 0 })
	waitFor(t, "the control download to finish", 20*time.Second, func() bool { return tor.Complete().Bool() })
	got, err := os.ReadFile(filepath.Join(cc.DataDir, name, "a.bin"))
	if err != nil {
		t.Fatalf("read control download: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("control download from the webseed does not match the fixture")
	}
}

// TestDirectFetchNeverUsesWebseed is the direct half: the same metainfo, whose
// url-list points at the tripwire, must not cause a single webseed request, and
// the fetch must fail bounded instead of quietly completing over HTTP.
func TestDirectFetchNeverUsesWebseed(t *testing.T) {
	payload := patterned(60000, 31)
	tw := newWebseedTripwire(t, payload)

	fixtureDir := t.TempDir()
	name := "fixture"
	dir := filepath.Join(fixtureDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), UrlList: []string{tw.srv.URL + "/"}}
	torrentPath := filepath.Join(t.TempDir(), "webseed.torrent")
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(torrentPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	sess := discSession(t, Config{
		StagingDir:      t.TempDir(),
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		Timeout:         1500 * time.Millisecond,
	})
	ctx, cancel := waitCtx(t)
	defer cancel()
	if _, err := sess.Fetch(ctx, torrentPath, []string{"fixture/a.bin"}); err == nil {
		t.Fatal("direct fetch completed with no peer and webseeds off; it must have used the webseed")
	}
	waitNoHits(t, "direct-mode webseed tripwire", 500*time.Millisecond, tw.count)

	// The webseed URL must not have been recorded in resume state either.
	persisted, err := metainfo.LoadFromFile(sess.metainfoPath(mustIdentity(t, mi)))
	if err != nil {
		t.Fatalf("read persisted metainfo: %v", err)
	}
	if len(persisted.UrlList) != 0 {
		t.Errorf("direct run persisted webseed URLs: %v", persisted.UrlList)
	}
}

// mustIdentity derives the typed identity of a metainfo.
func mustIdentity(t *testing.T, mi *metainfo.MetaInfo) Identity {
	t.Helper()
	id, err := IdentityFromMetaInfo(mi)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDirectFetchAndRepairStayOffTracker runs the whole job lifecycle under
// direct discovery against a source that carries a tracker URL: a first fetch
// with the explicit peer, a corrupted payload, a bounded failure with no peer,
// and a repair with the peer again. The tracker is never contacted, and every
// run still reconciles to one capture.
func TestDirectFetchAndRepairStayOffTracker(t *testing.T) {
	tw := newTrackerTripwire(t)
	s := newSeeder(t, "fixture", 32<<10, map[string][]byte{"a.bin": patterned(96000, 17)})
	// The announce URL is written into the .torrent the fetch consumes, so a
	// client with trackers enabled would have something to announce to.
	s.mi.Announce = tw.announceURL()
	src := s.torrentFile(t)

	staging := t.TempDir()
	base := Config{
		StagingDir:      staging,
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		MaxPeers:        4,
	}

	first := base
	first.Peers = []string{s.peer()}
	first.Timeout = 60 * time.Second
	sess := discSession(t, first)
	ctx, cancel := waitCtx(t)
	cap1, err := sess.Fetch(ctx, src, []string{"fixture/a.bin"})
	cancel()
	if err != nil {
		t.Fatalf("first direct fetch: %v", err)
	}
	if cap1.CaptureKey == "" || len(cap1.Assets) != 1 {
		t.Fatalf("first capture = %+v, want one asset and a capture key", cap1)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close after first fetch: %v", err)
	}

	// Corrupt a byte of the finalized payload; the file is 0444.
	payload := filepath.Join(staging, "fixture", "a.bin")
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatal(err)
	}
	corruptAt(t, payload, 100)
	manifestsBefore := len(captureManifests(t, staging))

	// No peer, no discovery: bounded failure, nothing published.
	noPeer := base
	noPeer.Timeout = 1500 * time.Millisecond
	sess2 := discSession(t, noPeer)
	ctx2, cancel2 := waitCtx(t)
	_, err = sess2.Fetch(ctx2, src, []string{"fixture/a.bin"})
	cancel2()
	if err == nil {
		t.Fatal("fetch with a corrupted payload and no peer succeeded")
	}
	if got := len(captureManifests(t, staging)); got != manifestsBefore {
		t.Errorf("a failed repair wrote a capture manifest (%d -> %d)", manifestsBefore, got)
	}
	if err := sess2.Close(); err != nil {
		t.Fatalf("close after bounded failure: %v", err)
	}

	// The explicit peer repairs, reusing the same capture.
	third := base
	third.Peers = []string{s.peer()}
	third.Timeout = 60 * time.Second
	sess3 := discSession(t, third)
	ctx3, cancel3 := waitCtx(t)
	cap3, err := sess3.Fetch(ctx3, src, []string{"fixture/a.bin"})
	cancel3()
	if err != nil {
		t.Fatalf("repair fetch: %v", err)
	}
	if cap3.CaptureKey != cap1.CaptureKey {
		t.Errorf("repair produced a new capture key %s, want %s", cap3.CaptureKey, cap1.CaptureKey)
	}
	if got := manifestsWithKey(t, staging, cap1.CaptureKey); len(got) != 1 {
		t.Errorf("capture key appears in %d manifests, want exactly 1", len(got))
	}
	if cap3.BytesRead <= 0 {
		t.Errorf("BytesRead = %d after a repair, want a nonzero measured repair", cap3.BytesRead)
	}
	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := patterned(96000, 17)
	if string(got) != string(want) {
		t.Error("repaired payload does not match the fixture")
	}
	waitNoHits(t, "direct-mode tracker tripwire", 500*time.Millisecond, tw.count)
}

// TestOpenAppliesDiscoveryPolicyToResumeState pins the resume path: open() is
// what a restart, a repair and a metadata-only fetch all go through, and the
// metainfo it persists is the state the next run re-reads. In direct mode the
// tracker from the source must be gone from both; in public mode it survives
// (the control that the assertion is looking at something real).
func TestOpenAppliesDiscoveryPolicyToResumeState(t *testing.T) {
	tw := newTrackerTripwire(t)
	s := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(20000, 19)})
	s.mi.Announce = tw.announceURL()
	srcPath := s.torrentFile(t)

	for _, mode := range []Discovery{DiscoveryPublic, DiscoveryDirect} {
		mode := mode
		t.Run(mode.String(), func(t *testing.T) {
			staging := t.TempDir()
			sess := discSession(t, Config{
				StagingDir:      staging,
				Discovery:       mode,
				MetadataTimeout: 20 * time.Second,
			})
			src, err := parseSource(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := waitCtx(t)
			defer cancel()
			o, err := sess.open(ctx, src)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			live := o.t.Metainfo()
			got := live.UpvertedAnnounceList().DistinctValues()
			if mode.IsDirect() {
				if len(got) != 0 {
					t.Errorf("direct open kept trackers in the client view: %v", got)
				}
			} else if len(got) == 0 {
				t.Fatalf("control: public open dropped the tracker from the client view")
			}

			// The persisted metainfo is what the next run resumes from. It must
			// describe the same torrent, and carry no tracker in direct mode.
			persisted, err := metainfo.LoadFromFile(sess.metainfoPath(src.id))
			if err != nil {
				t.Fatalf("read persisted metainfo: %v", err)
			}
			if string(persisted.InfoBytes) != string(o.t.Metainfo().InfoBytes) {
				t.Error("persisted metainfo info bytes differ from the client's")
			}
			persistedTrackers := persisted.UpvertedAnnounceList().DistinctValues()
			if mode.IsDirect() && len(persistedTrackers) != 0 {
				t.Errorf("direct run persisted tracker URLs: %v", persistedTrackers)
			}
			if !mode.IsDirect() && len(persistedTrackers) == 0 {
				t.Fatal("control: public run persisted no tracker URLs")
			}
			// Reopening the persisted metainfo the way a resume does keeps the
			// policy: this is the spec the next run would add.
			resumeSpec := at.TorrentSpecFromMetaInfo(persisted)
			applySpecDiscovery(resumeSpec, mode)
			if mode.IsDirect() && (len(resumeSpec.Trackers) != 0 || len(resumeSpec.Webseeds) != 0 || len(resumeSpec.Sources) != 0) {
				t.Errorf("resume spec kept source hints in direct mode: %+v", resumeSpec)
			}
		})
	}
}

// TestNewRejectsInvalidDiscoveryBeforeTouchingTheFilesystem: an unknown profile
// is a configuration error, not a fallback. It must be reported before the
// staging root, the lock file or the client exist.
func TestNewRejectsInvalidDiscoveryBeforeTouchingTheFilesystem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "staging")
	_, err := New(Config{StagingDir: root, Discovery: Discovery("swarm")})
	if err == nil {
		t.Fatal("New accepted an unknown discovery profile")
	}
	if !errors.Is(err, ErrInvalidDiscovery) {
		t.Errorf("New error = %v, want ErrInvalidDiscovery", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("New created the staging root before rejecting the profile (stat err = %v)", statErr)
	}
}

// TestInspectDiscoveryProfiles: offline local inspection needs no peer at all,
// so it works in either profile; an unknown profile is still refused, before
// the source is even read.
func TestInspectDiscoveryProfiles(t *testing.T) {
	s := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(20000, 23)})
	src := s.torrentFile(t)
	ctx, cancel := waitCtx(t)
	defer cancel()

	for _, mode := range []Discovery{DiscoveryPublic, DiscoveryDirect} {
		info, err := Inspect(ctx, src, InspectConfig{Discovery: mode})
		if err != nil {
			t.Fatalf("Inspect(%s): %v", mode, err)
		}
		if len(info.Files) != 1 || info.InfoHash != s.infohash() {
			t.Errorf("Inspect(%s) = %+v, want the fixture's single file", mode, info)
		}
	}
	_, err := Inspect(ctx, src, InspectConfig{Discovery: Discovery("swarm")})
	if !errors.Is(err, ErrInvalidDiscovery) {
		t.Errorf("Inspect error = %v, want ErrInvalidDiscovery", err)
	}
}

// TestDirectInspectMagnetDoesNotContactTracker: inspection is metadata-only, but
// it still builds a client for a magnet, so the profile must apply there too.
func TestDirectInspectMagnetDoesNotContactTracker(t *testing.T) {
	tw := newTrackerTripwire(t)
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("ef", 20) + "&tr=" + url.QueryEscape(tw.announceURL())
	ctx, cancel := waitCtx(t)
	defer cancel()
	_, err := Inspect(ctx, magnet, InspectConfig{
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 400 * time.Millisecond,
	})
	if !errors.Is(err, ErrMetadataTimeout) {
		t.Fatalf("Inspect error = %v, want ErrMetadataTimeout", err)
	}
	waitNoHits(t, "direct-mode tracker tripwire", 400*time.Millisecond, tw.count)
}

// discSession is a Session with cleanup, for the discovery tests.
func discSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// listenerIP extracts the IP of a listener address, or nil.
func listenerIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func listenerAddrs(c *at.Client) []string {
	out := make([]string, 0)
	for _, l := range c.Listeners() {
		out = append(out, l.Addr().String())
	}
	return out
}

// jobFiles lists the job records in a staging root.
func jobFiles(t *testing.T, staging string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(staging, filepath.FromSlash(torrentStateDir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".job.json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// corruptAt flips one byte of a file at the given offset.
func corruptAt(t *testing.T, path string, offset int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, offset); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xff
	if _, err := f.WriteAt(buf, offset); err != nil {
		t.Fatal(err)
	}
}
