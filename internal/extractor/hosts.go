package extractor

import "strings"

// HostMatchesDomain reports whether host is exactly domain or a subdomain of
// it (for example "www.x.com" for "x.com"). Host must be a bare hostname as
// returned by url.URL.Hostname().
//
// Substring matching is unsafe here: a lookalike host such as
// "instagram.com.example.net" would be routed to the native extractor, which
// then silently resolves a different resource than the URL points at (and a
// hostile host could be used to disguise which platform a link targets).
func HostMatchesDomain(host, domain string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}
