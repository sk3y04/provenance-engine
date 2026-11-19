package bittorrent

// Shared-budget wiring (BT-08, second slice): one limiter instance must reach every
// client an attempt builds - the metadata-only resolver and the payload session -
// and a nil limiter must leave the library default alone.

import (
	"context"
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
