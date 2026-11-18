package dispatcher

import "testing"

// TestClassifyUsesExactHosts pins routing to exact domains and subdomains.
// Substring matching previously sent lookalike hosts (instagram.com.evil.net)
// to native extractors, which then resolved a different resource than the URL
// names instead of letting yt-dlp handle the actual host.
func TestClassifyUsesExactHosts(t *testing.T) {
	cases := []struct {
		url  string
		want Site
	}{
		{"https://x.com/user", SiteTwitter},
		{"https://twitter.com/user", SiteTwitter},
		{"https://mobile.twitter.com/user", SiteTwitter},
		{"https://x.com/hashtag/golang", SiteTwitter},
		{"x:#golang", SiteTwitter},
		{"https://x.com.evil.net/user", SiteGeneric},
		{"https://notx.com/user", SiteGeneric},

		{"https://www.instagram.com/user/", SiteInstagram},
		{"https://i.instagram.com/api/v1/x", SiteInstagram},
		{"https://instagram.com.evil.net/p/ABC", SiteGeneric},
		{"https://notinstagram.com/user/", SiteGeneric},

		{"https://www.reddit.com/r/golang", SiteReddit},
		{"https://old.reddit.com/user/x", SiteReddit},
		{"https://reddit.com.evil.net/r/golang", SiteGeneric},
		{"https://notreddit.com/r/golang", SiteGeneric},

		{"https://www.patreon.com/user", SitePatreon},
		{"https://patreon.com.evil.net/user", SiteGeneric},
		{"https://notpatreon.com/user", SiteGeneric},

		{"https://example.com/video", SiteGeneric},
	}
	for _, tc := range cases {
		got, err := Classify(tc.url)
		if err != nil {
			t.Errorf("Classify(%q): %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Classify(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
