package extractor

import (
	"strings"
	"testing"
)

func TestParseTwSourceHashtagForms(t *testing.T) {
	tests := []struct {
		in        string
		wantTag   string
		wantSlug  string
		wantQuery string
		wantCanon string
	}{
		{"#golang", "golang", "golang", "#golang filter:media", "x:#golang"},
		{"x:#golang", "golang", "golang", "#golang filter:media", "x:#golang"},
		{"X:#Golang", "Golang", "golang", "#Golang filter:media", "x:#golang"},
		{"https://x.com/hashtag/golang", "golang", "golang", "#golang filter:media", "x:#golang"},
		{"https://twitter.com/hashtag/golang", "golang", "golang", "#golang filter:media", "x:#golang"},
		{"  #golang  ", "golang", "golang", "#golang filter:media", "x:#golang"},
		{"#go_lang", "go_lang", "go_lang", "#go_lang filter:media", "x:#go_lang"},
	}
	for _, tt := range tests {
		src, err := ParseTwSource(tt.in)
		if err != nil {
			t.Errorf("ParseTwSource(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if src.Kind != TwSourceHashtag {
			t.Errorf("ParseTwSource(%q).Kind = %v, want hashtag", tt.in, src.Kind)
		}
		if src.Hashtag != tt.wantTag {
			t.Errorf("ParseTwSource(%q).Hashtag = %q, want %q", tt.in, src.Hashtag, tt.wantTag)
		}
		if src.Slug != tt.wantSlug {
			t.Errorf("ParseTwSource(%q).Slug = %q, want %q", tt.in, src.Slug, tt.wantSlug)
		}
		if src.Query != tt.wantQuery {
			t.Errorf("ParseTwSource(%q).Query = %q, want %q", tt.in, src.Query, tt.wantQuery)
		}
		if src.Canonical != tt.wantCanon {
			t.Errorf("ParseTwSource(%q).Canonical = %q, want %q", tt.in, src.Canonical, tt.wantCanon)
		}
	}
}

func TestParseTwSourceUnicodeHashtag(t *testing.T) {
	for _, in := range []string{"#日本語", "x:#café", "https://x.com/hashtag/%E6%97%A5%E6%9C%AC%E8%AA%9E"} {
		src, err := ParseTwSource(in)
		if err != nil {
			t.Fatalf("ParseTwSource(%q) unexpected error: %v", in, err)
		}
		if src.Kind != TwSourceHashtag {
			t.Errorf("ParseTwSource(%q).Kind = %v, want hashtag", in, src.Kind)
		}
		if src.Slug == "" {
			t.Errorf("ParseTwSource(%q).Slug is empty", in)
		}
	}
}

func TestParseTwSourceHashtagErrors(t *testing.T) {
	tests := []string{
		"#",
		"x:#",
		"# ",
		"#two words",
		"#tag filter:media",
		"#tag OR #other",
		"#tag(evil)",
		"#tag:injected",
		"#tag\"quote",
		"#tag#second",
		"https://x.com/hashtag/",
		"https://x.com/hashtag",
	}
	for _, in := range tests {
		if src, err := ParseTwSource(in); err == nil {
			t.Errorf("ParseTwSource(%q) expected error, got %+v", in, src)
		}
	}
}

func TestParseTwSourceProfileUnchanged(t *testing.T) {
	tests := []struct {
		in       string
		username string
		canon    string
	}{
		{"https://x.com/artist", "artist", "https://x.com/artist"},
		{"https://twitter.com/artist", "artist", "https://x.com/artist"},
		{"https://x.com/artist/status/123", "artist", "https://x.com/artist"},
		{"https://x.com/artist/", "artist", "https://x.com/artist"},
	}
	for _, tt := range tests {
		src, err := ParseTwSource(tt.in)
		if err != nil {
			t.Errorf("ParseTwSource(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if src.Kind != TwSourceProfile {
			t.Errorf("ParseTwSource(%q).Kind = %v, want profile", tt.in, src.Kind)
		}
		if src.Username != tt.username {
			t.Errorf("ParseTwSource(%q).Username = %q, want %q", tt.in, src.Username, tt.username)
		}
		if src.Canonical != tt.canon {
			t.Errorf("ParseTwSource(%q).Canonical = %q, want %q", tt.in, src.Canonical, tt.canon)
		}
	}
}

func TestParseTwURLStillWorks(t *testing.T) {
	got, err := ParseTwURL("https://x.com/artist/status/123")
	if err != nil {
		t.Fatalf("ParseTwURL error: %v", err)
	}
	if got != "artist" {
		t.Errorf("ParseTwURL = %q, want artist", got)
	}
}

func TestIsTwitterHashtagSource(t *testing.T) {
	hashtags := []string{"#golang", "x:#golang", "https://x.com/hashtag/golang", "  #tag  "}
	for _, in := range hashtags {
		if !IsTwitterHashtagSource(in) {
			t.Errorf("IsTwitterHashtagSource(%q) = false, want true", in)
		}
	}
	notHashtags := []string{"https://x.com/artist", "https://youtube.com/watch?v=1", "#", "#two words", "not a url"}
	for _, in := range notHashtags {
		if IsTwitterHashtagSource(in) {
			t.Errorf("IsTwitterHashtagSource(%q) = true, want false", in)
		}
	}
}

func TestTwSourceBaseDir(t *testing.T) {
	hashtag, err := ParseTwSource("x:#Golang")
	if err != nil {
		t.Fatal(err)
	}
	if got := hashtag.BaseDir("/out"); !strings.HasSuffix(got, "twitter/hashtags/golang") {
		t.Errorf("hashtag BaseDir = %q, want .../twitter/hashtags/golang", got)
	}
	profile, err := ParseTwSource("https://x.com/artist")
	if err != nil {
		t.Fatal(err)
	}
	if got := profile.BaseDir("/out"); !strings.HasSuffix(got, "twitter/artist") {
		t.Errorf("profile BaseDir = %q, want .../twitter/artist", got)
	}
}
