package main

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestParseRepeatInterval: --every feeds time.NewTicker, which panics on
// non-positive durations; zero/negative intervals must be rejected up front.
func TestParseRepeatInterval(t *testing.T) {
	if d, err := parseRepeatInterval("30m"); err != nil || d != 30*time.Minute {
		t.Errorf("parseRepeatInterval(30m) = %v, %v", d, err)
	}
	for _, bad := range []string{"0s", "-5s", "-1h", "garbage", ""} {
		if _, err := parseRepeatInterval(bad); err == nil {
			t.Errorf("parseRepeatInterval(%q) accepted, want error", bad)
		}
	}
}

// TestShouldSkipAutoInstall: offline vault/metadata operations must not
// trigger a yt-dlp/ffmpeg auto-install (which hits the network on a fresh
// cache), while operations that actually extract media must keep it.
func TestShouldSkipAutoInstall(t *testing.T) {
	// path is the command chain below the root, e.g. []string{"archive",
	// "session"} builds `provenance archive session`.
	cases := []struct {
		path []string
		want bool
	}{
		{[]string{"cite"}, true},
		{[]string{"diff"}, true},
		{[]string{"search"}, true},
		{[]string{"vault", "show"}, true},
		{[]string{"archive", "session"}, true},
		{[]string{"archive", "collection"}, true},
		{[]string{"archive", "import"}, true},
		{[]string{"archive", "import-pdf"}, true},
		{[]string{"archive", "import-git"}, true},
		{[]string{"archive", "import-docs"}, true},
		{[]string{"archive", "import-web"}, true},
		{[]string{"grab"}, false},
		{[]string{"scan"}, false},
		{[]string{"torrent", "inspect"}, true},
		{[]string{"torrent", "fetch"}, true},
		{[]string{"archive", "url"}, false},
		{[]string{"collect", "sync"}, false},
		{[]string{"watch", "run"}, false},
		{[]string{"encode"}, false},
	}
	for _, tc := range cases {
		root := &cobra.Command{Use: "provenance"}
		parent := root
		var leaf *cobra.Command
		for _, name := range tc.path {
			leaf = &cobra.Command{Use: name}
			parent.AddCommand(leaf)
			parent = leaf
		}
		if got := shouldSkipAutoInstall(leaf); got != tc.want {
			t.Errorf("shouldSkipAutoInstall(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
