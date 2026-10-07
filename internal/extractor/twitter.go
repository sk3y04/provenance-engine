package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sk3y04/provenance-engine/internal/downloader"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
	"github.com/sk3y04/provenance-engine/internal/resolve"
	"github.com/sk3y04/provenance-engine/internal/worker"
)

var twitterTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

var (
	twitterRefreshClient = &http.Client{Timeout: 15 * time.Second, Transport: twitterTransport, CheckRedirect: downloader.SafeRedirect}
	twitterAPIClient     = &http.Client{Timeout: 30 * time.Second, Transport: twitterTransport, CheckRedirect: downloader.SafeRedirect}
)

const (
	twAPIBase           = "https://x.com/i/api/graphql"
	twPageSize          = 20
	twSearchPageSize    = 20
	twMaxAttempts       = 4
	twErrorPreviewLimit = 4 << 10
	twRetryBackoff      = 2 * time.Second
	twMaxRetryBackoff   = 15 * time.Second
)

// twSearchAPIBase and twSearchRefreshURL are overridable so tests can point the
// SearchTimeline operation and query-ID discovery at an httptest server without
// weakening production TLS or redirect safety.
var (
	twSearchAPIBase    = twAPIBase
	twSearchRefreshURL = "https://x.com/home"
)

// twSearchOperation is the GraphQL operation name for hashtag search. Current
// web builds serve it via POST with a JSON body; the method/body detail is
// isolated here so it can be updated if X changes shape.
const twSearchOperation = "SearchTimeline"

var twUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func twBearerToken() string {
	if t := os.Getenv("TWITTER_BEARER_TOKEN"); t != "" {
		return "Bearer " + t
	}
	twQueryIDCache.mu.Lock()
	defer twQueryIDCache.mu.Unlock()
	if twQueryIDCache.guestBearer != "" {
		return twQueryIDCache.guestBearer
	}
	// Public guest token from Twitter's web client JS. Same for all
	// unauthenticated visitors; not a secret. Refreshed at runtime via
	// twRefreshQueryIDs. Hardcoded fallback for offline / first-run.
	return "Bearer AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"
}

var twQueryIDCache struct {
	mu               sync.RWMutex
	userByScreenName string
	userMedia        string
	userTweets       string
	searchTimeline   string
	guestBearer      string
}

func twUserByScreenNameQueryID() string {
	if v := os.Getenv("TWITTER_QUERY_USER_BY_SCREEN_NAME"); v != "" {
		return v
	}
	twQueryIDCache.mu.RLock()
	if twQueryIDCache.userByScreenName != "" {
		v := twQueryIDCache.userByScreenName
		twQueryIDCache.mu.RUnlock()
		return v
	}
	twQueryIDCache.mu.RUnlock()
	return "2qvSHpkWTMS9i0zJAwDNiA"
}

func twUserTweetsQueryID() string {
	if v := os.Getenv("TWITTER_QUERY_USER_TWEETS"); v != "" {
		return v
	}
	twQueryIDCache.mu.RLock()
	if twQueryIDCache.userTweets != "" {
		v := twQueryIDCache.userTweets
		twQueryIDCache.mu.RUnlock()
		return v
	}
	twQueryIDCache.mu.RUnlock()
	return "6r5OLCC_wFH4CpRyXKuAmQ"
}

// twSearchQueryID returns the SearchTimeline query ID from the env override or
// the runtime cache populated from X's web-client JS. Unlike the profile
// operations there is intentionally no hard-coded fallback: search IDs rotate
// independently and a stale value produces confusing failures, so an empty
// result raises an actionable error instead (see twSearchQueryIDError).
func twSearchQueryID() string {
	if v := os.Getenv("TWITTER_QUERY_SEARCH_TIMELINE"); v != "" {
		return v
	}
	twQueryIDCache.mu.RLock()
	v := twQueryIDCache.searchTimeline
	twQueryIDCache.mu.RUnlock()
	return v
}

func twRefreshQueryIDs(ctx context.Context, cookiesFile string) {
	twRefreshQueryIDsForce(ctx, cookiesFile, false)
}

func twRefreshQueryIDsForce(ctx context.Context, cookiesFile string, force bool) {
	twQueryIDCache.mu.Lock()
	if !force && twQueryIDCache.userByScreenName != "" && twQueryIDCache.userMedia != "" && twQueryIDCache.userTweets != "" && twQueryIDCache.searchTimeline != "" {
		twQueryIDCache.mu.Unlock()
		return
	}
	twQueryIDCache.mu.Unlock()

	client := twitterRefreshClient
	req, err := http.NewRequestWithContext(ctx, "GET", twSearchRefreshURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", twUserAgent)
	cv, err := cookieHeaderForHosts(cookiesFile, "x.com", "twitter.com")
	if err == nil && cv != "" {
		req.Header.Set("Cookie", cv)
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	// Prefer the main web-client bundle. The pattern also matches a test
	// server's bundle URL so discovery can be exercised without live X.
	re := regexp.MustCompile(`https?://[^"'\s\\]+/main\.[a-f0-9]+\.js`)
	jsURL := re.FindString(string(body))
	if jsURL == "" {
		return
	}
	req2, err := http.NewRequestWithContext(ctx, "GET", jsURL, nil)
	if err != nil {
		return
	}
	req2.Header.Set("User-Agent", twUserAgent)
	resp2, err := client.Do(req2)
	if err != nil {
		return
	}
	defer func() { _ = resp2.Body.Close() }()
	jsBody, _ := io.ReadAll(io.LimitReader(resp2.Body, 4<<20))

	twQueryIDCache.mu.Lock()
	defer twQueryIDCache.mu.Unlock()
	re2 := regexp.MustCompile(`queryId:"([^"]+)",operationName:"UserByScreenName"`)
	if m := re2.FindStringSubmatch(string(jsBody)); len(m) > 1 {
		twQueryIDCache.userByScreenName = m[1]
	}
	re3 := regexp.MustCompile(`queryId:"([^"]+)",operationName:"UserMedia"`)
	if m := re3.FindStringSubmatch(string(jsBody)); len(m) > 1 {
		twQueryIDCache.userMedia = m[1]
	}
	re4 := regexp.MustCompile(`queryId:"([^"]+)",operationName:"UserTweets"`)
	if m := re4.FindStringSubmatch(string(jsBody)); len(m) > 1 {
		twQueryIDCache.userTweets = m[1]
	}
	re4b := regexp.MustCompile(`queryId:"([^"]+)",operationName:"SearchTimeline"`)
	if m := re4b.FindStringSubmatch(string(jsBody)); len(m) > 1 {
		twQueryIDCache.searchTimeline = m[1]
	}
	// Guest bearer token - also public, extracted from the web client JS.
	re5 := regexp.MustCompile(`"(AAAAA[^"]{50,})"`)
	if m := re5.FindStringSubmatch(string(jsBody)); len(m) > 1 {
		twQueryIDCache.guestBearer = "Bearer " + m[1]
	}
}

func twInvalidateQueryIDs() {
	twQueryIDCache.mu.Lock()
	defer twQueryIDCache.mu.Unlock()
	twQueryIDCache.userByScreenName = ""
	twQueryIDCache.userMedia = ""
	twQueryIDCache.userTweets = ""
	twQueryIDCache.searchTimeline = ""
}

var twFeatures = map[string]interface{}{
	"hidden_profile_likes_enabled":                                      true,
	"hidden_profile_subscriptions_enabled":                              true,
	"profile_label_improvements_pcf_label_in_post_enabled":              true,
	"responsive_web_graphql_exclude_directive_enabled":                  true,
	"responsive_web_graphql_skip_user_profile_image_extensions_enabled": false,
	"responsive_web_graphql_timeline_navigation_enabled":                true,
	"rweb_tipjar_consumption_enabled":                                   true,
	"verified_phone_label_enabled":                                      false,
	"highlights_tweets_tab_ui_enabled":                                  true,
	"creator_subscriptions_tweet_preview_api_enabled":                   true,
	"responsive_web_twitter_article_tweet_consumption_enabled":          false,
	"tweet_awards_web_tipping_enabled":                                  false,
	"longform_notetweets_consumption_enabled":                           true,
	"longform_notetweets_rich_text_read_enabled":                        true,
	"longform_notetweets_inline_media_enabled":                          true,
	"responsive_web_media_download_video_enabled":                       false,
	"responsive_web_enhance_cards_enabled":                              false,
}

func twClient(cookiesFile string) (*http.Client, string, error) {
	if cookiesFile == "" {
		return nil, "", fmt.Errorf("cookies file is required for Twitter/X")
	}
	cookies, err := loadNetscapeCookies(cookiesFile)
	if err != nil {
		return nil, "", fmt.Errorf("load cookies: %w", err)
	}
	var csrfToken string
	for _, ck := range cookies {
		if strings.EqualFold(ck.Name, "ct0") {
			csrfToken = ck.Value
			break
		}
	}
	return twitterAPIClient, csrfToken, nil
}

type TwOptions struct {
	CookiesFile  string
	Filter       manifest.FilterOptions
	SpeedLimit   int64
	Progress     downloader.ProgressReporter
	Limit        int
	RateLimiter  *ratelimit.Manager
	IncludePosts bool
}

func ParseTwURL(rawURL string) (username string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	host := strings.ToLower(u.Host)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("not a twitter profile url: %s", rawURL)
	}
	_ = host
	return sanitizeFilename(parts[0]), nil
}

func twHeaders(cookiesFile, csrfToken string) (http.Header, error) {
	h := http.Header{}
	h.Set("User-Agent", twUserAgent)
	h.Set("Authorization", twBearerToken())
	h.Set("x-csrf-token", csrfToken)
	h.Set("Content-Type", "application/json")
	h.Set("Referer", "https://x.com/")
	h.Set("Origin", "https://x.com")

	cv, err := cookieHeaderForHosts(cookiesFile, "x.com", "twitter.com")
	if err != nil {
		return nil, err
	}
	if cv != "" {
		h.Set("Cookie", cv)
	}
	return h, nil
}

type twUserResult struct {
	RestID string `json:"rest_id"`
	Legacy struct {
		ScreenName string `json:"screen_name"`
	} `json:"legacy"`
}

type twUserData struct {
	User struct {
		Result twUserResult `json:"result"`
	} `json:"user"`
}

func twIsStaleQueryError(statusCode int, body string) bool {
	if statusCode == http.StatusNotFound {
		return true
	}
	if statusCode == http.StatusBadRequest && strings.Contains(body, "query") {
		return true
	}
	return false
}

func isRateLimitError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 429")
}

func resolveTwUserID(ctx context.Context, client *http.Client, cookiesFile, csrfToken, username string, rl *ratelimit.Manager) (string, error) {
	fmt.Fprintf(os.Stderr, "[twitter] resolving user @%s ...\n", username)
	for refresh := 0; refresh < 2; refresh++ {
		twRefreshQueryIDs(ctx, cookiesFile)
		queryID := twUserByScreenNameQueryID()
		endpoint := fmt.Sprintf("%s/%s/UserByScreenName", twAPIBase, queryID)

		variables, _ := json.Marshal(map[string]interface{}{
			"screen_name":                username,
			"withSafetyModeUserFields":   true,
			"withSuperFollowsUserFields": false,
			"includePromotedContent":     false,
		})
		features, _ := json.Marshal(twFeatures)

		params := url.Values{}
		params.Set("variables", string(variables))
		params.Set("features", string(features))
		fullURL := endpoint + "?" + params.Encode()

		var lastErr error
		staleQuery := false
		for attempt := 1; attempt <= twMaxAttempts; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
			if err != nil {
				return "", fmt.Errorf("new request: %w", err)
			}
			headers, err := twHeaders(cookiesFile, csrfToken)
			if err != nil {
				return "", err
			}
			req.Header = headers

			if u, _ := url.Parse(fullURL); u != nil && rl != nil {
				_ = rl.GetLimiter(u.Host).Wait(ctx)
			}

			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return "", fmt.Errorf("http: %w", err)
				}
				lastErr = fmt.Errorf("http: %w", err)
				if attempt == twMaxAttempts {
					return "", fmt.Errorf("resolve twitter user after %d attempts: %w", attempt, lastErr)
				}
				time.Sleep(twRetryBackoff << min(attempt-1, 3))
				continue
			}

			if resp.StatusCode >= 400 {
				preview, _ := io.ReadAll(io.LimitReader(resp.Body, twErrorPreviewLimit))
				_ = resp.Body.Close()
				body := string(preview)
				if twIsStaleQueryError(resp.StatusCode, body) && refresh == 0 {
					staleQuery = true
					fmt.Fprintf(os.Stderr, "[twitter] query ID stale (status %d), refreshing...\n", resp.StatusCode)
					break
				}
				if resp.StatusCode == http.StatusTooManyRequests {
					delay := twRetryBackoff << min(attempt-1, 3)
					if ra := resp.Header.Get("Retry-After"); ra != "" {
						if sec, err := strconv.Atoi(ra); err == nil && sec > 0 {
							delay = time.Duration(sec) * time.Second
						}
					}
					fmt.Fprintf(os.Stderr, "[twitter] rate limited (429), attempt %d, waiting %v...\n", attempt, delay)
					time.Sleep(delay)
					continue
				}
				return "", fmt.Errorf("twitter user lookup status %d: %s", resp.StatusCode, body)
			}

			var result struct {
				Data twUserData `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				_ = resp.Body.Close()
				if refresh == 0 {
					staleQuery = true
					break
				}
				return "", fmt.Errorf("decode: %w", err)
			}
			_ = resp.Body.Close()
			if result.Data.User.Result.RestID == "" {
				return "", fmt.Errorf("twitter: user not found: %s", username)
			}
			return result.Data.User.Result.RestID, nil
		}
		if staleQuery {
			twInvalidateQueryIDs()
			continue
		}
		return "", lastErr
	}
	return "", fmt.Errorf("resolve twitter user: failed after query id refresh")
}

type twMediaSize struct {
	W    int    `json:"w"`
	H    int    `json:"h"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type twVideoVariant struct {
	Bitrate     int    `json:"bitrate"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

type twVideoInfo struct {
	Variants []twVideoVariant `json:"variants"`
}

type twMedia struct {
	MediaURLHTTPS string `json:"media_url_https"`
	Type          string `json:"type"`
	Sizes         struct {
		Large  twMediaSize `json:"large"`
		Medium twMediaSize `json:"medium"`
		Small  twMediaSize `json:"small"`
		Thumb  twMediaSize `json:"thumb"`
		Orig   twMediaSize `json:"orig"`
	} `json:"sizes"`
	VideoInfo *twVideoInfo `json:"video_info,omitempty"`
}

type twURLEntity struct {
	URL         string `json:"url"`
	ExpandedURL string `json:"expanded_url"`
	DisplayURL  string `json:"display_url"`
}

type twEntities struct {
	URLs []twURLEntity `json:"urls"`
}

type twExtendedEntities struct {
	Media []twMedia `json:"media"`
}

type twLegacy struct {
	ExtendedEntities *twExtendedEntities `json:"extended_entities,omitempty"`
	FullText         string              `json:"full_text"`
	CreatedAt        string              `json:"created_at"`
	Entities         *twEntities         `json:"entities,omitempty"`
	QuotedStatusID   string              `json:"quoted_status_id_str,omitempty"`
	RetweetedID      string              `json:"retweeted_status_id_str,omitempty"`
}

// twNoteTweet carries the untruncated body of a long-form post.
type twNoteTweet struct {
	NoteTweetResults struct {
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	} `json:"note_tweet_results"`
}

func (n *twNoteTweet) text() string {
	if n == nil {
		return ""
	}
	return n.NoteTweetResults.Result.Text
}

type twTweetResult struct {
	RestID string   `json:"rest_id"`
	Legacy twLegacy `json:"legacy"`
	Core   struct {
		UserResults struct {
			Result twUserResult `json:"result"`
		} `json:"user_results"`
	} `json:"core"`
	NoteTweet             *twNoteTweet          `json:"note_tweet,omitempty"`
	QuotedStatusResult    *twTweetResultWrapper `json:"quoted_status_result,omitempty"`
	RetweetedStatusResult *twTweetResultWrapper `json:"retweeted_status_result,omitempty"`
}

// author returns the post author's screen name when the response embeds it,
// otherwise the supplied fallback (e.g. the profile being scanned).
func (t twTweetResult) author(fallback string) string {
	if s := strings.TrimSpace(t.Core.UserResults.Result.Legacy.ScreenName); s != "" {
		return s
	}
	return fallback
}

// fullText returns the best available post body: the untruncated note-tweet
// text when present, otherwise legacy full_text.
func (t twTweetResult) fullText() string {
	if s := t.NoteTweet.text(); strings.TrimSpace(s) != "" {
		return s
	}
	return t.Legacy.FullText
}

func (t twTweetResult) hasMedia() bool {
	return t.Legacy.ExtendedEntities != nil && len(t.Legacy.ExtendedEntities.Media) > 0
}

type twTweetResultWrapper struct {
	Tweet twTweetResult
}

func (w *twTweetResultWrapper) UnmarshalJSON(data []byte) error {
	// First try TweetWithVisibilityResults: {"__typename":"TweetWithVisibilityResults","tweet":{...}}
	var wrapped struct {
		Tweet twTweetResult `json:"tweet"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Tweet.RestID != "" {
		w.Tweet = wrapped.Tweet
		return nil
	}
	// Fall back to plain Tweet: {"__typename":"Tweet","rest_id":"...","legacy":{...}}
	return json.Unmarshal(data, &w.Tweet)
}

type twTweetContent struct {
	ItemContent struct {
		TweetResults struct {
			Result twTweetResultWrapper `json:"result"`
		} `json:"tweet_results"`
		PromotedMetadata json.RawMessage `json:"promotedMetadata,omitempty"`
	} `json:"itemContent"`
	Value      string `json:"value"`
	CursorType string `json:"cursorType"`
	Items      []struct {
		EntryID string `json:"entryId"`
		Item    struct {
			ItemContent struct {
				TweetResults struct {
					Result twTweetResultWrapper `json:"result"`
				} `json:"tweet_results"`
				PromotedMetadata json.RawMessage `json:"promotedMetadata,omitempty"`
			} `json:"itemContent"`
		} `json:"item"`
	} `json:"items"`
	EntryType string `json:"entryType"`
}

type twEntry struct {
	EntryID string         `json:"entryId"`
	Content twTweetContent `json:"content"`
}

type twInstruction struct {
	Type        string    `json:"type"`
	Entries     []twEntry `json:"entries,omitempty"`
	Entry       *twEntry  `json:"entry,omitempty"`
	ModuleItems []struct {
		EntryID string `json:"entryId"`
		Item    struct {
			ItemContent struct {
				TweetResults struct {
					Result twTweetResultWrapper `json:"result"`
				} `json:"tweet_results"`
				PromotedMetadata json.RawMessage `json:"promotedMetadata,omitempty"`
			} `json:"itemContent"`
		} `json:"item"`
	} `json:"moduleItems,omitempty"`
}

type twTimeline struct {
	Instructions []twInstruction `json:"instructions"`
}

type twUserResultWrapper struct {
	Result struct {
		Timeline struct {
			Timeline twTimeline `json:"timeline"`
		} `json:"timeline"`
	} `json:"result"`
}

// twParseTimeline extracts tweets and the bottom cursor from a timeline
// instruction list. It tolerates the wrappers seen in both UserTweets and
// SearchTimeline responses: plain entries, timeline modules, add-to-module
// instructions, and replacement entries. Promoted entries and cursor entries
// are ignored as media posts.
func twParseTimeline(instructions []twInstruction) (tweets []twTweetResult, nextCursor string) {
	for _, inst := range instructions {
		switch inst.Type {
		case "TimelineAddEntries":
			for _, entry := range inst.Entries {
				twCollectEntry(entry, &tweets, &nextCursor)
			}
		case "TimelineAddToModule":
			for _, item := range inst.ModuleItems {
				if len(item.Item.ItemContent.PromotedMetadata) > 0 {
					continue
				}
				if t := item.Item.ItemContent.TweetResults.Result.Tweet; t.RestID != "" {
					tweets = append(tweets, t)
				}
			}
		case "TimelineReplaceEntry":
			if inst.Entry != nil {
				twCollectEntry(*inst.Entry, &tweets, &nextCursor)
			}
		}
	}
	return tweets, nextCursor
}

func twCollectEntry(entry twEntry, tweets *[]twTweetResult, nextCursor *string) {
	id := entry.EntryID
	if strings.HasPrefix(id, "cursor-bottom-") {
		if entry.Content.Value != "" {
			*nextCursor = entry.Content.Value
		} else {
			*nextCursor = id
		}
		return
	}
	if strings.HasPrefix(id, "cursor-") {
		return
	}
	if strings.Contains(strings.ToLower(id), "promoted") {
		return
	}
	if entry.Content.EntryType == "TimelineTimelineModule" {
		for _, item := range entry.Content.Items {
			if len(item.Item.ItemContent.PromotedMetadata) > 0 {
				continue
			}
			if t := item.Item.ItemContent.TweetResults.Result.Tweet; t.RestID != "" {
				*tweets = append(*tweets, t)
			}
		}
		return
	}
	if len(entry.Content.ItemContent.PromotedMetadata) > 0 {
		return
	}
	if t := entry.Content.ItemContent.TweetResults.Result.Tweet; t.RestID != "" {
		*tweets = append(*tweets, t)
	}
}

func fetchTwMediaPage(ctx context.Context, client *http.Client, cookiesFile, csrfToken, userID, cursor string, rl *ratelimit.Manager) ([]twTweetResult, string, error) {
	for refresh := 0; refresh < 2; refresh++ {
		twRefreshQueryIDs(ctx, cookiesFile)
		queryID := twUserTweetsQueryID()
		endpoint := fmt.Sprintf("%s/%s/UserTweets", twAPIBase, queryID)

		vars := map[string]interface{}{
			"userId":                                 userID,
			"count":                                  twPageSize,
			"includePromotedContent":                 false,
			"withQuickPromoteEligibilityTweetFields": true,
			"withVoice":                              true,
			"withV2Timeline":                         true,
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		variables, _ := json.Marshal(vars)
		features, _ := json.Marshal(twFeatures)

		params := url.Values{}
		params.Set("variables", string(variables))
		params.Set("features", string(features))
		fullURL := endpoint + "?" + params.Encode()

		var lastErr error
		staleQuery := false
		for attempt := 1; attempt <= twMaxAttempts; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
			if err != nil {
				return nil, "", fmt.Errorf("new request: %w", err)
			}
			headers, err := twHeaders(cookiesFile, csrfToken)
			if err != nil {
				return nil, "", err
			}
			req.Header = headers

			if u, _ := url.Parse(fullURL); u != nil && rl != nil {
				_ = rl.GetLimiter(u.Host).Wait(ctx)
			}

			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", fmt.Errorf("http: %w", err)
				}
				lastErr = fmt.Errorf("http: %w", err)
				if attempt == twMaxAttempts {
					return nil, "", fmt.Errorf("twitter request failed after %d attempts: %w", attempt, lastErr)
				}
				time.Sleep(twRetryBackoff << min(attempt-1, 3))
				continue
			}

			if resp.StatusCode >= 400 {
				preview, _ := io.ReadAll(io.LimitReader(resp.Body, twErrorPreviewLimit))
				_ = resp.Body.Close()
				body := string(preview)
				staleCode := resp.StatusCode
				if twIsStaleQueryError(staleCode, body) && refresh == 0 {
					staleQuery = true
					fmt.Fprintf(os.Stderr, "[twitter] query ID stale (status %d), refreshing...\n", staleCode)
					break
				}
				lastErr = fmt.Errorf("twitter status %d", staleCode)
				fmt.Fprintf(os.Stderr, "[twitter] response: %s\n", sanitizeErrorBody(body))
				if resp.StatusCode == http.StatusTooManyRequests {
					delay := twMaxRetryBackoff
					if ra := resp.Header.Get("Retry-After"); ra != "" {
						if sec, err := strconv.Atoi(ra); err == nil && sec > 0 {
							delay = time.Duration(sec) * time.Second
						}
					}
					fmt.Fprintf(os.Stderr, "[twitter] rate limited (429), waiting %v...\n", delay)
					time.Sleep(delay)
					continue
				}
				if resp.StatusCode >= 400 && resp.StatusCode < 500 && attempt == twMaxAttempts {
					return nil, "", lastErr
				}
				if resp.StatusCode >= 500 && attempt < twMaxAttempts {
					time.Sleep(twRetryBackoff << min(attempt-1, 3))
					continue
				}
				return nil, "", lastErr
			}

			var result struct {
				Data struct {
					User twUserResultWrapper `json:"user"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				_ = resp.Body.Close()
				if refresh == 0 {
					staleQuery = true
					break
				}
				return nil, "", fmt.Errorf("decode: %w", err)
			}
			_ = resp.Body.Close()

			tweets, nextCursor := twParseTimeline(result.Data.User.Result.Timeline.Timeline.Instructions)
			return tweets, nextCursor, nil
		}
		if staleQuery {
			twInvalidateQueryIDs()
			continue
		}
		return nil, "", lastErr
	}
	return nil, "", fmt.Errorf("fetch twitter media: failed after query id refresh")
}

func FetchAllTwTweets(ctx context.Context, client *http.Client, cookiesFile, csrfToken, userID string, limit int, rl *ratelimit.Manager) ([]twTweetResult, error) {
	var all []twTweetResult
	cursor := ""
	pages := 0
	for {
		page, nextCursor, err := fetchTwMediaPage(ctx, client, cookiesFile, csrfToken, userID, cursor, rl)
		if err != nil {
			if len(all) > 0 && isRateLimitError(err) {
				fmt.Fprintf(os.Stderr, "[twitter] rate limited mid-scan, returning %d items from %d pages\n", len(all), pages)
				return all, nil
			}
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		pages++
		all = append(all, page...)
		fmt.Fprintf(os.Stderr, "[twitter] page %d: %d tweets (%d with media, %d total)\n", pages, len(page), tweetMediaCount(page), len(all))
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

func tweetMediaCount(tweets []twTweetResult) int {
	n := 0
	for _, t := range tweets {
		if t.Legacy.ExtendedEntities != nil {
			n++
		}
	}
	return n
}

// twPhotoBestURL returns the original-size URL for a photo.
func twPhotoBestURL(m twMedia) string {
	if m.Sizes.Orig.URL != "" {
		return m.Sizes.Orig.URL
	}
	if m.MediaURLHTTPS != "" {
		return m.MediaURLHTTPS + "?name=orig"
	}
	return ""
}

// twVideoVariantURL returns the highest-bitrate non-HLS variant, matching the
// downloader's supported formats (HLS requires yt-dlp and is skipped).
func twVideoVariantURL(m twMedia) string {
	if m.VideoInfo == nil {
		return ""
	}
	variants := make([]twVideoVariant, 0, len(m.VideoInfo.Variants))
	for _, v := range m.VideoInfo.Variants {
		if v.URL == "" || isTwHLSContentType(v.ContentType) {
			continue
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return ""
	}
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].Bitrate > variants[j].Bitrate
	})
	return variants[0].URL
}

func isTwHLSContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "mpegurl")
}

func twMediaBestURLs(m twMedia) []string {
	switch m.Type {
	case "photo":
		if u := twPhotoBestURL(m); u != "" {
			return []string{u}
		}
	case "animated_gif":
		// Animated GIFs are served as MP4; the still image is only a fallback
		// for the rare response without video variants.
		if u := twVideoVariantURL(m); u != "" {
			return []string{u}
		}
		if u := twPhotoBestURL(m); u != "" {
			return []string{u}
		}
	case "video":
		if u := twVideoVariantURL(m); u != "" {
			return []string{u}
		}
	}
	return nil
}

func twMediaExt(m twMedia) string {
	switch m.Type {
	case "photo":
		return "jpg"
	case "video", "animated_gif":
		return "mp4"
	}
	return ""
}

func parseTwTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{time.RubyDate, "Mon Jan 2 15:04:05 -0700 2006", "Mon Jan 02 15:04:05 -0700 2006", time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// twPost is a normalized, media-owning post. For quoted/reposted content the
// Tweet is the embedded post itself so metadata and canonical URLs point at the
// actual creator rather than the account that surfaced it.
type twPost struct {
	Tweet  twTweetResult
	Author string
}

// twPostURL returns the canonical status URL for a post, using the /i/ form
// when the actual author is unknown (rather than misattributing it).
func twPostURL(t twTweetResult, author string) string {
	if author != "" {
		return fmt.Sprintf("https://x.com/%s/status/%s", author, t.RestID)
	}
	return fmt.Sprintf("https://x.com/i/status/%s", t.RestID)
}

// twPostsFromResults flattens raw timeline tweets into deduplicated posts. The
// tweet itself is always included (so --include-posts captures the matching
// post); embedded quoted or reposted posts are included only when their data is
// present and they carry native media. Dedup is by post ID so the same
// underlying post/media is counted once even across pages or when it appears
// both standalone and embedded.
func twPostsFromResults(tweets []twTweetResult, defaultAuthor string) []twPost {
	seen := make(map[string]bool, len(tweets))
	out := make([]twPost, 0, len(tweets))
	add := func(t twTweetResult, fallback string) {
		if t.RestID == "" || seen[t.RestID] {
			return
		}
		seen[t.RestID] = true
		out = append(out, twPost{Tweet: t, Author: t.author(fallback)})
	}
	for _, t := range tweets {
		add(t, defaultAuthor)
		if t.QuotedStatusResult != nil {
			if q := t.QuotedStatusResult.Tweet; q.hasMedia() {
				add(q, "")
			}
		}
		if t.RetweetedStatusResult != nil {
			if r := t.RetweetedStatusResult.Tweet; r.hasMedia() {
				add(r, "")
			}
		}
	}
	return out
}

func twPostMarkdown(p twPost) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s", p.Author)
	if !parseTwTime(p.Tweet.Legacy.CreatedAt).IsZero() {
		fmt.Fprintf(&b, " · %s", parseTwTime(p.Tweet.Legacy.CreatedAt).UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString("\n\n")
	text := p.Tweet.fullText()
	if p.Tweet.Legacy.Entities != nil {
		for _, u := range p.Tweet.Legacy.Entities.URLs {
			text = strings.ReplaceAll(text, u.URL, u.ExpandedURL)
		}
	}
	b.WriteString(text)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "[Source](%s)\n", twPostURL(p.Tweet, p.Author))
	return []byte(b.String())
}

func twPostItems(baseDir string, posts []twPost, includePosts bool) []manifest.Item {
	items := make([]manifest.Item, 0)
	for _, p := range posts {
		t := p.Tweet
		published := parseTwTime(t.Legacy.CreatedAt)

		if includePosts && t.RestID != "" {
			postName := t.RestID + ".md"
			items = append(items, manifest.Item{
				ID:          t.RestID + "_post",
				URL:         "post://twitter/" + t.RestID,
				Title:       firstNonEmpty(t.fullText(), "post_"+t.RestID),
				Filename:    postName,
				Extension:   "md",
				Source:      "twitter",
				Creator:     p.Author,
				PostID:      t.RestID,
				PublishedAt: published,
				Destination: filepath.Join(baseDir, "posts", postName),
				Kind:        "post",
			})
		}

		if !t.hasMedia() {
			continue
		}
		for i, m := range t.Legacy.ExtendedEntities.Media {
			urls := twMediaBestURLs(m)
			for _, mediaURL := range urls {
				if mediaURL == "" {
					continue
				}
				ext := twMediaExt(m)
				name := fmt.Sprintf("%s_%d.%s", t.RestID, i, ext)
				if ext == "" {
					name = fmt.Sprintf("%s_%d", t.RestID, i)
					if u, _ := url.Parse(mediaURL); u != nil {
						name = sanitizeFilename(filepath.Base(u.Path))
					}
				}
				kind := m.Type
				if kind == "" {
					kind = "attachment"
				}
				subdir := "images"
				if m.Type == "video" || m.Type == "animated_gif" {
					subdir = "videos"
				}
				items = append(items, manifest.Item{
					ID:          t.RestID + "_" + strconv.Itoa(i),
					URL:         mediaURL,
					Title:       firstNonEmpty(t.fullText(), kind+"_"+strconv.Itoa(i)),
					Filename:    name,
					Extension:   ext,
					Source:      "twitter",
					Creator:     p.Author,
					PostID:      t.RestID,
					PublishedAt: published,
					Destination: filepath.Join(baseDir, subdir, name),
					Kind:        kind,
				})
			}
		}
	}
	return items
}

// twCollection is the normalized result of scanning any Twitter/X source.
type twCollection struct {
	Source TwSource
	Posts  []twPost
}

func (c *twCollection) items(outDir string, includePosts bool) []manifest.Item {
	return twPostItems(c.Source.BaseDir(outDir), c.Posts, includePosts)
}

// twCollect resolves a source and fetches its posts, branching between the
// profile timeline and the hashtag SearchTimeline. Cookies are required either
// way; hashtag search additionally requires the authenticated session cookies.
func twCollect(ctx context.Context, rawURL, cookiesFile string, opts TwOptions) (*twCollection, error) {
	src, err := ParseTwSource(rawURL)
	if err != nil {
		return nil, err
	}
	client, csrfToken, err := twClient(cookiesFile)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	if src.Kind == TwSourceHashtag {
		if err := twRequireAuthCookies(cookiesFile); err != nil {
			return nil, err
		}
		tweets, err := FetchAllTwSearchPosts(ctx, client, cookiesFile, csrfToken, src.Query, opts.Limit, opts.RateLimiter)
		if err != nil {
			return nil, err
		}
		return &twCollection{Source: src, Posts: twPostsFromResults(tweets, "")}, nil
	}

	userID, err := resolveTwUserID(ctx, client, cookiesFile, csrfToken, src.Username, opts.RateLimiter)
	if err != nil {
		return nil, fmt.Errorf("resolve user id: %w", err)
	}
	tweets, err := FetchAllTwTweets(ctx, client, cookiesFile, csrfToken, userID, opts.Limit, opts.RateLimiter)
	if err != nil {
		return nil, err
	}
	return &twCollection{Source: src, Posts: twPostsFromResults(tweets, src.Username)}, nil
}

func ScanTwitter(ctx context.Context, rawURL, outDir string, cookiesFile string, opts TwOptions) (manifest.Manifest, error) {
	coll, err := twCollect(ctx, rawURL, cookiesFile, opts)
	if err != nil {
		return manifest.Manifest{}, err
	}
	m := manifest.New(rawURL, "twitter", coll.items(outDir, opts.IncludePosts))
	return m.Filter(opts.Filter)
}

func DownloadTwitter(ctx context.Context, rawURL, outDir string, cookiesFile string, opts TwOptions, dryRun bool) error {
	coll, err := twCollect(ctx, rawURL, cookiesFile, opts)
	if err != nil {
		return err
	}
	base := coll.Source.BaseDir(outDir)

	items, err := manifest.FilterItems(coll.items(outDir, opts.IncludePosts), opts.Filter)
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

	pool := worker.NewPool(ctx, 4)

	failed := make(map[string]string)
	var failMu sync.Mutex
	recordFailure := func(url string, err error) {
		failMu.Lock()
		failed[url] = err.Error()
		failMu.Unlock()
		fmt.Fprintf(os.Stderr, "[provenance] twitter download failed: %s: %v\n", url, err)
	}

	for _, p := range coll.Posts {
		p := p
		t := p.Tweet

		if postItem, ok := postsByID[t.RestID]; ok {
			postDest := postItem.Destination
			postContent := twPostMarkdown(p)
			if dryRun {
				fmt.Printf("[dry-run] twitter post: %s -> %s\n", t.RestID, postDest)
			} else {
				pool.SubmitWithHooks(func() error {
					return writeFile(postDest, postContent)
				}, func() {
					fmt.Fprintf(os.Stderr, "[provenance] twitter post saved: %s\n", postDest)
				}, func(err error) {
					recordFailure("post:"+t.RestID, err)
				})
			}
		}

		if !t.hasMedia() {
			continue
		}
		for i, m := range t.Legacy.ExtendedEntities.Media {
			m := m
			urls := twMediaBestURLs(m)
			for _, mediaURL := range urls {
				mediaURL := mediaURL
				if mediaURL == "" {
					continue
				}
				item, ok := allowed[mediaURL]
				if !ok {
					continue
				}
				dest := item.Destination
				if dest == "" {
					ext := twMediaExt(m)
					name := fmt.Sprintf("%s_%d.%s", t.RestID, i, ext)
					subdir := "images"
					if m.Type == "video" || m.Type == "animated_gif" {
						subdir = "videos"
					}
					dest = filepath.Join(base, subdir, sanitizeFilename(name))
				}
				if dryRun {
					fmt.Printf("[dry-run] twitter: %s -> %s\n", mediaURL, dest)
					continue
				}
				furl := mediaURL
				pool.SubmitWithHooks(func() error {
					dl := downloader.New()
					dl.SpeedLimit = opts.SpeedLimit
					dl.Progress = opts.Progress
					return dl.Download(ctx, furl, dest, "")
				}, nil, func(err error) {
					recordFailure(furl, err)
				})
			}
		}
	}

	pool.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("twitter: %d file(s) failed to download (first: %s)", len(failed), firstFailedURL(failed))
	}
	return nil
}

func TwTweetToItem(t twTweetResult, username string) resolve.Item {
	author := t.author(username)
	published := parseTwTime(t.Legacy.CreatedAt)
	canonicalURL := twPostURL(t, author)
	item := resolve.NewItem(t.RestID, canonicalURL)
	item.Title = firstNonEmpty(t.fullText(), t.RestID)
	item.Author = author
	if !published.IsZero() {
		item.PublishedAt = &published
	}
	if text := t.fullText(); text != "" {
		item.Text = &resolve.TextContent{Body: text, Format: resolve.FormatPlain}
	}
	if raw, err := json.Marshal(t); err == nil {
		item.RawMetadata = raw
	}
	if !t.hasMedia() {
		return item
	}
	for _, m := range t.Legacy.ExtendedEntities.Media {
		urls := twMediaBestURLs(m)
		for _, mediaURL := range urls {
			ext := twMediaExt(m)
			kind := resolve.MediaImage
			if m.Type == "video" || m.Type == "animated_gif" {
				kind = resolve.MediaVideo
			}
			asset := resolve.NewMediaAsset(mediaURL, kind)
			asset.Extension = ext
			if m.Type != "video" && m.Type != "animated_gif" {
				asset.Size = m.Sizes.Orig.Size
			}
			item.Media = append(item.Media, asset)
		}
	}
	return item
}

func ScanTwitterResolved(ctx context.Context, rawURL, outDir, cookiesFile string, opts TwOptions) (resolve.Source, error) {
	coll, err := twCollect(ctx, rawURL, cookiesFile, opts)
	if err != nil {
		return resolve.Source{}, err
	}
	src := resolve.NewSource(rawURL, coll.Source.Canonical, resolve.KindFeed, "twitter")
	if coll.Source.Kind == TwSourceProfile {
		src.Author = coll.Source.Username
	}
	for _, p := range coll.Posts {
		src.Items = append(src.Items, TwTweetToItem(p.Tweet, p.Author))
	}
	return src, nil
}
