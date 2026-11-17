package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestSourceJSONShape pins the scan --json contract: field names are a
// documented compatibility surface for scripts.
func TestSourceJSONShape(t *testing.T) {
	pub := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	src := NewSource("https://x.com/user", "https://x.com/user", KindFeed, "twitter")
	src.Title = "User"
	src.Author = "user"
	item := NewItem("123", "https://x.com/user/status/123")
	item.PublishedAt = &pub
	item.Media = []MediaAsset{NewMediaAsset("https://cdn/x.mp4", MediaVideo)}
	item.Text = NewTextContent("hello", FormatPlain)
	item.RawMetadata = json.RawMessage(`{"k":1}`)
	src.Items = []Item{item}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"url", "canonical_url", "kind", "extractor", "title", "author", "items"} {
		if _, ok := m[key]; !ok {
			t.Errorf("source key %q missing: %s", key, data)
		}
	}
	im := m["items"].([]any)[0].(map[string]any)
	for _, key := range []string{"external_id", "url", "published_at", "media", "text", "raw_metadata"} {
		if _, ok := im[key]; !ok {
			t.Errorf("item key %q missing: %s", key, data)
		}
	}

	var back Source
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src, back) {
		t.Fatalf("round trip changed source:\n%+v\n%+v", src, back)
	}
}

func TestNewSourceCanonicalFallback(t *testing.T) {
	src := NewSource("https://x.com/user", "", KindSingle, "twitter")
	if src.CanonicalURL != src.URL {
		t.Fatalf("CanonicalURL = %q, want fallback to URL", src.CanonicalURL)
	}
	src = NewSource("u", "c", KindPlaylist, "yt-dlp")
	if src.CanonicalURL != "c" || src.Kind != KindPlaylist || src.Extractor != "yt-dlp" {
		t.Fatalf("source = %+v", src)
	}
}

func TestConstructors(t *testing.T) {
	item := NewItem("id", "url")
	if item.ExternalID != "id" || item.URL != "url" {
		t.Fatalf("item = %+v", item)
	}
	media := NewMediaAsset("url", MediaImage)
	if media.URL != "url" || media.Kind != MediaImage {
		t.Fatalf("media = %+v", media)
	}
	text := NewTextContent("body", FormatMarkdown)
	if text.Body != "body" || text.Format != FormatMarkdown {
		t.Fatalf("text = %+v", text)
	}
}

func TestErrorFormattingAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := NewError(ErrAuth, "cookies required", cause)
	if got := e.Error(); got != "[auth] cookies required: boom" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(e, cause) {
		t.Fatal("Unwrap chain broken")
	}
	bare := NewError(ErrRateLimit, "slow down", nil)
	if got := bare.Error(); got != "[rate_limit] slow down" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestErrorIsByCategory(t *testing.T) {
	e := NewError(ErrAuth, "cookies required", nil)
	if !errors.Is(e, &Error{Category: ErrAuth}) {
		t.Fatal("same category should match")
	}
	if errors.Is(e, &Error{Category: ErrRateLimit}) {
		t.Fatal("different category should not match")
	}
	// A wrapped standard error stays reachable through the cause chain.
	wrapped := NewError(ErrUnavailable, "canceled", context.Canceled)
	if !errors.Is(wrapped, context.Canceled) {
		t.Fatal("cause chain should reach context.Canceled")
	}
}
