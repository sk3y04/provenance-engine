package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sk3y04/provenance-engine/internal/downloader"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
	"github.com/sk3y04/provenance-engine/internal/resolve"
	"github.com/sk3y04/provenance-engine/internal/worker"
)

const (
	igAPIBase           = "https://i.instagram.com/api/v1"
	igWWWAPIBase        = "https://www.instagram.com/api/v1"
	igAppID             = "936619743392459"
	igPageSize          = 12
	igMaxAttempts       = 4
	igErrorPreviewLimit = 4 << 10
	igRetryBackoff      = 3 * time.Second
	igMaxRetryBackoff   = 20 * time.Second
)

// igAPIHosts are the REST API hosts, tried in order. Instagram serves the
// same /api/v1 endpoints on both, but session handling differs: i.instagram.com
// rate-limits and challenges API sessions more aggressively, and browser
// cookie exports often carry only www.instagram.com host cookies that do not
// apply to i.instagram.com. Fetchers fall through to the next host when one
// rejects the session (401/403/404); rate limits are session-wide, so 429s do
// not trigger a fall-through.
var igAPIHosts = []string{igAPIBase, igWWWAPIBase}

// igSessionRejected reports whether an API error looks like the host rejecting
// the session or endpoint rather than rate-limiting it, in which case the
// other API host may still accept the request.
func igSessionRejected(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "status 404")
}

// igNoteLoginState prints a hint when an API response is an HTML login page.
// A valid browser session does not help here: Instagram's private API hosts
// reject web sessions even when the cookies are present, so the extractor
// falls back to the server-rendered page where possible.
func igNoteLoginState(ctx context.Context, base, body string) {
	if strings.Contains(body, "not-logged-in") {
		narratef(ctx, kindWarn, event.ReasonNotLoggedIn, "", "", "[instagram] %s served a logged-out page (private API rejects browser sessions); falling back to the server-rendered web page where possible\n", base)
	}
}

// igLooksLikeHTML reports whether a response body that should be JSON is
// actually an HTML page (bot challenge, error page, or login wall).
func igLooksLikeHTML(contentType string, body string) bool {
	if strings.Contains(contentType, "application/json") {
		return false
	}
	trimmed := strings.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// igAPIHostFromBase extracts the rate-limit key (host) from an API base URL.
func igAPIHostFromBase(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

const igEncodingChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

var igUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func igAppIDFromEnv() string {
	if id := os.Getenv("INSTAGRAM_APP_ID"); id != "" {
		return id
	}
	return igAppID
}

func igShortcodeToID(shortcode string) int64 {
	var id int64
	for _, c := range shortcode {
		idx := strings.IndexRune(igEncodingChars, c)
		if idx < 0 {
			return 0
		}
		// Media ids are signed 64-bit values; a code whose value would
		// overflow cannot name a real post. Returning 0 marks it invalid so
		// the caller falls back to the server-rendered post page.
		if id > (math.MaxInt64-int64(idx))/64 {
			return 0
		}
		id = id*64 + int64(idx)
	}
	return id
}

type IgOptions struct {
	CookiesFile  string
	Filter       manifest.FilterOptions
	SpeedLimit   int64
	Progress     downloader.ProgressReporter
	Limit        int
	RateLimiter  *ratelimit.Manager
	IncludePosts bool
	// Events, when set, receives presentation-neutral structured events. When
	// set, this path does not write to the terminal.
	Events event.Sink
	// Deps are the engine-owned dependencies for this operation.
	Deps *Deps
}

type IgTarget struct {
	Type        string // "profile", "post", "reel", "stories", "highlight"
	Username    string
	Shortcode   string // only for single post/reel
	HighlightID string // only for /stories/highlights/<id>/
}

func ParseIgURL(rawURL string) (IgTarget, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return IgTarget{}, fmt.Errorf("parse url: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if !HostMatchesDomain(host, "instagram.com") {
		return IgTarget{}, fmt.Errorf("not an instagram url: %s", rawURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return IgTarget{}, fmt.Errorf("not a valid instagram url: %s", rawURL)
	}

	for i, p := range parts {
		switch strings.ToLower(p) {
		case "p", "tv":
			if i+1 < len(parts) && parts[i+1] != "" {
				return IgTarget{Type: "post", Shortcode: parts[i+1]}, nil
			}
		case "reel", "reels":
			if i+1 < len(parts) && parts[i+1] != "" {
				return IgTarget{Type: "reel", Shortcode: parts[i+1]}, nil
			}
		case "stories":
			if i+1 < len(parts) {
				// /stories/highlights/<id>/ is a highlight, not a user's active
				// stories; defer to the "highlights" case below instead of
				// mistaking "highlights" for a username.
				if parts[i+1] == "highlights" {
					continue
				}
				if parts[i+1] != "" {
					return IgTarget{Type: "stories", Username: parts[i+1]}, nil
				}
			}
		case "highlights":
			if i+1 < len(parts) && parts[i+1] != "" {
				return IgTarget{Type: "highlight", HighlightID: parts[i+1]}, nil
			}
		}
	}

	name := parts[0]
	if strings.HasPrefix(name, "?") || name == "explore" || name == "share" {
		return IgTarget{}, fmt.Errorf("unsupported instagram url type: %s", rawURL)
	}
	return IgTarget{Type: "profile", Username: name}, nil
}

func igClient(d *Deps, cookiesFile string) (string, error) {
	if cookiesFile == "" {
		return "", fmt.Errorf("cookies file is required for Instagram (sessionid cookie needed; export from browser with an extension)")
	}
	cookies, err := loadNetscapeCookies(cookiesFile)
	if err != nil {
		return "", fmt.Errorf("load cookies: %w", err)
	}
	var csrfToken string
	for _, ck := range cookies {
		if strings.EqualFold(ck.Name, "csrftoken") {
			csrfToken = ck.Value
			break
		}
	}
	return csrfToken, nil
}

func igHeaders(cookiesFile, csrfToken string) (http.Header, error) {
	h := http.Header{}
	h.Set("User-Agent", igUserAgent)
	h.Set("X-IG-App-ID", igAppIDFromEnv())
	h.Set("X-ASBD-ID", "359341")
	h.Set("X-IG-WWW-Claim", "0")
	h.Set("Accept", "*/*")
	h.Set("Content-Type", "application/json")
	if csrfToken != "" {
		h.Set("X-CSRFToken", csrfToken)
	}

	cv, err := cookieHeaderForHosts(cookiesFile, "www.instagram.com", "i.instagram.com")
	if err != nil {
		return nil, err
	}
	if cv != "" {
		h.Set("Cookie", cv)
	}
	return h, nil
}

// igFlexString accepts either a JSON string or number. Instagram's REST API
// returns numeric pks while the server-rendered web page and GraphQL payloads
// quote them as strings.
type igFlexString string

func (s *igFlexString) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" || raw == "null" {
		*s = ""
		return nil
	}
	if raw[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = igFlexString(v)
		return nil
	}
	*s = igFlexString(raw)
	return nil
}

func (s igFlexString) String() string { return string(s) }

type igUserResult struct {
	Pk       igFlexString `json:"pk"`
	ID       string       `json:"id"`
	Username string       `json:"username"`
	FullName string       `json:"full_name"`
}

type igProfileResponse struct {
	Data struct {
		User igUserResult `json:"user"`
	} `json:"data"`
}

type igMediaCandidate struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type igImageVersions struct {
	Candidates []igMediaCandidate `json:"candidates"`
}

type igVideoVersion struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Type   int    `json:"type"`
}

type igCaption struct {
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

type igCarouselMedia struct {
	ImageVersions2 *igImageVersions `json:"image_versions2"`
	VideoVersions  []igVideoVersion `json:"video_versions"`
	MediaType      int              `json:"media_type"`
}

type igFeedMedia struct {
	ID             string            `json:"id"`
	Code           string            `json:"code"`
	TakenAt        int64             `json:"taken_at"`
	MediaType      int               `json:"media_type"`
	Caption        *igCaption        `json:"caption"`
	ImageVersions2 *igImageVersions  `json:"image_versions2"`
	VideoVersions  []igVideoVersion  `json:"video_versions"`
	CarouselMedia  []igCarouselMedia `json:"carousel_media"`
	User           igUserResult      `json:"user"`
}

type igFeedResponse struct {
	Items         []igFeedMedia `json:"items"`
	NextMaxID     string        `json:"next_max_id"`
	MoreAvailable bool          `json:"more_available"`
}

type igMediaInfoResponse struct {
	Items []igFeedMedia `json:"items"`
}

// igWebFeedObject is the subset of Instagram's server-rendered payloads used by
// the web fallback. Profile pages carry a feed connection with edges; single
// post pages carry an items array. Both wrap the same node shape.
type igWebFeedObject struct {
	Edges []struct {
		Node *igFeedMedia `json:"node"`
	} `json:"edges"`
	Items    []igFeedMedia `json:"items"`
	PageInfo struct {
		EndCursor   string `json:"end_cursor"`
		HasNextPage bool   `json:"has_next_page"`
	} `json:"page_info"`
}

func resolveIgUserID(ctx context.Context, d *Deps, cookiesFile, csrfToken, username string, rl *ratelimit.Manager) (string, error) {
	client := d.HTTPClient
	narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] resolving user @%s ...\n", username)

	params := url.Values{}
	params.Set("username", username)
	endpoint := fmt.Sprintf("%s/users/web_profile_info/?%s", igAPIBase, params.Encode())
	referer := fmt.Sprintf("https://www.instagram.com/%s/", username)

	var lastErr error
	for attempt := 1; attempt <= igMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", fmt.Errorf("new request: %w", err)
		}
		headers, err := igHeaders(cookiesFile, csrfToken)
		if err != nil {
			return "", err
		}
		headers.Set("Referer", referer)
		req.Header = headers

		if rl != nil {
			_ = rl.GetLimiter("i.instagram.com").Wait(ctx)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("http: %w", err)
			}
			lastErr = fmt.Errorf("http: %w", err)
			if attempt == igMaxAttempts {
				break
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return "", err
			}
			continue
		}

		if resp.StatusCode >= 400 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
			_ = resp.Body.Close()
			body := string(preview)
			lastErr = fmt.Errorf("instagram status %d", resp.StatusCode)
			narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("status %d", resp.StatusCode), "[instagram] response: %s\n", sanitizeErrorBody(body))
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, capped := retryAfterDelay(resp.Header.Get("Retry-After"), igMaxRetryBackoff)
				if capped {
					narratef(ctx, kindWarn, event.ReasonRetryAfterCapped, "", fmt.Sprintf("retry-after capped to %s", delay),
						"[instagram] Retry-After exceeds %s cap, waiting %v...\n", maxRetryAfter, delay)
				}
				narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("waiting %s", delay), "[instagram] rate limited (429), waiting %v...\n", delay)
				if err := sleepCtx(ctx, delay); err != nil {
					return "", err
				}
				continue
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == igMaxAttempts {
				break
			}
			if resp.StatusCode >= 500 && attempt < igMaxAttempts {
				if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
					return "", err
				}
				continue
			}
			break
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("read body: %w", err)
		}

		var result igProfileResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", fmt.Errorf("decode: %w", err)
		}
		userID := result.Data.User.Pk.String()
		if userID == "" {
			userID = result.Data.User.ID
		}
		if userID == "" {
			narratef(ctx, kindInfo, reasonInfo, "", "empty user id", "[instagram] web_profile_info returned empty user id, response: %s\n", sanitizeErrorBody(string(respBody[:min(len(respBody), 512)])))
			lastErr = fmt.Errorf("instagram: user not found: %s", username)
			break
		}
		return userID, nil
	}

	narratef(ctx, kindInfo, reasonInfo, "", "api lookup failed, trying web scrape", "[instagram] api lookup failed (%v), trying web scrape ...\n", lastErr)

	if userID, err := resolveIgUserIDFromWeb(ctx, d, cookiesFile, username); err == nil {
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] resolved user @%s -> %s via web scrape\n", username, userID)
		return userID, nil
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "web scrape failed", "[instagram] web scrape failed: %v\n", err)
	}

	narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] trying www api host ...\n")

	if userID, err := resolveIgUserIDWWW(ctx, d, cookiesFile, csrfToken, username); err == nil {
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] resolved user @%s -> %s via www api\n", username, userID)
		return userID, nil
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "www api failed", "[instagram] www api failed: %v\n", err)
	}

	narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] trying json endpoint ...\n")

	if userID, err := resolveIgUserIDJSON(ctx, d, cookiesFile, csrfToken, username); err == nil {
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] resolved user @%s -> %s via json endpoint\n", username, userID)
		return userID, nil
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "json endpoint failed", "[instagram] json endpoint failed: %v\n", err)
	}

	narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] trying users/search ...\n")

	if userID, err := resolveIgUserIDSearch(ctx, d, cookiesFile, csrfToken, username); err == nil {
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] resolved user @%s -> %s via users/search\n", username, userID)
		return userID, nil
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "users/search failed", "[instagram] users/search failed: %v\n", err)
	}

	return "", fmt.Errorf("resolve user id: all methods failed; api: %w", lastErr)
}

func resolveIgUserIDWWW(ctx context.Context, d *Deps, cookiesFile, csrfToken, username string) (string, error) {
	client := d.HTTPClient
	wwwBase := "https://www.instagram.com/api/v1"
	params := url.Values{}
	params.Set("username", username)
	endpoint := fmt.Sprintf("%s/users/web_profile_info/?%s", wwwBase, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	headers, err := igHeaders(cookiesFile, csrfToken)
	if err != nil {
		return "", err
	}
	headers.Set("Referer", fmt.Sprintf("https://www.instagram.com/%s/", username))
	req.Header = headers

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("www api request: %w", err)
	}

	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
		_ = resp.Body.Close()
		return "", fmt.Errorf("www api status %d: %s", resp.StatusCode, sanitizeErrorBody(string(preview)))
	}

	var result igProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		_ = resp.Body.Close()
		return "", fmt.Errorf("www api decode: %w", err)
	}
	_ = resp.Body.Close()
	if result.Data.User.Pk.String() == "" && result.Data.User.ID == "" {
		return "", fmt.Errorf("www api returned empty user id")
	}
	userID := result.Data.User.Pk.String()
	if userID == "" {
		userID = result.Data.User.ID
	}
	return userID, nil
}

// igBrowserHeaders builds the headers a top-level browser navigation sends to
// www.instagram.com. Instagram only server-renders profile and post data (as
// opposed to an empty app shell) when the Sec-Fetch-* navigation headers are
// present, so these matter even outside a real browser.
func igBrowserHeaders(cookiesFile string, hosts ...string) (http.Header, error) {
	h := http.Header{}
	h.Set("User-Agent", igUserAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
	if len(hosts) == 0 {
		hosts = []string{"www.instagram.com"}
	}
	cv, err := cookieHeaderForHosts(cookiesFile, hosts...)
	if err != nil {
		return nil, err
	}
	if cv != "" {
		h.Set("Cookie", cv)
	}
	return h, nil
}

// fetchIgHTMLPage fetches a www.instagram.com page as a browser navigation and
// returns the server-rendered HTML.
func fetchIgHTMLPage(ctx context.Context, d *Deps, cookiesFile, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	headers, err := igBrowserHeaders(cookiesFile, "www.instagram.com")
	if err != nil {
		return "", err
	}
	req.Header = headers

	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch page: %w", err)
	}
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
		_ = resp.Body.Close()
		return "", fmt.Errorf("page status %d: %s", resp.StatusCode, sanitizeErrorBody(string(preview)))
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("read page: %w", err)
	}
	narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] web page: status %d, body length %d\n", resp.StatusCode, len(body))
	return string(body), nil
}

// igExtractJSONObject returns the JSON object that follows key in html. The
// object is located by brace matching so braces and escapes inside string
// values do not truncate it.
func igExtractJSONObject(html, key string) (string, bool) {
	idx := strings.Index(html, key)
	if idx < 0 {
		return "", false
	}
	idx += len(key)
	for idx < len(html) && (html[idx] == ' ' || html[idx] == '\n' || html[idx] == '\r' || html[idx] == '\t') {
		idx++
	}
	if idx >= len(html) || html[idx] != '{' {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := idx; i < len(html); i++ {
		c := html[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[idx : i+1], true
			}
		}
	}
	return "", false
}

// digitsAfter returns the run of digits immediately following the first
// occurrence of marker in s.
func digitsAfter(s, marker string) string {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(marker):]
	var b strings.Builder
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		b.WriteByte(byte(c))
	}
	return b.String()
}

func extractIgProfileID(html string) string {
	// The server-rendered profile route embeds the owner id in page logging
	// params. Prefer it over the generic "user_id" key, which may belong to a
	// suggested account or to the viewer.
	if id := digitsAfter(html, `"profile_id":"`); id != "" {
		return id
	}
	if obj, ok := igExtractJSONObject(html, `"xdt_api__v1__feed__user_timeline_graphql_connection":`); ok {
		var conn igWebFeedObject
		if err := json.Unmarshal([]byte(obj), &conn); err == nil {
			for _, edge := range conn.Edges {
				if edge.Node != nil && edge.Node.User.Pk.String() != "" {
					return edge.Node.User.Pk.String()
				}
			}
		}
	}
	return extractIgUserID(html)
}

func resolveIgUserIDFromWeb(ctx context.Context, d *Deps, cookiesFile, username string) (string, error) {
	html, err := fetchIgHTMLPage(ctx, d, cookiesFile, fmt.Sprintf("https://www.instagram.com/%s/", username))
	if err != nil {
		return "", err
	}

	if userID := extractIgProfileID(html); userID != "" {
		return userID, nil
	}

	if strings.Contains(html, `"native_client_redesign"`) || strings.Contains(html, `"entry_data"`) {
		narratef(ctx, kindInfo, reasonInfo, "", "profile id patterns not matched", "[instagram] web scrape: page structure looks like profile but no ID patterns matched, preview:\n%s\n", sanitizeErrorBody(html[:min(len(html), 1024)]))
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "not a profile page", "[instagram] web scrape: page does not look like a profile page (possibly login/challenge wall), preview:\n%s\n", sanitizeErrorBody(html[:min(len(html), 1024)]))
	}

	return "", fmt.Errorf("could not find user id in profile page html")
}

// parseIgProfilePosts extracts the first page of the feed embedded in a
// server-rendered profile page.
func parseIgProfilePosts(html string) ([]igFeedMedia, bool, error) {
	obj, ok := igExtractJSONObject(html, `"xdt_api__v1__feed__user_timeline_graphql_connection":`)
	if !ok {
		return nil, false, fmt.Errorf("profile page did not embed a feed connection (possibly a login/challenge wall)")
	}
	var conn igWebFeedObject
	if err := json.Unmarshal([]byte(obj), &conn); err != nil {
		return nil, false, fmt.Errorf("decode embedded feed: %w", err)
	}

	posts := make([]igFeedMedia, 0, len(conn.Edges))
	for _, edge := range conn.Edges {
		if edge.Node != nil {
			posts = append(posts, *edge.Node)
		}
	}
	if len(posts) == 0 {
		return nil, conn.PageInfo.HasNextPage, fmt.Errorf("profile page embedded no posts")
	}
	return posts, conn.PageInfo.HasNextPage, nil
}

// fetchIgProfilePostsFromWeb reads the posts embedded in the server-rendered
// profile page. Instagram's REST endpoints now reject web sessions, but the
// navigation response still contains the first page of the Relay feed.
func fetchIgProfilePostsFromWeb(ctx context.Context, d *Deps, cookiesFile, username string) ([]igFeedMedia, bool, error) {
	html, err := fetchIgHTMLPage(ctx, d, cookiesFile, fmt.Sprintf("https://www.instagram.com/%s/", username))
	if err != nil {
		return nil, false, err
	}
	return parseIgProfilePosts(html)
}

func parseIgPostPage(html string) (*igFeedMedia, error) {
	obj, ok := igExtractJSONObject(html, `"xdt_api__v1__media__shortcode__web_info":`)
	if !ok {
		return nil, fmt.Errorf("post page did not embed media info")
	}
	var info igWebFeedObject
	if err := json.Unmarshal([]byte(obj), &info); err != nil {
		return nil, fmt.Errorf("decode embedded media info: %w", err)
	}
	if len(info.Items) == 0 {
		return nil, fmt.Errorf("post page embedded no media")
	}
	return &info.Items[0], nil
}

// fetchIgPostFromWeb reads a single post or reel from the server-rendered
// post page.
func fetchIgPostFromWeb(ctx context.Context, d *Deps, cookiesFile, shortcode string) (*igFeedMedia, error) {
	html, err := fetchIgHTMLPage(ctx, d, cookiesFile, fmt.Sprintf("https://www.instagram.com/p/%s/", shortcode))
	if err != nil {
		return nil, err
	}
	return parseIgPostPage(html)
}

func resolveIgUserIDJSON(ctx context.Context, d *Deps, cookiesFile, csrfToken, username string) (string, error) {
	jsonURL := fmt.Sprintf("https://www.instagram.com/%s/?__a=1&__d=1", username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jsonURL, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}

	headers, err := igHeaders(cookiesFile, csrfToken)
	if err != nil {
		return "", err
	}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("Referer", fmt.Sprintf("https://www.instagram.com/%s/", username))
	req.Header = headers

	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("json endpoint request: %w", err)
	}

	if resp.StatusCode != 200 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
		_ = resp.Body.Close()
		return "", fmt.Errorf("json endpoint status %d: %s", resp.StatusCode, sanitizeErrorBody(string(preview)))
	}

	var result struct {
		GraphQL struct {
			User struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		} `json:"graphql"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		_ = resp.Body.Close()
		return "", fmt.Errorf("json endpoint decode: %w", err)
	}
	_ = resp.Body.Close()
	if result.GraphQL.User.ID != "" {
		return result.GraphQL.User.ID, nil
	}

	return "", fmt.Errorf("json endpoint returned empty user id")
}

func resolveIgUserIDSearch(ctx context.Context, d *Deps, cookiesFile, csrfToken, username string) (string, error) {
	client := d.HTTPClient
	params := url.Values{}
	params.Set("q", username)
	params.Set("count", "1")
	endpoint := fmt.Sprintf("%s/users/search/?%s", igAPIBase, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	headers, err := igHeaders(cookiesFile, csrfToken)
	if err != nil {
		return "", err
	}
	headers.Set("Referer", fmt.Sprintf("https://www.instagram.com/%s/", username))
	req.Header = headers

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request: %w", err)
	}

	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
		_ = resp.Body.Close()
		return "", fmt.Errorf("search status %d: %s", resp.StatusCode, sanitizeErrorBody(string(preview)))
	}

	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("search read body: %w", err)
	}

	var result struct {
		Users []struct {
			Pk       igFlexString `json:"pk"`
			Username string       `json:"username"`
		} `json:"users"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("search decode: %w", err)
	}

	for _, u := range result.Users {
		if strings.EqualFold(u.Username, username) {
			return u.Pk.String(), nil
		}
	}
	if len(result.Users) > 0 {
		return result.Users[0].Pk.String(), nil
	}

	return "", fmt.Errorf("search returned no results for %s", username)
}

func extractIgUserID(html string) string {
	if idx := strings.Index(html, `"profilePage_`); idx >= 0 {
		rest := html[idx+len(`"profilePage_"`):]
		var id string
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				id += string(c)
			} else {
				break
			}
		}
		if id != "" {
			return id
		}
	}

	if idx := strings.Index(html, `instagram://user?id=`); idx >= 0 {
		rest := html[idx+len(`instagram://user?id=`):]
		var id string
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				id += string(c)
			} else {
				break
			}
		}
		if id != "" {
			return id
		}
	}

	if idx := strings.Index(html, `"user_id":"`); idx >= 0 {
		rest := html[idx+len(`"user_id":"`):]
		var id string
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				id += string(c)
			} else {
				break
			}
		}
		if id != "" {
			return id
		}
	}

	return ""
}

func fetchIgMediaPage(ctx context.Context, d *Deps, cookiesFile, csrfToken, userID, cursor string, rl *ratelimit.Manager) ([]igFeedMedia, string, error) {
	var lastErr error
	for i, base := range igAPIHosts {
		items, next, err := fetchIgMediaPageHost(ctx, d, cookiesFile, csrfToken, userID, cursor, base, rl)
		if err == nil {
			return items, next, nil
		}
		lastErr = err
		if i == len(igAPIHosts)-1 || !igSessionRejected(err) {
			return nil, "", err
		}
		narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("%s rejected the session, trying %s", base, igAPIHosts[i+1]), "[instagram] %s rejected the session (%v), trying %s ...\n", base, err, igAPIHosts[i+1])
	}
	return nil, "", lastErr
}

func fetchIgMediaPageHost(ctx context.Context, d *Deps, cookiesFile, csrfToken, userID, cursor, base string, rl *ratelimit.Manager) ([]igFeedMedia, string, error) {
	client := d.HTTPClient
	params := url.Values{}
	params.Set("count", strconv.Itoa(igPageSize))
	if cursor != "" {
		params.Set("max_id", cursor)
	}
	endpoint := fmt.Sprintf("%s/feed/user/%s/?%s", base, userID, params.Encode())

	var lastErr error
	for attempt := 1; attempt <= igMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, "", fmt.Errorf("new request: %w", err)
		}
		headers, err := igHeaders(cookiesFile, csrfToken)
		if err != nil {
			return nil, "", err
		}
		req.Header = headers

		if rl != nil {
			_ = rl.GetLimiter(igAPIHostFromBase(base)).Wait(ctx)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", fmt.Errorf("http: %w", err)
			}
			lastErr = fmt.Errorf("http: %w", err)
			if attempt == igMaxAttempts {
				return nil, "", fmt.Errorf("instagram request failed after %d attempts: %w", attempt, lastErr)
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return nil, "", err
			}
			continue
		}

		if resp.StatusCode >= 400 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
			_ = resp.Body.Close()
			body := string(preview)
			lastErr = fmt.Errorf("instagram status %d", resp.StatusCode)
			narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("status %d", resp.StatusCode), "[instagram] response: %s\n", sanitizeErrorBody(body))
			igNoteLoginState(ctx, base, body)
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, capped := retryAfterDelay(resp.Header.Get("Retry-After"), igMaxRetryBackoff)
				if capped {
					narratef(ctx, kindWarn, event.ReasonRetryAfterCapped, "", fmt.Sprintf("retry-after capped to %s", delay),
						"[instagram] Retry-After exceeds %s cap, waiting %v...\n", maxRetryAfter, delay)
				}
				narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("waiting %s", delay), "[instagram] rate limited (429), waiting %v...\n", delay)
				if err := sleepCtx(ctx, delay); err != nil {
					return nil, "", err
				}
				continue
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == igMaxAttempts {
				return nil, "", lastErr
			}
			if resp.StatusCode >= 500 && attempt < igMaxAttempts {
				if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
					return nil, "", err
				}
				continue
			}
			return nil, "", lastErr
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read body: %w", err)
		}
		if igLooksLikeHTML(resp.Header.Get("Content-Type"), string(body)) {
			lastErr = fmt.Errorf("instagram %s served an HTML challenge page instead of JSON: %s", base, sanitizeErrorBody(string(body[:min(len(body), 256)])))
			narratef(ctx, kindWarn, reasonInfo, "", "", "[instagram] %s returned HTML challenge (bot block), not JSON\n", base)
			if attempt == igMaxAttempts {
				return nil, "", lastErr
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return nil, "", err
			}
			continue
		}
		var result igFeedResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, "", fmt.Errorf("decode: %w", err)
		}
		return result.Items, result.NextMaxID, nil
	}
	return nil, "", lastErr
}

func FetchAllIgPosts(ctx context.Context, d *Deps, cookiesFile, csrfToken, userID, username string, limit int, rl *ratelimit.Manager) ([]igFeedMedia, error) {
	// The private REST feed endpoint is increasingly rejected for browser
	// sessions. The profile page still server-renders the first page of the
	// same feed, so prefer it and fall back to the API only if it is missing.
	if username != "" {
		posts, more, err := fetchIgProfilePostsFromWeb(ctx, d, cookiesFile, username)
		if err == nil {
			narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] fetched %d posts from the profile page\n", len(posts))
			if more && (limit == 0 || len(posts) < limit) {
				narratef(ctx, kindWarn, reasonInfo, "", "", "[instagram] profile page only exposes the first %d posts; older posts need Instagram's authenticated API, which currently rejects web sessions\n", len(posts))
			}
			if limit > 0 && len(posts) > limit {
				posts = posts[:limit]
			}
			return posts, nil
		}
		narratef(ctx, kindInfo, reasonInfo, "", "profile page fallback failed", "[instagram] profile page fallback failed: %v\n", err)
	}

	var all []igFeedMedia
	cursor := ""
	pages := 0
	for {
		page, nextCursor, err := fetchIgMediaPage(ctx, d, cookiesFile, csrfToken, userID, cursor, rl)
		if err != nil {
			if len(all) > 0 && strings.Contains(err.Error(), "429") {
				narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] rate limited mid-scan, returning %d items from %d pages\n", len(all), pages)
				return all, nil
			}
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		pages++
		all = append(all, page...)
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] page %d: %d posts (%d total)\n", pages, len(page), len(all))
		if limit > 0 && len(all) >= limit {
			all = all[:limit]
			break
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return all, nil
}

func fetchSingleIgPost(ctx context.Context, d *Deps, cookiesFile, csrfToken, shortcode string, rl *ratelimit.Manager) (*igFeedMedia, error) {
	mediaID := igShortcodeToID(shortcode)
	if mediaID == 0 {
		return nil, fmt.Errorf("invalid instagram shortcode: %s", shortcode)
	}
	var lastErr error
	for i, base := range igAPIHosts {
		post, err := fetchSingleIgPostHost(ctx, d, cookiesFile, csrfToken, mediaID, shortcode, base, rl)
		if err == nil {
			return post, nil
		}
		lastErr = err
		if i == len(igAPIHosts)-1 || !igSessionRejected(err) {
			break
		}
		narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("%s rejected the session, trying %s", base, igAPIHosts[i+1]), "[instagram] %s rejected the session (%v), trying %s ...\n", base, err, igAPIHosts[i+1])
	}
	if post, err := fetchIgPostFromWeb(ctx, d, cookiesFile, shortcode); err == nil {
		narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] fetched post %s from the post page\n", shortcode)
		return post, nil
	} else {
		narratef(ctx, kindInfo, reasonInfo, "", "post page fallback failed", "[instagram] post page fallback failed: %v\n", err)
	}
	return nil, lastErr
}

func fetchSingleIgPostHost(ctx context.Context, d *Deps, cookiesFile, csrfToken string, mediaID int64, shortcode, base string, rl *ratelimit.Manager) (*igFeedMedia, error) {
	client := d.HTTPClient
	endpoint := fmt.Sprintf("%s/media/%d/info/", base, mediaID)

	var lastErr error
	for attempt := 1; attempt <= igMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		headers, err := igHeaders(cookiesFile, csrfToken)
		if err != nil {
			return nil, err
		}
		req.Header = headers

		if rl != nil {
			_ = rl.GetLimiter(igAPIHostFromBase(base)).Wait(ctx)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("http: %w", err)
			}
			lastErr = fmt.Errorf("http: %w", err)
			if attempt == igMaxAttempts {
				return nil, fmt.Errorf("instagram request failed after %d attempts: %w", attempt, lastErr)
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode >= 400 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
			_ = resp.Body.Close()
			body := string(preview)
			lastErr = fmt.Errorf("instagram status %d", resp.StatusCode)
			narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("status %d", resp.StatusCode), "[instagram] response: %s\n", sanitizeErrorBody(body))
			igNoteLoginState(ctx, base, body)
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, capped := retryAfterDelay(resp.Header.Get("Retry-After"), igMaxRetryBackoff)
				if capped {
					narratef(ctx, kindWarn, event.ReasonRetryAfterCapped, "", fmt.Sprintf("retry-after capped to %s", delay),
						"[instagram] Retry-After exceeds %s cap, waiting %v...\n", maxRetryAfter, delay)
				}
				narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("waiting %s", delay), "[instagram] rate limited (429), waiting %v...\n", delay)
				if err := sleepCtx(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == igMaxAttempts {
				return nil, lastErr
			}
			if resp.StatusCode >= 500 && attempt < igMaxAttempts {
				if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		}

		var result igMediaInfoResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("decode: %w", err)
		}
		_ = resp.Body.Close()
		if len(result.Items) == 0 {
			return nil, fmt.Errorf("instagram: post not found: %s", shortcode)
		}
		return &result.Items[0], nil
	}
	return nil, lastErr
}

func fetchIgStories(ctx context.Context, d *Deps, cookiesFile, csrfToken, userID string, rl *ratelimit.Manager) ([]igFeedMedia, error) {
	var lastErr error
	for i, base := range igAPIHosts {
		items, err := fetchIgStoriesHost(ctx, d, cookiesFile, csrfToken, userID, base, rl)
		if err == nil {
			return items, nil
		}
		lastErr = err
		if i == len(igAPIHosts)-1 || !igSessionRejected(err) {
			return nil, err
		}
		narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("%s rejected the session, trying %s", base, igAPIHosts[i+1]), "[instagram] %s rejected the session (%v), trying %s ...\n", base, err, igAPIHosts[i+1])
	}
	return nil, lastErr
}

func fetchIgStoriesHost(ctx context.Context, d *Deps, cookiesFile, csrfToken, userID, base string, rl *ratelimit.Manager) ([]igFeedMedia, error) {
	client := d.HTTPClient
	params := url.Values{}
	params.Set("reel_ids", userID)
	endpoint := fmt.Sprintf("%s/feed/reels_media/?%s", base, params.Encode())

	var lastErr error
	for attempt := 1; attempt <= igMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		headers, err := igHeaders(cookiesFile, csrfToken)
		if err != nil {
			return nil, err
		}
		req.Header = headers

		if rl != nil {
			_ = rl.GetLimiter(igAPIHostFromBase(base)).Wait(ctx)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("http: %w", err)
			}
			lastErr = fmt.Errorf("http: %w", err)
			if attempt == igMaxAttempts {
				return nil, fmt.Errorf("instagram stories request failed after %d attempts: %w", attempt, lastErr)
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode >= 400 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
			_ = resp.Body.Close()
			body := string(preview)
			lastErr = fmt.Errorf("instagram stories status %d", resp.StatusCode)
			narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("status %d", resp.StatusCode), "[instagram] response: %s\n", sanitizeErrorBody(body))
			igNoteLoginState(ctx, base, body)
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, capped := retryAfterDelay(resp.Header.Get("Retry-After"), igMaxRetryBackoff)
				if capped {
					narratef(ctx, kindWarn, event.ReasonRetryAfterCapped, "", fmt.Sprintf("retry-after capped to %s", delay),
						"[instagram] Retry-After exceeds %s cap, waiting %v...\n", maxRetryAfter, delay)
				}
				narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("waiting %s", delay), "[instagram] rate limited (429), waiting %v...\n", delay)
				if err := sleepCtx(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == igMaxAttempts {
				return nil, lastErr
			}
			if resp.StatusCode >= 500 && attempt < igMaxAttempts {
				if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}

		var result struct {
			Reels map[string]struct {
				Items []igFeedMedia `json:"items"`
			} `json:"reels"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}

		for _, reel := range result.Reels {
			if len(reel.Items) > 0 {
				narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] found %d stories\n", len(reel.Items))
				return reel.Items, nil
			}
		}

		return nil, fmt.Errorf("instagram: no stories found for user %s", userID)
	}
	return nil, lastErr
}

// reelEntry is one media entry inside a feed/reels_media response. A highlight
// and an active-stories reel share this shape; Instagram names the owning actor
// either "user" (web/mobile) or "owner" (some private endpoints), so both are
// captured. Usernames drive the output path so highlight content still lands
// under instagram/<username>/ even when the URL only carries a highlight id.
type reelEntry struct {
	User  igUserResult  `json:"user"`
	Owner igUserResult  `json:"owner"`
	Items []igFeedMedia `json:"items"`
}

type igReelsResponse struct {
	Reels map[string]reelEntry `json:"reels"`
}

// owningUsername returns the reel's owning username, preferring "user" and
// falling back to "owner".
func owningUsername(e reelEntry) string {
	if e.User.Username != "" {
		return e.User.Username
	}
	return e.Owner.Username
}

// pickReelItems returns the items for the requested reel key when present,
// otherwise the first reel with items (mirrors fetchIgStories' first non-empty
// reel behaviour) and the owning username of the selected reel.
func pickReelItems(reels map[string]reelEntry, reelKey string) ([]igFeedMedia, string) {
	if r, ok := reels[reelKey]; ok && len(r.Items) > 0 {
		return r.Items, owningUsername(r)
	}
	for _, r := range reels {
		if len(r.Items) > 0 {
			return r.Items, owningUsername(r)
		}
	}
	return nil, ""
}

// fetchIgHighlightStories fetches every story item within a single highlight.
//
// Instagram serves highlight reels through the same /feed/reels_media/ endpoint
// used for active stories (see fetchIgStories); the only difference is the
// reel_ids value: a highlight is addressed as "highlight:<highlight_id>" instead
// of a bare numeric user id. The response therefore has the same shape
// ({"reels": {"highlight:<id>": {"user": {...}, "items": [...]}}}), and the
// owning username is carried in the reel's user/owner object.
//
// Retry/429/rate-limit handling mirrors fetchIgStories. When the bare
// reel_ids=highlight:<id> GET yields an empty highlight for certain account/link
// combinations, a second attempt as a form POST additionally advertises
// supported_capabilities_new — the JSON-encoded capability list the web/mobile
// clients send with reels_media — which resolves it in practice.
func fetchIgHighlightStories(ctx context.Context, d *Deps, cookiesFile, csrfToken, highlightID string, rl *ratelimit.Manager) ([]igFeedMedia, string, error) {
	var lastErr error
	for i, base := range igAPIHosts {
		items, username, err := fetchIgHighlightStoriesHost(ctx, d, cookiesFile, csrfToken, highlightID, base, rl)
		if err == nil {
			return items, username, nil
		}
		lastErr = err
		if i == len(igAPIHosts)-1 || !igSessionRejected(err) {
			return nil, "", err
		}
		narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("%s rejected the session, trying %s", base, igAPIHosts[i+1]), "[instagram] %s rejected the session (%v), trying %s ...\n", base, err, igAPIHosts[i+1])
	}
	return nil, "", lastErr
}

func fetchIgHighlightStoriesHost(ctx context.Context, d *Deps, cookiesFile, csrfToken, highlightID, base string, rl *ratelimit.Manager) ([]igFeedMedia, string, error) {
	client := d.HTTPClient
	reelKey := "highlight:" + highlightID
	params := url.Values{}
	params.Set("reel_ids", reelKey)
	endpoint := fmt.Sprintf("%s/feed/reels_media/?%s", base, params.Encode())

	var lastErr error
	for attempt := 1; attempt <= igMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, "", fmt.Errorf("new request: %w", err)
		}
		headers, err := igHeaders(cookiesFile, csrfToken)
		if err != nil {
			return nil, "", err
		}
		req.Header = headers

		if rl != nil {
			_ = rl.GetLimiter(igAPIHostFromBase(base)).Wait(ctx)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", fmt.Errorf("http: %w", err)
			}
			lastErr = fmt.Errorf("http: %w", err)
			if attempt == igMaxAttempts {
				return nil, "", fmt.Errorf("instagram highlight request failed after %d attempts: %w", attempt, lastErr)
			}
			if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
				return nil, "", err
			}
			continue
		}

		if resp.StatusCode >= 400 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
			_ = resp.Body.Close()
			body := string(preview)
			lastErr = fmt.Errorf("instagram highlight status %d", resp.StatusCode)
			narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("status %d", resp.StatusCode), "[instagram] response: %s\n", sanitizeErrorBody(body))
			igNoteLoginState(ctx, base, body)
			if resp.StatusCode == http.StatusTooManyRequests {
				delay, capped := retryAfterDelay(resp.Header.Get("Retry-After"), igMaxRetryBackoff)
				if capped {
					narratef(ctx, kindWarn, event.ReasonRetryAfterCapped, "", fmt.Sprintf("retry-after capped to %s", delay),
						"[instagram] Retry-After exceeds %s cap, waiting %v...\n", maxRetryAfter, delay)
				}
				narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("waiting %s", delay), "[instagram] rate limited (429), waiting %v...\n", delay)
				if err := sleepCtx(ctx, delay); err != nil {
					return nil, "", err
				}
				continue
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == igMaxAttempts {
				return nil, "", lastErr
			}
			if resp.StatusCode >= 500 && attempt < igMaxAttempts {
				if err := sleepCtx(ctx, igRetryBackoff<<min(attempt-1, 3)); err != nil {
					return nil, "", err
				}
				continue
			}
			return nil, "", lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read body: %w", err)
		}

		var result igReelsResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, "", fmt.Errorf("decode: %w", err)
		}

		if items, username := pickReelItems(result.Reels, reelKey); len(items) > 0 {
			narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] found %d stories in highlight %s\n", len(items), highlightID)
			return items, username, nil
		}

		// Bare reel_ids=highlight:<id> occasionally returns an empty highlight.
		// A POST that also advertises supported_capabilities_new recovers it.
		if items, username, ok := fetchIgHighlightStoriesWithCaps(ctx, d, cookiesFile, csrfToken, reelKey, base, rl); ok {
			narratef(ctx, kindInfo, reasonInfo, "", "", "[instagram] found %d stories in highlight %s (capability retry)\n", len(items), highlightID)
			return items, username, nil
		}
	}
	return nil, "", fmt.Errorf("instagram: no stories found for highlight %s", highlightID)
}

// supported_capabilities_new payload, mirroring the capability list the Instagram
// web/mobile reels_media clients send. Included only in the recovery POST when a
// highlight returns empty; it is harmless to also send on the primary request.
var igHighlightCapabilities = []string{
	"android_usage_in_app_grid",
	"atv_watch_media_playback",
	"capsule_v2_creation_banner",
	"capsule_v2_footer_section",
	"capsule_v2_quick_play",
	"draggable_show_reels_stacks",
	"dynamic_group_creation",
	"fb_feed_v3",
	"ig_portrait_transitions",
	"lens_video",
	"live_thread",
	"music",
	"photos_of_you",
	"reel_24h_stories",
	"reels_captions",
	"tabbed_profile",
	"threaded_comment_history",
	"threaded_comment_control",
	"video_muti_upload",
}

// fetchIgHighlightStoriesWithCaps retries a highlight fetch as a form POST that
// additionally advertises supported_capabilities_new. Returns ok=false if the
// attempt fails or still yields no items.
func fetchIgHighlightStoriesWithCaps(ctx context.Context, d *Deps, cookiesFile, csrfToken, reelKey, base string, rl *ratelimit.Manager) ([]igFeedMedia, string, bool) {
	client := d.HTTPClient
	capsJSON, err := json.Marshal(igHighlightCapabilities)
	if err != nil {
		return nil, "", false
	}
	body := url.Values{}
	body.Set("reel_ids", reelKey)
	body.Set("supported_capabilities_new", string(capsJSON))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/feed/reels_media/", base), strings.NewReader(body.Encode()))
	if err != nil {
		return nil, "", false
	}
	headers, err := igHeaders(cookiesFile, csrfToken)
	if err != nil {
		return nil, "", false
	}
	headers.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header = headers

	if rl != nil {
		_ = rl.GetLimiter(igAPIHostFromBase(base)).Wait(ctx)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, igErrorPreviewLimit))
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", false
	}

	var result igReelsResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, "", false
	}
	items, username := pickReelItems(result.Reels, reelKey)
	if len(items) == 0 {
		return nil, "", false
	}
	return items, username, true
}

func igMediaTypeName(mediaType int) string {
	switch mediaType {
	case 1:
		return "image"
	case 2:
		return "video"
	case 8:
		return "carousel"
	}
	return "unknown"
}

func igMediaBestURLs(media igFeedMedia) []string {
	switch media.MediaType {
	case 1: // image
		if media.ImageVersions2 != nil && len(media.ImageVersions2.Candidates) > 0 {
			return []string{media.ImageVersions2.Candidates[0].URL}
		}
	case 2: // video
		if len(media.VideoVersions) > 0 {
			variants := append([]igVideoVersion(nil), media.VideoVersions...)
			sort.Slice(variants, func(i, j int) bool {
				if variants[i].Height != variants[j].Height {
					return variants[i].Height > variants[j].Height
				}
				return variants[i].Width > variants[j].Width
			})
			return []string{variants[0].URL}
		}
	case 8: // carousel
		var urls []string
		for _, cm := range media.CarouselMedia {
			switch cm.MediaType {
			case 1:
				if cm.ImageVersions2 != nil && len(cm.ImageVersions2.Candidates) > 0 {
					urls = append(urls, cm.ImageVersions2.Candidates[0].URL)
				}
			case 2:
				if len(cm.VideoVersions) > 0 {
					vs := append([]igVideoVersion(nil), cm.VideoVersions...)
					sort.Slice(vs, func(i, j int) bool {
						if vs[i].Height != vs[j].Height {
							return vs[i].Height > vs[j].Height
						}
						return vs[i].Width > vs[j].Width
					})
					urls = append(urls, vs[0].URL)
				}
			}
		}
		return urls
	}
	return nil
}

func igMediaExt(media igFeedMedia) string {
	switch media.MediaType {
	case 1:
		return "jpg"
	case 2:
		return "mp4"
	case 8:
		// carousel - check first item's type
		if len(media.CarouselMedia) > 0 && media.CarouselMedia[0].MediaType == 2 {
			return "mp4"
		}
		return "jpg"
	}
	return ""
}

func igPostMarkdown(post igFeedMedia, username string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s", username)
	if !strings.EqualFold(post.User.FullName, "") {
		fmt.Fprintf(&b, " · %s", post.User.FullName)
	}
	if post.TakenAt > 0 {
		t := time.Unix(post.TakenAt, 0).UTC()
		fmt.Fprintf(&b, " · %s", t.Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString("\n\n")
	if post.Caption != nil && post.Caption.Text != "" {
		b.WriteString(post.Caption.Text)
		b.WriteString("\n\n")
	}
	if post.Code != "" {
		fmt.Fprintf(&b, "[Source](https://www.instagram.com/p/%s/)\n", post.Code)
	}
	return []byte(b.String())
}

func igPostItems(rawURL, outDir string, posts []igFeedMedia, username string, includePosts bool) []manifest.Item {
	items := make([]manifest.Item, 0)
	for _, p := range posts {
		p := p
		base := filepath.Join(outDir, "instagram", username)

		if includePosts && p.Code != "" {
			postName := p.Code + ".md"
			title := ""
			if p.Caption != nil {
				title = p.Caption.Text
			}
			items = append(items, manifest.Item{
				ID:          p.Code + "_post",
				URL:         "post://instagram/" + p.Code,
				Title:       firstNonEmpty(title, "post_"+p.Code),
				Filename:    postName,
				Extension:   "md",
				Source:      "instagram",
				Creator:     username,
				PostID:      p.Code,
				PublishedAt: time.Unix(p.TakenAt, 0),
				Destination: filepath.Join(base, postName),
				Kind:        "post",
			})
		}

		if p.MediaType == 8 {
			for ci, cm := range p.CarouselMedia {
				urls := igCarouselBestURLs(cm)
				for _, mediaURL := range urls {
					ext := igCarouselExt(cm)
					name := fmt.Sprintf("%s_carousel_%d.%s", p.Code, ci, ext)
					items = append(items, manifest.Item{
						ID:          fmt.Sprintf("%s_cm%d", p.Code, ci),
						URL:         mediaURL,
						Title:       name,
						Filename:    name,
						Extension:   ext,
						Source:      "instagram",
						Creator:     username,
						PostID:      p.Code,
						PublishedAt: time.Unix(p.TakenAt, 0),
						Destination: filepath.Join(base, "images", sanitizeFilename(name)),
					})
				}
			}
			continue
		}

		urls := igMediaBestURLs(p)
		ext := igMediaExt(p)
		typeName := igMediaTypeName(p.MediaType)
		for i, mediaURL := range urls {
			var subdir string
			switch p.MediaType {
			case 2:
				subdir = "videos"
			case 1:
				subdir = "images"
			default:
				subdir = typeName
			}
			var name string
			if len(urls) > 1 {
				name = fmt.Sprintf("%s_%d.%s", p.Code, i, ext)
			} else {
				name = fmt.Sprintf("%s.%s", p.Code, ext)
			}
			items = append(items, manifest.Item{
				ID:          fmt.Sprintf("%s_%d", p.Code, i),
				URL:         mediaURL,
				Title:       name,
				Filename:    name,
				Extension:   ext,
				Source:      "instagram",
				Creator:     username,
				PostID:      p.Code,
				PublishedAt: time.Unix(p.TakenAt, 0),
				Destination: filepath.Join(base, subdir, sanitizeFilename(name)),
			})
		}
	}
	return items
}

func igCarouselBestURLs(cm igCarouselMedia) []string {
	switch cm.MediaType {
	case 1:
		if cm.ImageVersions2 != nil && len(cm.ImageVersions2.Candidates) > 0 {
			return []string{cm.ImageVersions2.Candidates[0].URL}
		}
	case 2:
		if len(cm.VideoVersions) > 0 {
			vs := append([]igVideoVersion(nil), cm.VideoVersions...)
			sort.Slice(vs, func(i, j int) bool {
				if vs[i].Height != vs[j].Height {
					return vs[i].Height > vs[j].Height
				}
				return vs[i].Width > vs[j].Width
			})
			return []string{vs[0].URL}
		}
	}
	return nil
}

func igCarouselExt(cm igCarouselMedia) string {
	switch cm.MediaType {
	case 1:
		return "jpg"
	case 2:
		return "mp4"
	}
	return "jpg"
}

func ScanInstagram(ctx context.Context, rawURL, outDir string, cookiesFile string, opts IgOptions) (manifest.Manifest, error) {
	d := depsFor(opts.Deps)
	ctx = event.WithSink(ctx, opts.Events)
	target, err := ParseIgURL(rawURL)
	if err != nil {
		return manifest.Manifest{}, err
	}
	csrfToken, err := igClient(d, cookiesFile)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("create client: %w", err)
	}

	var posts []igFeedMedia
	var username string

	switch target.Type {
	case "profile":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = FetchAllIgPosts(ctx, d, cookiesFile, csrfToken, userID, target.Username, opts.Limit, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, err
		}
		username = target.Username

	case "stories":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = fetchIgStories(ctx, d, cookiesFile, csrfToken, userID, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, err
		}
		username = target.Username

	case "highlight":
		posts, username, err = fetchIgHighlightStories(ctx, d, cookiesFile, csrfToken, target.HighlightID, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("fetch highlight: %w", err)
		}
		if username == "" {
			username = "highlight_" + target.HighlightID
		}

	case "post", "reel":
		post, err := fetchSingleIgPost(ctx, d, cookiesFile, csrfToken, target.Shortcode, opts.RateLimiter)
		if err != nil {
			return manifest.Manifest{}, err
		}
		posts = []igFeedMedia{*post}
		username = post.User.Username
		if username == "" {
			username = target.Username
		}
		if username == "" {
			username = sanitizeFilename(target.Shortcode)
		}
	}

	if opts.Limit > 0 && len(posts) > opts.Limit {
		posts = posts[:opts.Limit]
	}
	m := manifest.New(rawURL, "instagram", igPostItems(rawURL, outDir, posts, username, opts.IncludePosts))
	return m.Filter(opts.Filter)
}

func DownloadInstagram(ctx context.Context, rawURL, outDir string, cookiesFile string, opts IgOptions, dryRun bool) error {
	d := depsFor(opts.Deps)
	target, err := ParseIgURL(rawURL)
	if err != nil {
		return err
	}
	csrfToken, err := igClient(d, cookiesFile)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	var posts []igFeedMedia
	var username string

	switch target.Type {
	case "profile":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = FetchAllIgPosts(ctx, d, cookiesFile, csrfToken, userID, target.Username, opts.Limit, opts.RateLimiter)
		if err != nil {
			return err
		}
		username = target.Username

	case "stories":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = fetchIgStories(ctx, d, cookiesFile, csrfToken, userID, opts.RateLimiter)
		if err != nil {
			return err
		}
		username = target.Username

	case "highlight":
		posts, username, err = fetchIgHighlightStories(ctx, d, cookiesFile, csrfToken, target.HighlightID, opts.RateLimiter)
		if err != nil {
			return fmt.Errorf("fetch highlight: %w", err)
		}
		if username == "" {
			username = "highlight_" + target.HighlightID
		}

	case "post", "reel":
		post, err := fetchSingleIgPost(ctx, d, cookiesFile, csrfToken, target.Shortcode, opts.RateLimiter)
		if err != nil {
			return err
		}
		posts = []igFeedMedia{*post}
		username = post.User.Username
		if username == "" {
			username = target.Username
		}
		if username == "" {
			username = sanitizeFilename(target.Shortcode)
		}
	}

	if opts.Limit > 0 && len(posts) > opts.Limit {
		posts = posts[:opts.Limit]
	}

	items := igPostItems(rawURL, outDir, posts, username, opts.IncludePosts)
	items, err = manifest.FilterItems(items, opts.Filter)
	if err != nil {
		return err
	}
	allowed := map[string]manifest.Item{}
	postsByID := map[string]manifest.Item{}
	for _, it := range items {
		allowed[it.URL] = it
		if it.Kind == "post" {
			postsByID[it.PostID] = it
		}
	}

	pool := worker.NewPool(ctx, 3)

	failed := make(map[string]string)
	var failMu sync.Mutex
	recordFailure := func(id string, err error) {
		failMu.Lock()
		failed[id] = err.Error()
		failMu.Unlock()
		narratef(ctx, kindInfo, reasonInfo, "", fmt.Sprintf("instagram download failed: %s", id), "[provenance] instagram download failed: %s: %v\n", id, err)
	}

	for _, p := range posts {
		p := p
		base := filepath.Join(outDir, "instagram", username)

		if postItem, ok := postsByID[p.Code]; ok {
			postDest := postItem.Destination
			postContent := igPostMarkdown(p, username)
			if dryRun {
				narrateOut(ctx, kindInfo, reasonInfo, "", "", "[dry-run] instagram post: %s -> %s\n", p.Code, postDest)
			} else {
				pool.SubmitWithHooks(func() error {
					return writeFile(postDest, postContent)
				}, func() {
					narratef(ctx, kindInfo, reasonInfo, "", "", "[provenance] instagram post saved: %s\n", postDest)
				}, func(err error) {
					recordFailure("post:"+p.Code, err)
				})
			}
		}

		handleMedia := func(media igFeedMedia, allowMap map[string]manifest.Item, prefix string) {
			if media.MediaType == 8 {
				for ci, cm := range media.CarouselMedia {
					urls := igCarouselBestURLs(cm)
					ext := igCarouselExt(cm)
					for _, mediaURL := range urls {
						mediaURL := mediaURL
						if mediaURL == "" {
							continue
						}
						item, ok := allowMap[mediaURL]
						if !ok {
							continue
						}
						dest := item.Destination
						if dest == "" {
							name := fmt.Sprintf("%s_carousel_%d.%s", p.Code, ci, ext)
							dest = filepath.Join(base, "images", sanitizeFilename(name))
						}
						if dryRun {
							narrateOut(ctx, kindInfo, reasonInfo, "", "", "[dry-run] instagram: %s -> %s\n", mediaURL, dest)
							continue
						}
						furl := mediaURL
						pool.SubmitWithHooks(func() error {
							dl := downloader.New()
							dl.SpeedLimit = opts.SpeedLimit
							dl.Progress = progressFor(ctx, opts.Progress)
							return dl.Download(ctx, furl, dest, "")
						}, nil, func(err error) {
							recordFailure(furl, err)
						})
					}
				}
				return
			}

			urls := igMediaBestURLs(media)
			ext := igMediaExt(media)
			var subdir string
			switch media.MediaType {
			case 2:
				subdir = "videos"
			default:
				subdir = "images"
			}
			for i, mediaURL := range urls {
				mediaURL := mediaURL
				if mediaURL == "" {
					continue
				}
				item, ok := allowMap[mediaURL]
				if !ok {
					continue
				}
				dest := item.Destination
				if dest == "" {
					var name string
					if len(urls) > 1 {
						name = fmt.Sprintf("%s_%d.%s", p.Code, i, ext)
					} else {
						name = fmt.Sprintf("%s.%s", p.Code, ext)
					}
					dest = filepath.Join(base, subdir, sanitizeFilename(name))
				}
				if dryRun {
					narrateOut(ctx, kindInfo, reasonInfo, "", "", "[dry-run] instagram: %s -> %s\n", mediaURL, dest)
					continue
				}
				furl := mediaURL
				pool.SubmitWithHooks(func() error {
					dl := downloader.New()
					dl.SpeedLimit = opts.SpeedLimit
					dl.Progress = progressFor(ctx, opts.Progress)
					return dl.Download(ctx, furl, dest, "")
				}, nil, func(err error) {
					recordFailure(furl, err)
				})
			}
		}

		handleMedia(p, allowed, "0")
	}

	pool.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("instagram: %d file(s) failed to download (first: %s)", len(failed), firstFailedURL(failed))
	}
	return nil
}

func IgPostToItem(p igFeedMedia, username, targetType string) resolve.Item {
	var canonicalURL string
	switch targetType {
	case "reel":
		canonicalURL = fmt.Sprintf("https://instagram.com/reel/%s/", p.Code)
	case "tv":
		canonicalURL = fmt.Sprintf("https://instagram.com/tv/%s/", p.Code)
	default:
		canonicalURL = fmt.Sprintf("https://instagram.com/p/%s/", p.Code)
	}
	item := resolve.NewItem(p.Code, canonicalURL)
	item.Author = username
	if p.Caption != nil {
		item.Title = p.Caption.Text
	}
	if p.TakenAt > 0 {
		t := time.Unix(p.TakenAt, 0)
		item.PublishedAt = &t
	}
	if p.Caption != nil && p.Caption.Text != "" {
		item.Text = &resolve.TextContent{Body: p.Caption.Text, Format: resolve.FormatPlain}
	}
	if raw, err := json.Marshal(p); err == nil {
		item.RawMetadata = raw
	}

	if p.MediaType == 8 {
		for _, cm := range p.CarouselMedia {
			urls := igCarouselBestURLs(cm)
			ext := igCarouselExt(cm)
			kind := resolve.MediaImage
			if cm.MediaType == 2 {
				kind = resolve.MediaVideo
			}
			for _, mediaURL := range urls {
				asset := resolve.NewMediaAsset(mediaURL, kind)
				asset.Extension = ext
				item.Media = append(item.Media, asset)
			}
		}
		return item
	}

	urls := igMediaBestURLs(p)
	ext := igMediaExt(p)
	kind := resolve.MediaImage
	if p.MediaType == 2 {
		kind = resolve.MediaVideo
	}
	for _, mediaURL := range urls {
		asset := resolve.NewMediaAsset(mediaURL, kind)
		asset.Extension = ext
		item.Media = append(item.Media, asset)
	}
	return item
}

func ScanInstagramResolved(ctx context.Context, rawURL, outDir, cookiesFile string, opts IgOptions) (resolve.Source, error) {
	d := depsFor(opts.Deps)
	ctx = event.WithSink(ctx, opts.Events)
	target, err := ParseIgURL(rawURL)
	if err != nil {
		return resolve.Source{}, err
	}
	csrfToken, err := igClient(d, cookiesFile)
	if err != nil {
		return resolve.Source{}, fmt.Errorf("create client: %w", err)
	}

	var posts []igFeedMedia
	var username string

	switch target.Type {
	case "profile":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = FetchAllIgPosts(ctx, d, cookiesFile, csrfToken, userID, target.Username, opts.Limit, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, err
		}
		username = target.Username

	case "stories":
		userID, err := resolveIgUserID(ctx, d, cookiesFile, csrfToken, target.Username, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, fmt.Errorf("resolve user id: %w", err)
		}
		posts, err = fetchIgStories(ctx, d, cookiesFile, csrfToken, userID, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, err
		}
		username = target.Username

	case "highlight":
		posts, username, err = fetchIgHighlightStories(ctx, d, cookiesFile, csrfToken, target.HighlightID, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, fmt.Errorf("fetch highlight: %w", err)
		}
		if username == "" {
			username = "highlight_" + target.HighlightID
		}

	case "post", "reel":
		post, err := fetchSingleIgPost(ctx, d, cookiesFile, csrfToken, target.Shortcode, opts.RateLimiter)
		if err != nil {
			return resolve.Source{}, err
		}
		posts = []igFeedMedia{*post}
		username = post.User.Username
		if username == "" {
			username = target.Username
		}
		if username == "" {
			username = sanitizeFilename(target.Shortcode)
		}
	}

	if opts.Limit > 0 && len(posts) > opts.Limit {
		posts = posts[:opts.Limit]
	}
	kind := resolve.KindFeed
	canonicalURL := fmt.Sprintf("https://www.instagram.com/%s/", username)
	if target.Type == "post" || target.Type == "reel" || target.Type == "tv" {
		kind = resolve.KindSingle
		switch target.Type {
		case "reel":
			canonicalURL = fmt.Sprintf("https://www.instagram.com/reel/%s/", target.Shortcode)
		case "tv":
			canonicalURL = fmt.Sprintf("https://www.instagram.com/tv/%s/", target.Shortcode)
		default:
			canonicalURL = fmt.Sprintf("https://www.instagram.com/p/%s/", target.Shortcode)
		}
	}
	if target.Type == "stories" {
		kind = resolve.KindSingle
		canonicalURL = fmt.Sprintf("https://www.instagram.com/stories/%s/", username)
	}
	if target.Type == "highlight" {
		kind = resolve.KindSingle
		canonicalURL = fmt.Sprintf("https://www.instagram.com/stories/highlights/%s/", target.HighlightID)
	}
	src := resolve.NewSource(rawURL, canonicalURL, kind, "instagram")
	src.Author = username
	for _, p := range posts {
		src.Items = append(src.Items, IgPostToItem(p, username, target.Type))
	}
	return src, nil
}
