package torrentqueue

import (
	"os"
	"testing"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"
)

// startProbeSeeder serves a prepared directory with a chosen metainfo and upload rate,
// returning its loopback address.
func startProbeSeeder(t *testing.T, dir string, mi *metainfo.MetaInfo, upload int) string {
	t.Helper()
	cfg := at.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.Seed = true
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisablePEX = true, true, true
	cfg.DisableUTP, cfg.DisableIPv6, cfg.DisableWebtorrent = true, true, true
	cfg.DisableWebseeds, cfg.NoDefaultPortForwarding = true, true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	if upload > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(upload), 1<<20)
	}
	client, err := at.NewClient(cfg)
	if err != nil {
		t.Fatalf("seeder client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	tor, err := client.AddTorrent(mi)
	if err != nil {
		t.Fatalf("seeder add: %v", err)
	}
	<-tor.GotInfo()
	_ = os.Stdout
	return "127.0.0.1:" + itoa(client.LocalPort())
}
