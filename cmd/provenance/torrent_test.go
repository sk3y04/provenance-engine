package main

import (
	"context"
	"strings"
	"testing"
)

func saveTorrentFlags(t *testing.T) {
	t.Helper()
	oldOut, oldSel, oldPeer, oldMax := flagTorrentOut, flagTorrentSelect, flagTorrentPeer, flagTorrentMaxBytes
	oldTimeout, oldJSON, oldSeed := flagTorrentTimeout, flagTorrentJSON, flagTorrentSeed
	t.Cleanup(func() {
		flagTorrentOut, flagTorrentSelect, flagTorrentPeer, flagTorrentMaxBytes = oldOut, oldSel, oldPeer, oldMax
		flagTorrentTimeout, flagTorrentJSON, flagTorrentSeed = oldTimeout, oldJSON, oldSeed
	})
}

// TestNormalizeCLISelection: both repeated flags and comma lists must work,
// because file paths are awkward to repeat in a shell.
func TestNormalizeCLISelection(t *testing.T) {
	got := normalizeCLISelection([]string{" fixture/a.bin,b.bin", "", " fixture/c.bin "})
	want := []string{"fixture/a.bin", "b.bin", "fixture/c.bin"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("normalizeCLISelection = %v, want %v", got, want)
	}
	if len(normalizeCLISelection(nil)) != 0 {
		t.Fatal("nil selection produced entries")
	}
}

// TestRedactSource: terminal output must never echo tracker URLs, which can
// carry private passkeys.
func TestRedactSource(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=http://tracker.example/announce?passkey=SECRET"
	got := redactSource(magnet)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "tracker.example") {
		t.Fatalf("redactSource leaked a tracker: %q", got)
	}
	if !strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("redactSource dropped the infohash: %q", got)
	}
	if got := redactSource("/home/user/media/file.torrent"); got != "file.torrent" {
		t.Fatalf("redactSource(path) = %q, want file.torrent", got)
	}
}

// TestTorrentFetchRequiresSelection: the CLI refuses to fetch without an
// explicit selection before it resolves the source or opens a socket.
func TestTorrentFetchRequiresSelection(t *testing.T) {
	saveTorrentFlags(t)
	flagTorrentSelect = nil
	flagTorrentOut = t.TempDir()
	flagTorrentMaxBytes = ""

	err := runTorrentFetch(context.Background(), "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567")
	if err == nil || !strings.Contains(err.Error(), "--select") {
		t.Fatalf("error = %v, want a --select requirement", err)
	}
}

// TestTorrentMaxBytesRejectsGarbage: a malformed limit must fail loudly rather
// than silently disabling the cap.
func TestTorrentMaxBytesRejectsGarbage(t *testing.T) {
	saveTorrentFlags(t)
	flagTorrentMaxBytes = "lots"
	if _, err := parseMaxBytes(); err == nil {
		t.Fatal("parseMaxBytes accepted garbage")
	}
	flagTorrentMaxBytes = "4GB"
	n, err := parseMaxBytes()
	if err != nil || n != 4<<30 {
		t.Fatalf("parseMaxBytes(4GB) = %d, %v", n, err)
	}
}
