package extractor

import "testing"

func TestHostMatchesDomain(t *testing.T) {
	cases := []struct {
		host, domain string
		want         bool
	}{
		{"x.com", "x.com", true},
		{"www.x.com", "x.com", true},
		{"mobile.twitter.com", "twitter.com", true},
		{"x.com.", "x.com", true},             // fully-qualified trailing dot
		{"X.COM", "x.com", true},              // case-insensitive
		{"x.com.example.net", "x.com", false}, // lookalike prefix
		{"notx.com", "x.com", false},          // lookalike suffix
		{"x.com.evil.net", "x.com", false},    // lookalike subdomain
		{"com", "x.com", false},               // partial label
		{"", "x.com", false},                  // empty host
		{"x.com", "", false},                  // empty domain
		{"evilnet/x.com", "x.com", false},     // path-ish junk
		{"sub.instagram.com", "instagram.com", true},
	}
	for _, tc := range cases {
		if got := HostMatchesDomain(tc.host, tc.domain); got != tc.want {
			t.Errorf("HostMatchesDomain(%q, %q) = %v, want %v", tc.host, tc.domain, got, tc.want)
		}
	}
}

// TestParsersRejectLookalikeHosts proves the platform URL parsers only accept
// the real platform domains, so a disguised link cannot be resolved as a
// different platform resource.
func TestParsersRejectLookalikeHosts(t *testing.T) {
	igLookalikes := []string{
		"https://instagram.com.example.net/user/",
		"https://notinstagram.com/user/",
		"https://instagram.example.net/p/ABC",
	}
	for _, u := range igLookalikes {
		if _, err := ParseIgURL(u); err == nil {
			t.Errorf("ParseIgURL(%q) accepted a lookalike host", u)
		}
	}
	if got, err := ParseIgURL("https://www.instagram.com/user/"); err != nil || got.Username != "user" {
		t.Errorf("ParseIgURL real host = %+v, %v", got, err)
	}

	twLookalikes := []string{
		"https://x.com.example.net/user",
		"https://notx.com/user",
		"https://x.com.evil.net/hashtag/golang",
	}
	for _, u := range twLookalikes {
		if _, err := ParseTwSource(u); err == nil {
			t.Errorf("ParseTwSource(%q) accepted a lookalike host", u)
		}
	}
	if src, err := ParseTwSource("https://mobile.twitter.com/user"); err != nil || src.Kind != TwSourceProfile {
		t.Errorf("ParseTwSource real host = %+v, %v", src, err)
	}

	rdLookalikes := []string{
		"https://reddit.com.example.net/r/golang",
		"https://notreddit.com/user/x",
	}
	for _, u := range rdLookalikes {
		if _, err := ParseRdURL(u); err == nil {
			t.Errorf("ParseRdURL(%q) accepted a lookalike host", u)
		}
	}
	if _, err := ParseRdURL("https://old.reddit.com/r/golang"); err != nil {
		t.Errorf("ParseRdURL real host: %v", err)
	}
}

// TestAlbumURLRejectsLookalikeHosts proves the album router uses exact/domain
// matching for its index and album hosts.
func TestAlbumURLRejectsLookalikeHosts(t *testing.T) {
	if len(albIndexHosts) == 0 || len(albAlbumHosts) == 0 {
		t.Fatal("album host tables are empty")
	}
	index := albIndexHosts[0]
	if got := albURLKind("https://" + index + "/"); got != "index" {
		t.Errorf("index host %q = %q, want index", index, got)
	}
	if got := albURLKind("https://" + index + ".example.net/"); got != "" {
		t.Errorf("lookalike index host routed as %q, want generic", got)
	}
	album := albAlbumHosts[0]
	if got := albURLKind("https://" + album + "/a/xyz"); got != "album" {
		t.Errorf("album host %q = %q, want album", album, got)
	}
	if got := albURLKind("https://" + album + ".example.net/a/xyz"); got != "" {
		t.Errorf("lookalike album host routed as %q, want generic", got)
	}
	if got := albURLKind("https://" + album + "/f/abc"); got != "file" {
		t.Errorf("file page = %q, want file", got)
	}
}

// TestIgShortcodeOverflow proves codes that cannot fit a signed 64-bit media id
// are rejected instead of silently overflowing into another post's id.
func TestIgShortcodeOverflow(t *testing.T) {
	if id := igShortcodeToID("aye83DjauH"); id == 0 {
		t.Fatal("valid 10-char shortcode decoded to 0")
	}
	// 12 valid-alphabet characters exceed int64's 11-digit capacity.
	if id := igShortcodeToID("BAAAAAAAAAAA"); id != 0 {
		t.Errorf("12-char shortcode = %d, want 0 (overflow)", id)
	}
	// Previously a >28-char input was truncated to its last-28 removed prefix
	// and decoded; it must now be rejected outright.
	if id := igShortcodeToID("BCAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); id != 0 {
		t.Errorf("30-char shortcode = %d, want 0", id)
	}
	if id := igShortcodeToID(""); id != 0 {
		t.Errorf("empty shortcode = %d, want 0", id)
	}
}
