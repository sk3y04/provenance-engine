package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func saveTorrentFlags(t *testing.T) {
	t.Helper()
	oldOut, oldSel, oldPeer, oldMax := flagTorrentOut, flagTorrentSelect, flagTorrentPeer, flagTorrentMaxBytes
	oldTimeout, oldJSON, oldSeed := flagTorrentTimeout, flagTorrentJSON, flagTorrentSeed
	oldDiscovery := flagTorrentDiscovery
	t.Cleanup(func() {
		flagTorrentOut, flagTorrentSelect, flagTorrentPeer, flagTorrentMaxBytes = oldOut, oldSel, oldPeer, oldMax
		flagTorrentTimeout, flagTorrentJSON, flagTorrentSeed = oldTimeout, oldJSON, oldSeed
		flagTorrentDiscovery = oldDiscovery
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

// runTorrentCLI executes the torrent command group with stdout/stderr silenced.
// The subcommands write to os.Stdout directly, so the descriptors are swapped
// rather than cobra's writers redirected.
func runTorrentCLI(t *testing.T, args ...string) error {
	t.Helper()
	silence := func() func() {
		oldOut, oldErr := os.Stdout, os.Stderr
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		os.Stdout, os.Stderr = null, null
		return func() { os.Stdout, os.Stderr = oldOut, oldErr; _ = null.Close() }
	}
	restore := silence()
	defer restore()
	cmd := torrentCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

// TestTorrentDiscoveryFlagReachesSubcommands: --discovery is a persistent flag on
// the group, so the subcommands that use a client inherit it. A value the
// adapter does not implement is rejected by name and never becomes a fallback.
func TestTorrentDiscoveryFlagReachesSubcommands(t *testing.T) {
	saveTorrentFlags(t)
	src, _ := torrentFixture(t)

	if err := runTorrentCLI(t, "inspect", "--discovery", "direct", src); err != nil {
		t.Fatalf("inspect --discovery direct: %v", err)
	}

	err := runTorrentCLI(t, "inspect", "--discovery", "swarm", src)
	if err == nil {
		t.Fatal("inspect accepted an unknown discovery profile")
	}
	for _, want := range []string{"--discovery", "public", "direct"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestTorrentFetchRejectsUnknownDiscoveryBeforeWork: the profile is checked
// before the staging root, the lock and the client exist, so a typo cannot leave
// half a job behind or quietly run with the default profile.
func TestTorrentFetchRejectsUnknownDiscoveryBeforeWork(t *testing.T) {
	saveTorrentFlags(t)
	src, _ := torrentFixture(t)
	staging := filepath.Join(t.TempDir(), "never-created")

	err := runTorrentCLI(t, "fetch", "--discovery", "swarm", "--output", staging,
		"--select", "fixture/a.bin", src)
	if err == nil {
		t.Fatal("fetch accepted an unknown discovery profile")
	}
	if !strings.Contains(err.Error(), "discovery") {
		t.Errorf("error = %v, want it to name the discovery profile", err)
	}
	if _, statErr := os.Stat(staging); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("fetch created the staging root %s before rejecting the profile", staging)
	}
}
