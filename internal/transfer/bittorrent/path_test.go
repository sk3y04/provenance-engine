package bittorrent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeComponentRejectsHostileNames: metainfo path components come from a
// peer or a downloaded .torrent, so anything that could escape the staging
// root, alias another entry, or break on a supported platform must be refused.
func TestSafeComponentRejectsHostileNames(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../evil",
		"..\\evil",
		"/etc",
		"a/b",
		"a\\b",
		"C:",
		"c:",
		"CON",
		"con.txt",
		"NUL",
		"LPT9.log",
		"trailing.",
		"trailing ",
		"nul\x00byte",
		"\xff\xfe invalid utf8",
		strings.Repeat("a", maxComponentLen+1),
	}
	for _, name := range bad {
		if err := safeComponent(name); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("safeComponent(%q) = %v, want ErrUnsafePath", name, err)
		}
	}
	good := []string{"a.bin", "Some File.mkv", ".hidden", "archive.tar.gz", "é-accent", "-", "e2024"}
	for _, name := range good {
		if err := safeComponent(name); err != nil {
			t.Errorf("safeComponent(%q) = %v, want nil", name, err)
		}
	}
}

// TestRelPathJoinsNameAndPath: the capture path must be the torrent name plus
// the file path, and any unsafe component in either rejects the whole entry.
func TestRelPathJoinsNameAndPath(t *testing.T) {
	rel, err := relPath("fixture", []string{"sub", "a.bin"})
	if err != nil {
		t.Fatalf("relPath: %v", err)
	}
	if rel != "fixture/sub/a.bin" {
		t.Errorf("relPath = %q, want fixture/sub/a.bin", rel)
	}

	for _, tc := range []struct {
		name string
		path []string
	}{
		{"..", []string{"a.bin"}},
		{"fixture", []string{"..", "a.bin"}},
		{"fixture", []string{}},
		{"fixture", []string{"a", "../b"}},
		{"fixture", []string{"/etc/passwd"}},
		{"fixture", []string{"C:"}},
	} {
		if _, err := relPath(tc.name, tc.path); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("relPath(%q, %v) = %v, want ErrUnsafePath", tc.name, tc.path, err)
		}
	}
}

func TestWithinRoot(t *testing.T) {
	root := string(filepath.Separator) + filepath.Join("srv", "staging")
	cases := []struct {
		target string
		want   bool
	}{
		{root, true},
		{filepath.Join(root, "a", "b.bin"), true},
		{filepath.Join(root, "..", "evil"), false},
		{string(filepath.Separator) + filepath.Join("etc", "passwd"), false},
		{root + "-sibling", false},
	}
	for _, tc := range cases {
		if got := withinRoot(root, tc.target); got != tc.want {
			t.Errorf("withinRoot(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

// TestCheckNoSymlinkRejectsLinkInsideStaging: a symlink left in staging (by an
// earlier job, a sync tool, or a local attacker) must stop the write even
// though the relative path looks safe.
func TestCheckNoSymlinkRejectsLinkInsideStaging(t *testing.T) {
	staging := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(staging, "fixture")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := checkNoSymlink(staging, "fixture"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("checkNoSymlink = %v, want ErrUnsafePath", err)
	}
	// A path that does not exist yet is fine: nothing to follow.
	if err := checkNoSymlink(staging, "other/a.bin"); err != nil {
		t.Fatalf("checkNoSymlink(new path) = %v, want nil", err)
	}
}

// TestResolveCapturedRejectsSymlinkedParent: the final read for hashing must
// prove that resolution stayed inside staging.
func TestResolveCapturedRejectsSymlinkedParent(t *testing.T) {
	staging := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(staging, "dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveCaptured(staging, "dir/evil.bin"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("resolveCaptured = %v, want ErrUnsafePath", err)
	}
}

func TestParsePeers(t *testing.T) {
	if _, err := parsePeers([]string{"127.0.0.1"}); err == nil {
		t.Error("peer without a port accepted")
	}
	if _, err := parsePeers([]string{"127.0.0.1:0"}); err == nil {
		t.Error("peer with port 0 accepted")
	}
	if _, err := parsePeers([]string{"127.0.0.1:70000"}); err == nil {
		t.Error("peer with an out-of-range port accepted")
	}
	peers, err := parsePeers([]string{" 127.0.0.1:51413 ", ""})
	if err != nil {
		t.Fatalf("parsePeers: %v", err)
	}
	if len(peers) != 1 || peers[0].Addr.String() != "127.0.0.1:51413" {
		t.Fatalf("parsePeers = %+v", peers)
	}
}

// TestJobSelectionIsImmutable: resuming a job with a different selection would
// make "resume" ambiguous, so it is refused instead of silently redefined.
func TestJobSelectionIsImmutable(t *testing.T) {
	if !equalSelection([]string{"b", "a"}, []string{"a", "b", "a"}) {
		t.Error("equalSelection rejected the same set in a different order")
	}
	if equalSelection([]string{"a"}, []string{"a", "b"}) {
		t.Error("equalSelection accepted different sets")
	}
	if got := normalizeSelection([]string{"b", "a", "b", ""}); strings.Join(got, ",") != "a,b" {
		t.Errorf("normalizeSelection = %v, want [a b]", got)
	}
}

func TestKindFor(t *testing.T) {
	cases := map[string]string{
		"x/a.mkv":    "video",
		"x/a.MP4":    "video",
		"x/a.jpg":    "image",
		"x/a.flac":   "audio",
		"x/a.md":     "text",
		"x/a.exe":    "binary",
		"x/noext":    "binary",
		"x/a.tar.gz": "binary",
	}
	for in, want := range cases {
		if got := kindFor(in); got != want {
			t.Errorf("kindFor(%q) = %q, want %q", in, got, want)
		}
	}
}
