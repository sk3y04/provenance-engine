package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/ratelimit"
)

// twRequireAuthCookies validates that the supplied Netscape cookie file carries
// the two cookies the authenticated web GraphQL session needs: auth_token
// (session) and ct0 (CSRF). It returns an actionable error otherwise.
func twRequireAuthCookies(cookiesFile string) error {
	if strings.TrimSpace(cookiesFile) == "" {
		return fmt.Errorf("cookies file is required for Twitter/X hashtag search")
	}
	cookies, err := loadNetscapeCookies(cookiesFile)
	if err != nil {
		return fmt.Errorf("load cookies: %w", err)
	}
	var haveAuth, haveCSRF bool
	for _, ck := range cookies {
		switch ck.Name {
		case "auth_token":
			haveAuth = strings.TrimSpace(ck.Value) != ""
		case "ct0":
			haveCSRF = strings.TrimSpace(ck.Value) != ""
		}
	}
	var missing []string
	if !haveAuth {
		missing = append(missing, "auth_token")
	}
	if !haveCSRF {
		missing = append(missing, "ct0")
	}
	if len(missing) > 0 {
		return fmt.Errorf("twitter/X hashtag search requires an authenticated session: missing cookie(s) %s in %s (log in to x.com and re-export cookies.txt)",
			strings.Join(missing, ", "), cookiesFile)
	}
	return nil
}

func twSearchQueryIDError() error {
	return fmt.Errorf("twitter search query id unavailable: set TWITTER_QUERY_SEARCH_TIMELINE or allow query-id discovery to reach x.com (SearchTimeline rotates independently and has no hard-coded fallback)")
}

// twSearchPage is one decoded SearchTimeline response. Pointers distinguish an
// absent (malformed/changed) wrapper from a legitimately empty timeline.
type twSearchResponse struct {
	Data struct {
		SearchByRawQuery *struct {
			SearchTimeline *struct {
				Timeline twTimeline `json:"timeline"`
			} `json:"search_timeline"`
		} `json:"search_by_raw_query"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// fetchTwSearchPage runs one page of the SearchTimeline operation. It uses the
// POST + JSON body shape currently served by X's web client; the method/body
// detail is isolated here so it can be updated without touching pagination or
// media handling. Stale query IDs trigger a single invalidate/refresh/retry.
func fetchTwSearchPage(ctx context.Context, client *http.Client, cookiesFile, csrfToken, query, cursor string, rl *ratelimit.Manager) ([]twTweetResult, string, error) {
	for refresh := 0; refresh < 2; refresh++ {
		// Only discover the query ID when it is not already known (env override
		// or a previous page in this process), so bundles are fetched at most
		// once per process rather than once per page.
		if twSearchQueryID() == "" {
			twRefreshQueryIDs(ctx, cookiesFile)
		}
		queryID := twSearchQueryID()
		if queryID == "" {
			return nil, "", twSearchQueryIDError()
		}
		endpoint := fmt.Sprintf("%s/%s/%s", twSearchAPIBase, queryID, twSearchOperation)

		vars := map[string]interface{}{
			"rawQuery":    query,
			"count":       twSearchPageSize,
			"querySource": "typed_query",
			"product":     "Latest",
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		variables, _ := json.Marshal(vars)
		features, _ := json.Marshal(twFeatures)
		body, err := json.Marshal(map[string]interface{}{
			"variables": json.RawMessage(variables),
			"features":  json.RawMessage(features),
			"queryId":   queryID,
		})
		if err != nil {
			return nil, "", fmt.Errorf("encode search request: %w", err)
		}

		var lastErr error
		staleQuery := false
		for attempt := 1; attempt <= twMaxAttempts; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, "", fmt.Errorf("new request: %w", err)
			}
			headers, err := twHeaders(cookiesFile, csrfToken)
			if err != nil {
				return nil, "", err
			}
			req.Header = headers

			if u, _ := url.Parse(endpoint); u != nil && rl != nil {
				_ = rl.GetLimiter(u.Host).Wait(ctx)
			}

			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", fmt.Errorf("http: %w", err)
				}
				lastErr = fmt.Errorf("http: %w", err)
				if attempt == twMaxAttempts {
					return nil, "", fmt.Errorf("twitter search request failed after %d attempts: %w", attempt, lastErr)
				}
				time.Sleep(twRetryBackoff << min(attempt-1, 3))
				continue
			}

			if resp.StatusCode >= 400 {
				preview, _ := io.ReadAll(io.LimitReader(resp.Body, twErrorPreviewLimit))
				_ = resp.Body.Close()
				bodyStr := string(preview)
				if twIsStaleQueryError(resp.StatusCode, bodyStr) && refresh == 0 {
					staleQuery = true
					fmt.Fprintf(os.Stderr, "[twitter] search query ID stale (status %d), refreshing...\n", resp.StatusCode)
					break
				}
				if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
					return nil, "", fmt.Errorf("twitter search authentication failed (status %d): re-export cookies from a logged-in x.com session", resp.StatusCode)
				}
				lastErr = fmt.Errorf("twitter status %d", resp.StatusCode)
				fmt.Fprintf(os.Stderr, "[twitter] search response: %s\n", sanitizeErrorBody(bodyStr))
				if resp.StatusCode == http.StatusTooManyRequests {
					delay := twMaxRetryBackoff
					if ra := resp.Header.Get("Retry-After"); ra != "" {
						if sec, err := strconv.Atoi(ra); err == nil && sec > 0 {
							delay = time.Duration(sec) * time.Second
						}
					}
					fmt.Fprintf(os.Stderr, "[twitter] search rate limited (429), waiting %v...\n", delay)
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

			var result twSearchResponse
			if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result); err != nil {
				_ = resp.Body.Close()
				if refresh == 0 {
					staleQuery = true
					break
				}
				return nil, "", fmt.Errorf("decode: %w", err)
			}
			_ = resp.Body.Close()

			if result.Data.SearchByRawQuery == nil || result.Data.SearchByRawQuery.SearchTimeline == nil {
				if len(result.Errors) > 0 {
					return nil, "", fmt.Errorf("twitter search error: %s", sanitizeErrorBody(twSearchErrorMessages(result.Errors)))
				}
				return nil, "", fmt.Errorf("twitter search: unexpected response schema (missing search_by_raw_query.search_timeline)")
			}
			tweets, nextCursor := twParseTimeline(result.Data.SearchByRawQuery.SearchTimeline.Timeline.Instructions)
			return tweets, nextCursor, nil
		}
		if staleQuery {
			twInvalidateQueryIDs()
			continue
		}
		return nil, "", lastErr
	}
	return nil, "", fmt.Errorf("twitter search: query id still stale after refresh (set TWITTER_QUERY_SEARCH_TIMELINE to a current SearchTimeline id)")
}

func twSearchErrorMessages(errs []struct {
	Message string `json:"message"`
}) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		if strings.TrimSpace(e.Message) != "" {
			msgs = append(msgs, e.Message)
		}
	}
	if len(msgs) == 0 {
		return "unknown error"
	}
	return strings.Join(msgs, "; ")
}

// FetchAllTwSearchPosts pages the Latest hashtag search timeline until the
// cursor is exhausted, the limit is reached, context is cancelled, or a
// repeated cursor / repeated no-progress page is detected. Posts are
// deduplicated by post ID. A rate-limit mid-scan returns the posts collected so
// far (matching the profile extractor's convention); any other failure after
// results were collected returns them alongside an explicit error so a partial
// scan is never reported as complete.
func FetchAllTwSearchPosts(ctx context.Context, client *http.Client, cookiesFile, csrfToken, query string, limit int, rl *ratelimit.Manager) ([]twTweetResult, error) {
	var all []twTweetResult
	seen := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	pages := 0
	noProgress := 0

	for {
		page, nextCursor, err := fetchTwSearchPage(ctx, client, cookiesFile, csrfToken, query, cursor, rl)
		if err != nil {
			if len(all) > 0 && isRateLimitError(err) {
				fmt.Fprintf(os.Stderr, "[twitter] search rate limited mid-scan, returning %d post(s) from %d page(s)\n", len(all), pages)
				return all, nil
			}
			if len(all) > 0 {
				return all, fmt.Errorf("twitter search: pagination stopped after %d post(s): %w", len(all), err)
			}
			return nil, err
		}
		pages++

		newCount := 0
		for _, t := range page {
			if t.RestID == "" || seen[t.RestID] {
				continue
			}
			seen[t.RestID] = true
			all = append(all, t)
			newCount++
		}
		fmt.Fprintf(os.Stderr, "[twitter] search page %d: %d result(s), %d new, %d total\n", pages, len(page), newCount, len(all))

		if limit > 0 && len(all) >= limit {
			all = all[:limit]
			break
		}
		if nextCursor == "" {
			break
		}
		if seenCursors[nextCursor] {
			fmt.Fprintf(os.Stderr, "[twitter] search: repeated cursor detected, stopping pagination\n")
			break
		}
		seenCursors[nextCursor] = true

		if newCount == 0 {
			noProgress++
			if noProgress >= 2 {
				fmt.Fprintf(os.Stderr, "[twitter] search: no new posts across consecutive pages, stopping pagination\n")
				break
			}
		} else {
			noProgress = 0
		}
		cursor = nextCursor
	}
	return all, nil
}
