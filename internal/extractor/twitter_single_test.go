package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/resolve"
)

// ---------------------------------------------------------------------------
// Test server for the single-post operation
// ---------------------------------------------------------------------------

type twGraphQLTestRequest struct {
	Method    string
	Path      string
	Variables map[string]interface{}
}

type twGraphQLTestServer struct {
	srv      *httptest.Server
	deps     *Deps
	mu       sync.Mutex
	requests []twGraphQLTestRequest
}

func (ts *twGraphQLTestServer) Requests() []twGraphQLTestRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]twGraphQLTestRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

// newTwGraphQLServer starts an httptest server and points the GraphQL client at
// it. respond may return nil to serve an empty result.
func newTwGraphQLServer(t *testing.T, respond func(vars map[string]interface{}) []byte) *twGraphQLTestServer {
	t.Helper()
	ts := &twGraphQLTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home" {
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		vars := map[string]interface{}{}
		if raw := r.URL.Query().Get("variables"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &vars)
		}
		ts.mu.Lock()
		ts.requests = append(ts.requests, twGraphQLTestRequest{Method: r.Method, Path: r.URL.Path, Variables: vars})
		ts.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if b := respond(vars); b != nil {
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)

	oldBase, oldRefresh := twAPIBase, twSearchRefreshURL
	twAPIBase = ts.srv.URL
	twSearchRefreshURL = ts.srv.URL + "/home"
	ts.deps = &Deps{HTTPClient: ts.srv.Client(), QueryIDs: &QueryIDCache{}}
	resetTwQueryIDs(ts.deps)
	t.Cleanup(func() {
		twAPIBase, twSearchRefreshURL = oldBase, oldRefresh
	})
	return ts
}

func twTestSingleTweetResponse(result interface{}) []byte {
	resp := map[string]interface{}{
		"data": map[string]interface{}{
			"tweet_results": map[string]interface{}{"result": result},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// ---------------------------------------------------------------------------
// Single-post resolution
// ---------------------------------------------------------------------------

func TestFetchTwTweetByRestIDResolvesOnePost(t *testing.T) {
	t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
	srv := newTwGraphQLServer(t, func(vars map[string]interface{}) []byte {
		id, _ := vars["tweetId"].(string)
		return twTestSingleTweetResponse(twTestTweet(id, "artist", "one video",
			"Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twVideoMedia()}, nil))
	})

	tweet, err := fetchTwTweetByRestID(context.Background(), testDeps(srv.srv.Client()), "", "csrf", "1234567890123456789", nil)
	if err != nil {
		t.Fatalf("fetchTwTweetByRestID: %v", err)
	}
	if tweet.RestID != "1234567890123456789" {
		t.Errorf("rest id = %q", tweet.RestID)
	}
	if got := tweet.author(""); got != "artist" {
		t.Errorf("author = %q, want artist", got)
	}
	urls := twMediaBestURLs(tweet.Legacy.ExtendedEntities.Media[0])
	if len(urls) != 1 || urls[0] != "https://video.twimg.com/hi.mp4" {
		t.Errorf("best URLs = %v, want the highest-bitrate mp4", urls)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	if reqs[0].Method != http.MethodGet {
		t.Errorf("method = %q, want GET", reqs[0].Method)
	}
	if !strings.Contains(reqs[0].Path, "testqid/TweetResultByRestId") {
		t.Errorf("path = %q, want .../testqid/TweetResultByRestId", reqs[0].Path)
	}
	if got := reqs[0].Variables["tweetId"]; got != "1234567890123456789" {
		t.Errorf("tweetId = %v, want the status id", got)
	}
}

func TestFetchTwTweetByRestIDNotFound(t *testing.T) {
	t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
	srv := newTwGraphQLServer(t, func(_ map[string]interface{}) []byte {
		return []byte(`{"data":{"tweet_results":{"result":null}},"errors":[{"message":"Tweet not found."}]}`)
	})

	_, err := fetchTwTweetByRestID(context.Background(), testDeps(srv.srv.Client()), "", "csrf", "123", nil)
	if err == nil {
		t.Fatal("expected an error for a missing post")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %v, want the post-not-available message", err)
	}
}

func TestFetchTwTweetByRestIDNeedsQueryID(t *testing.T) {
	srv := newTwGraphQLServer(t, func(_ map[string]interface{}) []byte { return nil })
	_, err := fetchTwTweetByRestID(context.Background(), testDeps(srv.srv.Client()), "", "csrf", "123", nil)
	if err == nil {
		t.Fatal("expected an actionable error when the query id cannot be discovered")
	}
	if !strings.Contains(err.Error(), "TWITTER_QUERY_TWEET_RESULT_BY_REST_ID") {
		t.Errorf("error = %v, want the actionable env-var hint", err)
	}
	if len(srv.Requests()) > 1 {
		t.Errorf("got %d requests, want at most the discovery request", len(srv.Requests()))
	}
}

func TestScanTwitterResolvedStatusIsOnePost(t *testing.T) {
	t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
	cookies := writeTwCookies(t)
	srv := newTwGraphQLServer(t, func(vars map[string]interface{}) []byte {
		id, _ := vars["tweetId"].(string)
		return twTestSingleTweetResponse(twTestTweet(id, "artist", "one video",
			"Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twVideoMedia()}, nil))
	})

	src, err := ScanTwitterResolved(context.Background(), "https://x.com/artist/status/1234567890123456789/video/1", t.TempDir(), cookies, TwOptions{})
	if err != nil {
		t.Fatalf("ScanTwitterResolved: %v", err)
	}
	if src.Kind != resolve.KindSingle {
		t.Errorf("Kind = %q, want single", src.Kind)
	}
	if src.CanonicalURL != "https://x.com/artist/status/1234567890123456789" {
		t.Errorf("CanonicalURL = %q", src.CanonicalURL)
	}
	if src.Author != "artist" {
		t.Errorf("Author = %q, want artist", src.Author)
	}
	if len(src.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(src.Items))
	}
	if len(src.Items[0].Media) != 1 || src.Items[0].Media[0].Kind != resolve.MediaVideo {
		t.Errorf("media = %v, want one video", src.Items[0].Media)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1 (no timeline pagination)", len(reqs))
	}
	if !strings.Contains(reqs[0].Path, "TweetResultByRestId") {
		t.Errorf("path = %q, want the single-post operation", reqs[0].Path)
	}
}

func TestScanTwitterStatusManifestPaths(t *testing.T) {
	t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
	cookies := writeTwCookies(t)
	srv := newTwGraphQLServer(t, func(vars map[string]interface{}) []byte {
		id, _ := vars["tweetId"].(string)
		return twTestSingleTweetResponse(twTestTweet(id, "artist", "one video",
			"Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twVideoMedia()}, nil))
	})

	out := t.TempDir()
	m, err := ScanTwitter(context.Background(), "https://x.com/artist/status/1234567890123456789", out, cookies, TwOptions{IncludePosts: true})
	if err != nil {
		t.Fatalf("ScanTwitter: %v", err)
	}
	if len(m.Items) != 2 {
		t.Fatalf("got %d items, want 2 (video + post)", len(m.Items))
	}
	byDest := map[string]manifest.Item{}
	for _, it := range m.Items {
		byDest[filepath.ToSlash(it.Destination)] = it
	}
	want := []string{
		"/1234567890123456789_0.mp4",
		"/1234567890123456789.md",
	}
	for _, suffix := range want {
		found := false
		for dest, it := range byDest {
			if strings.HasSuffix(dest, suffix) {
				found = true
				if it.Creator != "artist" || it.PostID != "1234567890123456789" {
					t.Errorf("item %s creator=%q postID=%q", dest, it.Creator, it.PostID)
				}
			}
		}
		if !found {
			t.Errorf("missing expected destination %s (have %v)", suffix, keysOf(byDest))
		}
	}
	if len(srv.Requests()) != 1 {
		t.Errorf("got %d requests, want 1", len(srv.Requests()))
	}
}
