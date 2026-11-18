package main

import (
	"testing"

	"github.com/spf13/cobra"
)

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
