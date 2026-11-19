package bittorrent

// BT-08d.1 step 1: the control harness, with no production code in the loop.
//
// Two plain library clients, a complete metainfo and deterministic bytes. If this
// cannot move a fixture from one loopback process to another, nothing built on top
// of it can be diagnosed - so every wait here is bounded and reports the phase, the
// elapsed time and the observable state instead of blocking on a channel that may
// never fire.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// ctlPeer is one plain library client in the control harness.
type ctlPeer struct {
	client *at.Client
	tor    *at.Torrent
	addr   string
	root   string
}

// ctlMetainfo writes files under root/<name> and builds the complete metainfo.
func ctlMetainfo(t *testing.T, root, name string, files map[string][]byte) *metainfo.MetaInfo {
	t.Helper()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10, Name: name}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatalf("control: build info: %v", err)
	}
	return &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
}

// ctlConfig is the loopback-only client config used by both sides.
func ctlConfig(dataDir string) *at.ClientConfig {
	cfg := at.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisablePEX = true, true, true
	cfg.DisableUTP, cfg.DisableIPv6, cfg.DisableWebtorrent = true, true, true
	cfg.DisableWebseeds, cfg.NoDefaultPortForwarding = true, true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	return cfg
}

// ctlServe starts a seeder for an already-written fixture directory.
func ctlServe(t *testing.T, root, name string, mi *metainfo.MetaInfo) *ctlPeer {
	t.Helper()
	cfg := ctlConfig(root)
	cfg.Seed = true
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("control seeder: start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("control seeder: add torrent: %v", err)
	}
	peer := &ctlPeer{client: client, tor: tor, addr: "127.0.0.1:" + strconv.Itoa(client.LocalPort()), root: root}
	// VerifyDataContext is synchronous: when it returns, every piece has been
	// hashed and marked, so Complete() is authoritative afterwards.
	if err := tor.VerifyDataContext(t.Context()); err != nil {
		t.Fatalf("control seeder: verify: %v", err)
	}
	// Complete() is a cached flag that lags the piece marks, so the authoritative
	// check right after verification is the piece states themselves. Both are
	// reported on failure so a future discrepancy is visible rather than mysterious.
	if !peer.allPiecesComplete() {
		t.Fatalf("control seeder: fixture did not verify (%s)", peer.describe())
	}
	return peer
}

// ctlLeech starts a leecher that dials exactly one peer and downloads everything.
func ctlLeech(t *testing.T, root string, mi *metainfo.MetaInfo, seedAddr string) *ctlPeer {
	t.Helper()
	client, err := at.NewClient(ctlConfig(root))
	if err != nil {
		t.Fatalf("control leecher: start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("control leecher: add torrent: %v", err)
	}
	peer := &ctlPeer{client: client, tor: tor, addr: "127.0.0.1:" + strconv.Itoa(client.LocalPort()), root: root}
	if tor.Info() == nil {
		// A complete metainfo should give us info immediately; if it does not, say so
		// instead of waiting on a channel that may never fire.
		t.Fatalf("control leecher: no info after AddTorrent (%s)", peer.describe())
	}
	tor.AddPeers([]at.PeerInfo{{Addr: peerAddr{host: "127.0.0.1", port: portOf(t, seedAddr)}}})
	tor.DownloadAll()
	return peer
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("control: bad address %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("control: bad port %q: %v", portStr, err)
	}
	return port
}

// allPiecesComplete is the synchronous truth: every piece has been hashed and
// marked. It is deliberately not Complete(), which is a flag updated on client
// events and can lag a direct verification pass.
func (p *ctlPeer) allPiecesComplete() bool {
	if p.tor.NumPieces() == 0 {
		return false
	}
	for i := 0; i < p.tor.NumPieces(); i++ {
		if !p.tor.Piece(i).State().Complete {
			return false
		}
	}
	return true
}

// describe renders what is observable right now, for a failure message.
func (p *ctlPeer) describe() string {
	stats := p.client.Stats()
	states := map[string]int{}
	for i := 0; i < p.tor.NumPieces(); i++ {
		s := p.tor.Piece(i).State()
		switch {
		case s.Complete:
			states["complete"]++
		case s.Hashing:
			states["hashing"]++
		case s.QueuedForHash:
			states["queued"]++
		case s.Marking:
			states["marking"]++
		default:
			states["missing"]++
		}
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pieces := ""
	for _, k := range keys {
		pieces += fmt.Sprintf("%s=%d ", k, states[k])
	}
	return fmt.Sprintf("addr=%s completeFlag=%v piecesComplete=%v bytesRead=%d bytesWritten=%d activePeers=%d halfOpen=%d pieces[%s]",
		p.addr, p.tor.Complete().Bool(), p.allPiecesComplete(), stats.BytesReadUsefulData.Int64(), stats.BytesWrittenData.Int64(),
		stats.ActivePeers, stats.ActiveHalfOpenAttempts, pieces)
}

// waitFor polls a condition with a bounded deadline and names the phase it was
// waiting for. It never sleeps indefinitely and always reports the observed state.
func ctlWait(t *testing.T, phase string, timeout time.Duration, cond func() bool, state func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: timed out after %s waiting for %s; state: %s", timeout, phase, state())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestControlLoopbackTransfer proves the harness itself: one plain seeder, one plain
// leecher, exact bytes. This is the floor everything else in BT-08d.1 stands on.
func TestControlLoopbackTransfer(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(180<<10, 3), "b.bin": patterned(40<<10, 5)}
	seedRoot := t.TempDir()
	mi := ctlMetainfo(t, seedRoot, "fixture", files)
	seeder := ctlServe(t, seedRoot, "fixture", mi)

	leechRoot := t.TempDir()
	leecher := ctlLeech(t, leechRoot, mi, seeder.addr)
	ctlWait(t, "leecher completion", 60*time.Second, func() bool { return leecher.tor.Complete().Bool() }, leecher.describe)

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(leechRoot, "fixture", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("control: read %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("control: %s bytes differ (got %d bytes)", rel, len(got))
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("control: %s digest differs", rel)
		}
	}
	t.Logf("control transfer: %s", leecher.describe())
}
