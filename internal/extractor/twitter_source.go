package extractor

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"
)

// TwSourceKind identifies the two supported native X/Twitter source families.
type TwSourceKind int

const (
	// TwSourceProfile is a user profile timeline: https://x.com/<user>.
	TwSourceProfile TwSourceKind = iota
	// TwSourceHashtag is a hashtag search: #tag, x:#tag, or /hashtag/tag.
	TwSourceHashtag
)

// TwSource is a normalized X/Twitter source. Profile sources carry a username;
// hashtag sources carry the original tag, the GraphQL search query, a
// filesystem-safe slug, and a stable canonical identity (x:#tag) so equivalent
// spellings map to one source.
type TwSource struct {
	Kind      TwSourceKind
	Username  string
	Hashtag   string // original tag as supplied, without the leading '#'
	Query     string // GraphQL rawQuery, e.g. "#golang filter:media"
	Slug      string // filesystem-safe, lowercased tag
	Canonical string // profile: https://x.com/<user>; hashtag: x:#<tag>
}

// BaseDir returns the native output directory for this source:
//
//	<outDir>/twitter/<username>            (profile)
//	<outDir>/twitter/hashtags/<slug>       (hashtag)
func (s TwSource) BaseDir(outDir string) string {
	if s.Kind == TwSourceHashtag {
		return filepath.Join(outDir, "twitter", "hashtags", s.Slug)
	}
	return filepath.Join(outDir, "twitter", s.Username)
}

// twHashtagQuerySuffix narrows a hashtag search to posts with native media.
const twHashtagQuerySuffix = " filter:media"

// twValidHashtagRune reports whether r may appear in an X hashtag. X permits
// letters (including non-ASCII), digits, combining marks, and underscore.
func twValidHashtagRune(r rune) bool {
	if r == '_' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// twNormalizeHashtag validates a hashtag value (without the leading '#') and
// returns it trimmed. It accepts non-ASCII tags but rejects empty values,
// whitespace, and any punctuation/operator that could be smuggled into a
// search query (e.g. `#tag OR #other`, `#tag filter:media`).
func twNormalizeHashtag(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", fmt.Errorf("empty hashtag: expected a non-empty tag such as #golang")
	}
	for _, r := range tag {
		if !twValidHashtagRune(r) {
			return "", fmt.Errorf("invalid hashtag %q: %q is not allowed (use letters, digits, '_' or non-ASCII letters)", tag, r)
		}
	}
	return tag, nil
}

// twSlug lowers and filesystem-sanitizes a validated hashtag.
func twSlug(tag string) string {
	return strings.ToLower(sanitizeFilename(strings.ToLower(tag)))
}

// ParseTwSource parses any supported X/Twitter source into a normalized
// TwSource. It handles:
//
//	https://x.com/<user>              (profile, also twitter.com)
//	#tag                              (hashtag, must be quoted in a shell)
//	x:#tag                            (canonical hashtag shorthand)
//	https://x.com/hashtag/<tag>       (hashtag URL)
func ParseTwSource(raw string) (TwSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return TwSource{}, fmt.Errorf("empty twitter source")
	}

	// Canonical hashtag shorthand: x:#tag (case-insensitive prefix).
	if rest, ok := trimPrefixFold(raw, "x:#"); ok {
		return newTwHashtagSource(rest)
	}

	// Bare hashtag. An unquoted '#' starts a shell comment, so docs require
	// quotes; anything reaching here with a leading '#' is a hashtag.
	if strings.HasPrefix(raw, "#") {
		return newTwHashtagSource(raw[1:])
	}

	// URL forms (profile or /hashtag/<tag>). Bare hashtags never reach
	// url.Parse; they are handled above.
	u, err := url.Parse(raw)
	if err != nil {
		return TwSource{}, fmt.Errorf("parse twitter source %q: %w", raw, err)
	}
	host := strings.ToLower(u.Host)
	if !strings.Contains(host, "x.com") && !strings.Contains(host, "twitter") {
		return TwSource{}, fmt.Errorf("not a twitter/x source: %s", raw)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && strings.EqualFold(parts[0], "hashtag") {
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			return TwSource{}, fmt.Errorf("malformed hashtag url %q: expected https://x.com/hashtag/<tag>", raw)
		}
		decoded, err := url.PathUnescape(parts[1])
		if err != nil {
			return TwSource{}, fmt.Errorf("decode hashtag from %q: %w", raw, err)
		}
		return newTwHashtagSource(decoded)
	}
	if len(parts) == 0 || parts[0] == "" {
		return TwSource{}, fmt.Errorf("not a twitter profile url: %s", raw)
	}
	username := sanitizeFilename(parts[0])
	if username == "" {
		return TwSource{}, fmt.Errorf("not a twitter profile url: %s", raw)
	}
	return TwSource{
		Kind:      TwSourceProfile,
		Username:  username,
		Canonical: fmt.Sprintf("https://x.com/%s", username),
	}, nil
}

func newTwHashtagSource(rawTag string) (TwSource, error) {
	tag, err := twNormalizeHashtag(rawTag)
	if err != nil {
		return TwSource{}, err
	}
	slug := twSlug(tag)
	if slug == "" {
		return TwSource{}, fmt.Errorf("invalid hashtag %q: no filesystem-safe characters", tag)
	}
	return TwSource{
		Kind:      TwSourceHashtag,
		Hashtag:   tag,
		Query:     "#" + tag + twHashtagQuerySuffix,
		Slug:      slug,
		Canonical: "x:#" + strings.ToLower(tag),
	}, nil
}

// IsTwitterHashtagSource reports whether raw is any accepted hashtag spelling
// (#tag, x:#tag, or an x.com/hashtag/<tag> URL). It is used by dispatcher
// classification before generic URL host classification, so bare hashtags are
// never routed through url.Parse host logic.
func IsTwitterHashtagSource(raw string) bool {
	src, err := ParseTwSource(raw)
	return err == nil && src.Kind == TwSourceHashtag
}

// trimPrefixFold is strings.TrimPrefix with ASCII case folding.
func trimPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return s, false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}
