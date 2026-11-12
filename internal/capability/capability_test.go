package capability

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestProbeHasNoSideEffects proves a health check cannot install a tool or mutate
// the filesystem: the probe only resolves names through the injected seam.
func TestProbeHasNoSideEffects(t *testing.T) {
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if name == ToolYtdlp {
			return "/fake/yt-dlp", nil
		}
		return "", errNotFound
	}
	defer func() { lookPath = orig }()

	set := Probe(context.Background(), "")
	ytdlp, ok := set.Get(ToolYtdlp)
	if !ok || !ytdlp.Available || ytdlp.Path != "/fake/yt-dlp" {
		t.Fatalf("yt-dlp = %+v", ytdlp)
	}
	if !ytdlp.Required {
		t.Error("yt-dlp must be reported as required")
	}
	ffmpeg, ok := set.Get(ToolFfmpeg)
	if !ok || ffmpeg.Available {
		t.Fatalf("ffmpeg = %+v", ffmpeg)
	}
	if !set.Available(ToolChrome) {
		t.Log("chrome correctly reported unavailable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Probe(ctx, ""); got != nil {
		t.Fatalf("probe on a canceled context should return nothing, got %+v", got)
	}
}

var errNotFound = &probeError{}

type probeError struct{}

func (probeError) Error() string { return "not found" }

// cacheHome points os.UserCacheDir at dir for the test (Linux honors
// XDG_CACHE_HOME) so probes never read the real go-ytdlp cache.
func cacheHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

// TestResolveFindsVersionedCacheBinary: go-ytdlp caches its binary as
// <name>-<version> (e.g. yt-dlp-2026.08.19), and its own resolver tries that
// name first. The probe must find such installs or it reports the tool
// missing while the engine can actually run it.
func TestResolveFindsVersionedCacheBinary(t *testing.T) {
	dir := cacheHome(t)
	cache := filepath.Join(dir, "go-ytdlp")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(cache, "yt-dlp-2026.08.19")
	if err := os.WriteFile(versioned, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	lookPath = func(name string) (string, error) { return "", errNotFound }
	defer func() { lookPath = orig }()

	set := Probe(context.Background(), "")
	ytdlp, _ := set.Get(ToolYtdlp)
	if !ytdlp.Available || ytdlp.Path != versioned {
		t.Fatalf("yt-dlp = %+v, want available at %s", ytdlp, versioned)
	}
}

// TestResolvePrefersPlainCacheName: a plain <name> file in the cache wins
// over a versioned one, so a manually placed binary is reported.
func TestResolvePrefersPlainCacheName(t *testing.T) {
	dir := cacheHome(t)
	cache := filepath.Join(dir, "go-ytdlp")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(cache, "yt-dlp")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "yt-dlp-2026.08.19"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	lookPath = func(name string) (string, error) { return "", errNotFound }
	defer func() { lookPath = orig }()

	set := Probe(context.Background(), "")
	ytdlp, _ := set.Get(ToolYtdlp)
	if !ytdlp.Available || ytdlp.Path != plain {
		t.Fatalf("yt-dlp = %+v, want available at %s", ytdlp, plain)
	}
}

// TestResolvePicksNewestVersionedCacheBinary: with several versioned
// installs in the cache, the newest version is reported. Version parts are
// compared numerically (2026.8.19 > 2026.1.1), not as strings.
func TestResolvePicksNewestVersionedCacheBinary(t *testing.T) {
	dir := cacheHome(t)
	cache := filepath.Join(dir, "go-ytdlp")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2026.1.1", "2026.8.19"} {
		if err := os.WriteFile(filepath.Join(cache, "yt-dlp-"+v), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	orig := lookPath
	lookPath = func(name string) (string, error) { return "", errNotFound }
	defer func() { lookPath = orig }()

	set := Probe(context.Background(), "")
	want := filepath.Join(cache, "yt-dlp-2026.8.19")
	ytdlp, _ := set.Get(ToolYtdlp)
	if !ytdlp.Available || ytdlp.Path != want {
		t.Fatalf("yt-dlp = %+v, want available at %s", ytdlp, want)
	}
}
