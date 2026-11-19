package bittorrent

// Shared-budget wiring (BT-08, second slice): one limiter instance must reach every
// client an attempt builds - the metadata-only resolver and the payload session -
// and a nil limiter must leave the library default alone.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"golang.org/x/time/rate"
)

// captureClientConfigs installs the hook for one test.
func captureClientConfigs(t *testing.T) func() []*at.ClientConfig {
	t.Helper()
	var mu sync.Mutex
	var got []*at.ClientConfig
	prev := clientConfigHook
	clientConfigHook = func(cc *at.ClientConfig) {
		mu.Lock()
		got = append(got, cc)
		mu.Unlock()
	}
	t.Cleanup(func() { clientConfigHook = prev })
	return func() []*at.ClientConfig {
		mu.Lock()
		defer mu.Unlock()
		out := got
		return out
	}
}

// TestClientConfigInstallsSharedLimiters covers the install point directly.
func TestClientConfigInstallsSharedLimiters(t *testing.T) {
	down := rate.NewLimiter(rate.Limit(256<<10), 64<<10)
	up := rate.NewLimiter(rate.Limit(64<<10), 32<<10)
	cc := clientConfig("/tmp/ignored", Config{DownloadLimiter: down, UploadLimiter: up})
	if cc.DownloadRateLimiter != down {
		t.Error("download limiter was not installed")
	}
	if cc.UploadRateLimiter != up {
		t.Error("upload limiter was not installed")
	}
	// Unlimited leaves the library default (rate.Inf), not a zero-rate limiter: a
	// zero rate is rejected by the library's own config check.
	plain := clientConfig("/tmp/ignored", Config{})
	if plain.DownloadRateLimiter != nil && plain.DownloadRateLimiter.Limit() == 0 {
		t.Error("unconfigured client has a zero-rate download limiter")
	}
	if plain.DownloadRateLimiter == down || plain.UploadRateLimiter == up {
		t.Error("an unconfigured client inherited a limiter")
	}
}

// TestFetchIsolatedSharesOneLimiter is the end-to-end wiring proof: a magnet forces
// both client constructions (metadata resolver, then payload session), and both must
// carry the same instance.
func TestFetchIsolatedSharesOneLimiter(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(48<<10, 5)}
	seed := newSeeder(t, "fixture", 16<<10, files)
	root := t.TempDir()
	down := rate.NewLimiter(rate.Limit(8<<20), 256<<10)
	up := rate.NewLimiter(rate.Limit(4<<20), 1<<20)

	configs := captureClientConfigs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := isolatedConfig(root, seed.peer())
	cfg.DownloadLimiter = down
	cfg.UploadLimiter = up
	if _, err := FetchIsolated(ctx, cfg, seed.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}

	seen := configs()
	if len(seen) < 2 {
		t.Fatalf("%d clients built, want at least 2 (resolver and payload)", len(seen))
	}
	for i, cc := range seen {
		if cc.DownloadRateLimiter != down {
			t.Errorf("client %d download limiter = %p, want %p", i, cc.DownloadRateLimiter, down)
		}
		if cc.UploadRateLimiter != up {
			t.Errorf("client %d upload limiter = %p, want %p", i, cc.UploadRateLimiter, up)
		}
	}
}

// TestClientConfigPeerLimits covers the mapping of the two peer knobs, including the
// client-wide half-open floor: a total below the per-torrent cap would make that cap
// unreachable.
func TestClientConfigPeerLimits(t *testing.T) {
	cc := clientConfig("/tmp/ignored", Config{MaxPeers: 6, MaxHalfOpen: 3})
	if cc.EstablishedConnsPerTorrent != 6 {
		t.Errorf("EstablishedConnsPerTorrent = %d, want 6", cc.EstablishedConnsPerTorrent)
	}
	if cc.HalfOpenConnsPerTorrent != 3 {
		t.Errorf("HalfOpenConnsPerTorrent = %d, want 3", cc.HalfOpenConnsPerTorrent)
	}
	if cc.TotalHalfOpenConns < 3 {
		t.Errorf("TotalHalfOpenConns = %d, below the per-torrent cap", cc.TotalHalfOpenConns)
	}
	// Derived default, as before this knob existed.
	dflt := clientConfig("/tmp/ignored", Config{MaxPeers: 6})
	if dflt.HalfOpenConnsPerTorrent != 6 && dflt.HalfOpenConnsPerTorrent != 8 {
		// min(6, 8) = 6
		if dflt.HalfOpenConnsPerTorrent != 6 {
			t.Errorf("derived half-open = %d, want min(6, 8)", dflt.HalfOpenConnsPerTorrent)
		}
	}
	if dflt.HalfOpenConnsPerTorrent == 3 {
		t.Error("the derived default ignored MaxPeers")
	}
}

// TestResolverAndPayloadClientsDoNotOverlap answers the question the runner bound
// depends on: can one attempt hold a metadata resolver and a payload client at the
// same time? The lifecycle hook records every open and close, so the peak concurrent
// client count is measured, not assumed.
func TestResolverAndPayloadClientsDoNotOverlap(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(64<<10, 11)}
	seed := newSeeder(t, "fixture", 16<<10, files)
	root := t.TempDir()

	var mu sync.Mutex
	var events []string
	live, peak := 0, 0
	prev := clientLifecycleHook
	clientLifecycleHook = func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		if len(event) >= 5 && event[:5] == "open:" {
			live++
			if live > peak {
				peak = live
			}
		} else {
			live--
		}
	}
	t.Cleanup(func() { clientLifecycleHook = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := isolatedConfig(root, seed.peer())
	cfg.MaxPeers = 4
	cfg.MaxHalfOpen = 2
	if _, err := FetchIsolated(ctx, cfg, seed.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), events...)
	peakClients := peak
	liveNow := live
	mu.Unlock()
	t.Logf("client lifecycle: %v (peak concurrent %d)", got, peakClients)
	if peakClients > 1 {
		t.Errorf("peak concurrent clients = %d: a resolver and a payload client overlapped, so the runner bound would not be max-active x per-job", peakClients)
	}
	if liveNow != 0 {
		t.Errorf("%d client(s) still open after the fetch returned", liveNow)
	}
	// The order matters as much as the count: resolve, close, then open the payload
	// session. A payload client that opened before the resolver closed would break the
	// bound even if the peak happened to be measured as one.
	wantPrefix := []string{"open:resolver", "close:resolver", "open:payload"}
	if len(got) < len(wantPrefix) {
		t.Fatalf("lifecycle = %v, want at least %v", got, wantPrefix)
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Fatalf("lifecycle[%d] = %q, want %q (full: %v)", i, got[i], want, got)
		}
	}
}

// TestCancelledResolutionLeavesNoClient: a magnet that never resolves must still
// close its resolver client and release the workspace, which is what keeps a slot
// honest when an attempt fails before the handoff.
func TestCancelledResolutionLeavesNoClient(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	live, peak := 0, 0
	prev := clientLifecycleHook
	clientLifecycleHook = func(event string) {
		mu.Lock()
		defer mu.Unlock()
		if len(event) >= 5 && event[:5] == "open:" {
			live++
			if live > peak {
				peak = live
			}
		} else {
			live--
		}
	}
	t.Cleanup(func() { clientLifecycleHook = prev })

	// No peers and a short metadata deadline: resolution fails, and nothing else
	// should ever be constructed.
	cfg := Config{
		StagingDir:      root,
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 300 * time.Millisecond,
		MaxPeers:        4,
		MaxHalfOpen:     2,
	}
	_, err := FetchIsolated(context.Background(), cfg, "magnet:?xt=urn:btih:"+strings.Repeat("7", 40), []string{"fixture/a.bin"})
	if err == nil {
		t.Fatal("a magnet with no peers and no metadata resolved")
	}
	mu.Lock()
	defer mu.Unlock()
	if live != 0 {
		t.Errorf("%d client(s) still open after a failed resolution", live)
	}
	if peak > 1 {
		t.Errorf("peak concurrent clients during a failed resolution = %d", peak)
	}
}
