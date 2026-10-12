// Package redact turns untrusted diagnostic text into a value that is safe to
// carry in a structured event or a log line. It never invents facts: it only
// shortens, normalizes, and removes secret-bearing fragments.
//
// Events must never carry raw platform response bodies, cookies, authorization
// headers, tokens, or URLs with query strings, because a hosted caller persists
// and forwards event payloads.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// MaxDetailLen bounds a Detail so a hostile or oversized response body cannot
// be smuggled through an event.
const MaxDetailLen = 200

// secretPattern matches cookie, authorization, bearer, token, and session
// fragments that commonly appear in diagnostic previews.
var secretPattern = regexp.MustCompile(`(?i)(cookie|authorization|set-cookie|bearer|token|sessionid|csrftoken|xs_token|auth_token|access_token)[=:]\s*([^;\s]{1,200})`)

// Detail normalizes s for use as an event Detail. Raw response bodies are never
// acceptable, so callers pass a short fact or a sanitized preview.
func Detail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = redactSecrets(s)
	s = redactURLs(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > MaxDetailLen {
		s = s[:MaxDetailLen] + "..."
	}
	return s
}

// URL strips the query string and fragment, which are the parts of a source URL
// that most often carry tokens, signed expiry parameters, or session state.
func URL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactSecrets(raw)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func redactSecrets(s string) string {
	return secretPattern.ReplaceAllString(s, "$1=<redacted>")
}

func redactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, func(match string) string {
		return URL(match)
	})
}

var urlPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)
