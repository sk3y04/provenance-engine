package citation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

func TestFormatParseRoundTrip(t *testing.T) {
	cases := []struct {
		collection, revision, entity string
	}{
		{"saved-media", "abc123", "https://example.com/a.jpg"},
		{"docs", "deadbeef", "https://medium.com/@user/article"},
		{"web", "rev1", "https://example.com/page#text"},
		{"web", "rev1", "https://example.com/p?x=1#frag"},
	}
	for _, tc := range cases {
		s := Format(tc.collection, tc.revision, tc.entity)
		ref, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if ref.Collection != tc.collection || ref.RevisionID != tc.revision || ref.EntityID != tc.entity {
			t.Fatalf("round trip = %+v, want %q %q %q", ref, tc.collection, tc.revision, tc.entity)
		}
		if ref.String() != s {
			t.Fatalf("String() = %q, want %q", ref.String(), s)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"https://example.com",
		"provenance://no-separators",
		"provenance://collection@revision",
		"provenance://@rev#entity",
		"provenance://col@#entity",
		"provenance://col@rev#",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", s)
		}
	}
}

func TestMarkdownTitleFallback(t *testing.T) {
	ref := &Reference{Collection: "c", RevisionID: "r", EntityID: "e"}
	if got := ref.Markdown(); got != "[e](provenance://c@r#e)" {
		t.Fatalf("entity fallback = %q", got)
	}
	ref.URL = "https://example.com"
	if got := ref.Markdown(); got != "[https://example.com](provenance://c@r#e)" {
		t.Fatalf("url fallback = %q", got)
	}
	ref.Title = "A Title"
	if got := ref.Markdown(); got != "[A Title](provenance://c@r#e)" {
		t.Fatalf("title = %q", got)
	}
}

func TestPlain(t *testing.T) {
	ref := &Reference{Collection: "c", RevisionID: "r", EntityID: "e"}
	if got := ref.Plain(); got != "provenance://c@r#e" {
		t.Fatalf("bare plain = %q", got)
	}
	ref.Title = "Title"
	ref.URL = "https://example.com"
	ref.Author = "alice"
	ref.Sha256 = "abcd"
	ref.CapturedAt = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	got := ref.Plain()
	for _, want := range []string{"Title", "https://example.com", "captured 2026-11-18", "provenance://c@r#e", "by alice", "SHA-256: abcd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain missing %q:\n%s", want, got)
		}
	}
}

func TestFromEntity(t *testing.T) {
	captured := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	ent := &archive.Entity{
		ExternalID: "https://example.com/a",
		Title:      "A",
		URL:        "https://example.com/a",
		Author:     "alice",
		CapturedAt: captured,
		Artifacts:  []string{"sha-first", "sha-second"},
	}
	ref := FromEntity("col", "rev", ent)
	if ref.Collection != "col" || ref.RevisionID != "rev" || ref.EntityID != ent.ExternalID {
		t.Fatalf("reference = %+v", ref)
	}
	if ref.Sha256 != "sha-first" {
		t.Fatalf("Sha256 = %q, want the first artifact", ref.Sha256)
	}
	if !ref.CapturedAt.Equal(captured) || ref.Author != "alice" || ref.Title != "A" {
		t.Fatalf("metadata not mapped: %+v", ref)
	}

	bare := FromEntity("col", "rev", &archive.Entity{ExternalID: "e"})
	if bare.Sha256 != "" {
		t.Fatalf("Sha256 = %q, want empty without artifacts", bare.Sha256)
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789abcdef0123", "entity"); got != "0123456789ab@entity" {
		t.Fatalf("Short = %q", got)
	}
	if got := Short("abc", "entity"); got != "abc@entity" {
		t.Fatalf("Short short revision = %q", got)
	}
}

func TestToJSONRoundTrip(t *testing.T) {
	ref := &Reference{
		Collection: "col", RevisionID: "rev", EntityID: "e",
		Title: "T", URL: "https://example.com", Author: "a",
		CapturedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
		Sha256:     "abcd",
	}
	data, err := ref.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Reference
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Collection != ref.Collection || back.EntityID != ref.EntityID || back.Sha256 != ref.Sha256 || !back.CapturedAt.Equal(ref.CapturedAt) {
		t.Fatalf("round trip = %+v", back)
	}
}
