package bittorrent

// BT-09b: Config.FilePriorities reaches the pinned backend's file priorities.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
)

func TestFilePrioritiesReachBackendBeforeRequests(t *testing.T) {
	files := map[string][]byte{
		"a.bin": patterned(96<<10, 79),
		"b.bin": patterned(64<<10, 83),
		"c.bin": patterned(32<<10, 89),
	}
	seedRoot := t.TempDir()
	source := ctlMetainfo(t, seedRoot, "fixture", files)
	peer := ctlServe(t, seedRoot, "fixture", source)

	applied := make(chan map[string]int, 1)
	prev := filePriorityHook
	filePriorityHook = func(m map[string]int) {
		select {
		case applied <- m:
		default:
		}
	}
	t.Cleanup(func() { filePriorityHook = prev })

	root := t.TempDir()
	cfg := isolatedConfig(root, peer.addr)
	cfg.FilePriorities = map[string]string{"fixture/a.bin": "high"}
	magnet := "magnet:?xt=urn:btih:" + source.HashInfoBytes().HexString()
	cap, err := FetchIsolated(context.Background(), cfg, magnet, []string{"fixture/a.bin", "fixture/b.bin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	var got map[string]int
	select {
	case got = <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("the priority hook never ran")
	}
	if got["fixture/a.bin"] != int(at.PiecePriorityHigh) {
		t.Errorf("a.bin priority = %d, want high (%d)", got["fixture/a.bin"], int(at.PiecePriorityHigh))
	}
	if got["fixture/b.bin"] != int(at.PiecePriorityNormal) {
		t.Errorf("b.bin priority = %d, want normal (%d)", got["fixture/b.bin"], int(at.PiecePriorityNormal))
	}
	if got["fixture/c.bin"] != int(at.PiecePriorityNone) {
		t.Errorf("unselected c.bin priority = %d, want none (%d)", got["fixture/c.bin"], int(at.PiecePriorityNone))
	}
	// The shared edge piece: 96 KiB is not a multiple of 16 KiB, so a.bin's last
	// piece carries b.bin's first bytes. Both files must still be present.
	for _, rel := range []string{"fixture/a.bin", "fixture/b.bin"} {
		if _, err := os.Stat(filepath.Join(cap.StagingDir, rel)); err != nil {
			t.Fatalf("selected %s missing after a prioritised fetch: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cap.StagingDir, "fixture/c.bin")); !os.IsNotExist(err) {
		t.Fatal("an unselected neighbour was written")
	}
	// An unselected path in the override table is ignored, never requested.
	cfg2 := isolatedConfig(t.TempDir(), peer.addr)
	cfg2.FilePriorities = map[string]string{"fixture/c.bin": "high"}
	applied2 := make(chan map[string]int, 1)
	filePriorityHook = func(m map[string]int) {
		select {
		case applied2 <- m:
		default:
		}
	}
	if _, err := FetchIsolated(context.Background(), cfg2, magnet, []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("fetch 2: %v", err)
	}
	second := <-applied2
	if second["fixture/c.bin"] != int(at.PiecePriorityNone) {
		t.Fatalf("an override for an unselected file raised it to %d", second["fixture/c.bin"])
	}
}
