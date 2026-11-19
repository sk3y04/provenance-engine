package torrentqueue

// BT-10g: queued downloads of a local .torrent, driven through the real runner.
//
// These are the controlled cases the BT-10f report lacked: the torrent file is built
// from the same directory the seeder serves, so the identities match by construction
// (asserted), and the matrix varies only piece length, payload size and the peer's
// upload rate.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// writeTorrentFile builds a real .torrent over files and returns its path.
func writeTorrentFile(t *testing.T, name string, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, name, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10, Name: name}
	if err := info.BuildFromFilePath(filepath.Join(dir, name)); err != nil {
		t.Fatalf("build info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	out := filepath.Join(t.TempDir(), name+".torrent")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := mi.Write(f); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return out
}

func probeCase(t *testing.T, upload int, size int) error {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	files := map[string][]byte{"data.bin": payload}
	seed := newLoopbackSeeder(t, "fixture", files, upload)
	torrentPath := writeTorrentFile(t, "fixture", files)
	// The queue must use the same identity the seeder serves.
	mi, lerr := metainfo.LoadFromFile(torrentPath)
	if lerr != nil {
		t.Fatalf("load torrent: %v", lerr)
	}
	if hex := mi.HashInfoBytes().HexString(); hex != seed.jobID()[3:] {
		t.Fatalf("fixture identity mismatch: torrent %s vs seeder %s", hex, seed.jobID()[3:])
	}
	root := t.TempDir()

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, _, aerr := store.Add(AddRequest{
		Source:     Source{Kind: "file", Path: torrentPath},
		RawSource:  torrentPath,
		Peers:      []string{seed.addr},
		Discovery:  "direct",
		Selection:  []string{"fixture/data.bin"},
		JobID:      seed.jobID(),
		InfoHash:   seed.jobID()[3:],
		Workspace:  ".provenance/torrent/jobs/" + seed.jobID(),
		Unresolved: false,
	})
	if aerr != nil {
		t.Fatalf("add: %v", aerr)
	}
	startEntry(t, store, entry.ID)
	sum, rerr := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
	if rerr != nil {
		return rerr
	}
	if sum.Completed != 1 {
		got, _ := store.Get(entry.ID)
		return fmt.Errorf("queue case did not complete: phase=%q err=%q", got.Phase, got.LastError)
	}
	return nil
}

// TestQueueFileSourceDownloadsLargePieces is the variant the CLI fixtures used: 64 KiB pieces with a
// throttled peer.
func TestQueueFileSourceDownloadsLargePieces(t *testing.T) {
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture", "data.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 64 << 10, Name: "fixture"}
	if err := info.BuildFromFilePath(filepath.Join(dir, "fixture")); err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	torrentPath := filepath.Join(t.TempDir(), "p64.torrent")
	f, _ := os.Create(torrentPath)
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	seed := startProbeSeeder(t, dir, &mi, 768<<10)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "file", Path: torrentPath},
		RawSource: torrentPath,
		Peers:     []string{seed},
		Discovery: "direct",
		Selection: []string{"fixture/data.bin"},
	})
	if aerr != nil {
		t.Fatal(aerr)
	}
	startEntry(t, store, entry.ID)
	start := time.Now()
	done := make(chan Summary, 1)
	go func() {
		sum, _ := NewRunner(store, realAdapter(), Options{Log: os.Stderr}).Run(context.Background())
		done <- sum
	}()
	select {
	case sum := <-done:
		got, _ := store.Get(entry.ID)
		t.Logf("P64 elapsed=%s completed=%d phase=%q err=%q", time.Since(start).Round(time.Millisecond), sum.Completed, got.Phase, got.LastError)
		if sum.Completed != 1 {
			t.Fatalf("64 KiB-piece fixture did not complete: %+v", got)
		}
	case <-time.After(75 * time.Second):
		got, _ := store.Get(entry.ID)
		t.Fatalf("64 KiB-piece fixture did not complete within 75s: phase=%q err=%q", got.Phase, got.LastError)
	}
}

func TestQueueFileSourceDownloadsMatrix(t *testing.T) {
	cases := []struct {
		name   string
		upload int
		size   int
	}{
		{"small-unthrottled", 0, 64 << 10},
		{"small-throttled", 128 << 10, 64 << 10},
		{"large-unthrottled", 0, 8 << 20},
		{"large-throttled", 768 << 10, 8 << 20},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- probeCase(t, c.upload, c.size) }()
			select {
			case err := <-done:
				t.Logf("MATRIX %s upload=%d size=%d elapsed=%s err=%v", c.name, c.upload, c.size, time.Since(start).Round(time.Millisecond), err)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
			case <-time.After(75 * time.Second):
				t.Fatalf("MATRIX %s upload=%d size=%d: no completion within 75s", c.name, c.upload, c.size)
			}
		})
	}
}
