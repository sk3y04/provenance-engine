package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

func twTestLegacy(text, createdAt string, media []map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	legacy := map[string]interface{}{
		"full_text":  text,
		"created_at": createdAt,
	}
	if media != nil {
		legacy["extended_entities"] = map[string]interface{}{"media": media}
	}
	for k, v := range extra {
		legacy[k] = v
	}
	return legacy
}

func twTestTweet(id, author, text, createdAt string, media []map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"__typename": "Tweet",
		"rest_id":    id,
		"legacy":     twTestLegacy(text, createdAt, media, extra),
		"core": map[string]interface{}{
			"user_results": map[string]interface{}{
				"result": map[string]interface{}{
					"rest_id": author + "-id",
					"legacy":  map[string]interface{}{"screen_name": author},
				},
			},
		},
	}
}

func twTestTweetItem(entryID string, result interface{}) map[string]interface{} {
	return map[string]interface{}{
		"entryId": entryID,
		"content": map[string]interface{}{
			"entryType": "TimelineTimelineItem",
			"itemContent": map[string]interface{}{
				"tweet_results": map[string]interface{}{"result": result},
			},
		},
	}
}

func twTestVisibilityTweet(id, author, text string, media []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"__typename": "TweetWithVisibilityResults",
		"tweet":      twTestTweet(id, author, text, "Mon Jan 15 10:00:00 +0000 2024", media, nil),
	}
}

func twTestCursorEntry(cursor string) map[string]interface{} {
	return map[string]interface{}{
		"entryId": "cursor-bottom-0",
		"content": map[string]interface{}{
			"entryType":  "TimelineTimelineCursor",
			"cursorType": "Bottom",
			"value":      cursor,
		},
	}
}

func twTestModuleEntry(entryID string, results []interface{}) map[string]interface{} {
	items := make([]interface{}, 0, len(results))
	for i, r := range results {
		items = append(items, map[string]interface{}{
			"entryId": fmt.Sprintf("%s-item-%d", entryID, i),
			"item": map[string]interface{}{
				"itemContent": map[string]interface{}{
					"tweet_results": map[string]interface{}{"result": r},
				},
			},
		})
	}
	return map[string]interface{}{
		"entryId": entryID,
		"content": map[string]interface{}{
			"entryType": "TimelineTimelineModule",
			"items":     items,
		},
	}
}

func twTestSearchResponse(entries []interface{}, cursor string) []byte {
	if entries == nil {
		entries = []interface{}{}
	}
	if cursor != "" {
		entries = append(entries, twTestCursorEntry(cursor))
	}
	resp := map[string]interface{}{
		"data": map[string]interface{}{
			"search_by_raw_query": map[string]interface{}{
				"search_timeline": map[string]interface{}{
					"timeline": map[string]interface{}{
						"instructions": []interface{}{
							map[string]interface{}{"type": "TimelineAddEntries", "entries": entries},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

func twPhotoMedia(name string) map[string]interface{} {
	return map[string]interface{}{
		"type":            "photo",
		"media_url_https": "https://pbs.twimg.com/media/" + name + ".jpg",
		"sizes": map[string]interface{}{
			"orig": map[string]interface{}{
				"url": "https://pbs.twimg.com/media/" + name + ".jpg", "w": 1200, "h": 800, "size": 50000,
			},
		},
	}
}

func twVideoMedia() map[string]interface{} {
	return map[string]interface{}{
		"type":            "video",
		"media_url_https": "https://pbs.twimg.com/thumb/VIDEO.jpg",
		"video_info": map[string]interface{}{
			"variants": []interface{}{
				map[string]interface{}{"bitrate": 256000, "content_type": "video/mp4", "url": "https://video.twimg.com/lo.mp4"},
				map[string]interface{}{"bitrate": 2176000, "content_type": "video/mp4", "url": "https://video.twimg.com/hi.mp4"},
				map[string]interface{}{"bitrate": 832000, "content_type": "video/mp4", "url": "https://video.twimg.com/mid.mp4"},
				map[string]interface{}{"content_type": "application/x-mpegURL", "url": "https://video.twimg.com/playlist.m3u8"},
			},
		},
	}
}

func twGifMedia() map[string]interface{} {
	return map[string]interface{}{
		"type":            "animated_gif",
		"media_url_https": "https://pbs.twimg.com/tweet_video/thumb.jpg",
		"video_info": map[string]interface{}{
			"variants": []interface{}{
				map[string]interface{}{"bitrate": 0, "content_type": "video/mp4", "url": "https://video.twimg.com/tweet_video/GIF.mp4"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Test server
// ---------------------------------------------------------------------------

type twSearchTestRequest struct {
	Method  string
	Path    string
	Query   string
	Product string
	Cursor  string
}

type twSearchTestServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []twSearchTestRequest
}

func (ts *twSearchTestServer) Requests() []twSearchTestRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]twSearchTestRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

func resetTwQueryIDs() {
	twInvalidateQueryIDs()
	twQueryIDCache.mu.Lock()
	twQueryIDCache.guestBearer = ""
	twQueryIDCache.mu.Unlock()
}

// newTwSearchServer starts an httptest server and points the SearchTimeline
// client at it. respond may return nil to serve an empty result page.
func newTwSearchServer(t *testing.T, respond func(cursor, rawQuery string) []byte) *twSearchTestServer {
	t.Helper()
	ts := &twSearchTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home" {
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Variables struct {
				RawQuery string `json:"rawQuery"`
				Product  string `json:"product"`
				Cursor   string `json:"cursor"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		ts.mu.Lock()
		ts.requests = append(ts.requests, twSearchTestRequest{
			Method: r.Method, Path: r.URL.Path, Query: body.Variables.RawQuery,
			Product: body.Variables.Product, Cursor: body.Variables.Cursor,
		})
		ts.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if respond != nil {
			if b := respond(body.Variables.Cursor, body.Variables.RawQuery); b != nil {
				_, _ = w.Write(b)
				return
			}
		}
		_, _ = w.Write(twTestSearchResponse(nil, ""))
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)

	oldBase, oldRefresh := twSearchAPIBase, twSearchRefreshURL
	oldClient := twitterRefreshClient
	twSearchAPIBase = ts.srv.URL
	twSearchRefreshURL = ts.srv.URL + "/home"
	twitterRefreshClient = ts.srv.Client()
	resetTwQueryIDs()
	t.Cleanup(func() {
		twSearchAPIBase, twSearchRefreshURL = oldBase, oldRefresh
		twitterRefreshClient = oldClient
		resetTwQueryIDs()
	})
	return ts
}

func writeTwCookies(t *testing.T) string {
	t.Helper()
	content := "# Netscape HTTP Cookie File\n" +
		".x.com\tTRUE\t/\tTRUE\t0\tauth_token\tsecretvalue\n" +
		".x.com\tTRUE\t/\tFALSE\t0\tct0\tcsrfvalue\n"
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Parsing / media selection
// ---------------------------------------------------------------------------

func TestFetchTwSearchPagePostsPhotoAndCursor(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	srv := newTwSearchServer(t, func(cursor, _ string) []byte {
		if cursor == "" {
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-1", twTestTweet("111", "alice", "hello #world", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("PHOTO")}, nil)),
			}, "CURSOR1")
		}
		return twTestSearchResponse(nil, "")
	})

	tweets, cursor, err := fetchTwSearchPage(context.Background(), srv.srv.Client(), "", "csrf", "#golang filter:media", "", nil)
	if err != nil {
		t.Fatalf("fetchTwSearchPage: %v", err)
	}
	if len(tweets) != 1 {
		t.Fatalf("got %d tweets, want 1", len(tweets))
	}
	if cursor != "CURSOR1" {
		t.Errorf("cursor = %q, want CURSOR1", cursor)
	}
	if got := tweets[0].author(""); got != "alice" {
		t.Errorf("author = %q, want alice", got)
	}
	if got := twMediaExt(tweets[0].Legacy.ExtendedEntities.Media[0]); got != "jpg" {
		t.Errorf("ext = %q, want jpg", got)
	}
	urls := twMediaBestURLs(tweets[0].Legacy.ExtendedEntities.Media[0])
	if len(urls) != 1 || urls[0] != "https://pbs.twimg.com/media/PHOTO.jpg" {
		t.Errorf("best URLs = %v", urls)
	}

	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	if reqs[0].Method != http.MethodPost {
		t.Errorf("method = %q, want POST", reqs[0].Method)
	}
	if reqs[0].Product != "Latest" {
		t.Errorf("product = %q, want Latest", reqs[0].Product)
	}
	if reqs[0].Query != "#golang filter:media" {
		t.Errorf("rawQuery = %q", reqs[0].Query)
	}
	if !strings.Contains(reqs[0].Path, "testqid/SearchTimeline") {
		t.Errorf("path = %q, want .../testqid/SearchTimeline", reqs[0].Path)
	}
}

func TestTwMediaBestURLsVideoHighestMP4(t *testing.T) {
	urls := twMediaBestURLs(twVideoMediaAsTwMedia(t))
	if len(urls) != 1 || urls[0] != "https://video.twimg.com/hi.mp4" {
		t.Errorf("best video URL = %v, want hi.mp4", urls)
	}
}

func TestTwMediaBestURLsAnimatedGifMP4(t *testing.T) {
	m := twMediaFromMap(t, twGifMedia())
	urls := twMediaBestURLs(m)
	if len(urls) != 1 || urls[0] != "https://video.twimg.com/tweet_video/GIF.mp4" {
		t.Errorf("gif URL = %v, want GIF.mp4", urls)
	}
	if ext := twMediaExt(m); ext != "mp4" {
		t.Errorf("gif ext = %q, want mp4", ext)
	}
}

func twVideoMediaAsTwMedia(t *testing.T) twMedia {
	t.Helper()
	return twMediaFromMap(t, twVideoMedia())
}

func twMediaFromMap(t *testing.T, m map[string]interface{}) twMedia {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out twMedia
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Pagination / dedup / termination
// ---------------------------------------------------------------------------

func TestFetchAllTwSearchPostsPaginationDedupAndLimit(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	newTwSearchServer(t, func(cursor, _ string) []byte {
		switch cursor {
		case "":
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-1", twTestTweet("111", "alice", "one", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
				twTestTweetItem("tweet-2", twTestTweet("222", "bob", "two", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			}, "C1")
		case "C1":
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-2-dup", twTestTweet("222", "bob", "two", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
				twTestTweetItem("tweet-3", twTestTweet("333", "carl", "three", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			}, "C2")
		default:
			return twTestSearchResponse(nil, "")
		}
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllTwSearchPosts: %v", err)
	}
	if len(posts) != 3 {
		t.Fatalf("got %d posts, want 3 (deduped)", len(posts))
	}
	ids := map[string]bool{}
	for _, p := range posts {
		ids[p.RestID] = true
	}
	for _, want := range []string{"111", "222", "333"} {
		if !ids[want] {
			t.Errorf("missing post %s", want)
		}
	}
}

func TestFetchAllTwSearchPostsRepeatedCursorStops(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	newTwSearchServer(t, func(cursor, _ string) []byte {
		if cursor == "" {
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-1", twTestTweet("111", "alice", "one", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			}, "LOOP")
		}
		// Always returns the same cursor -> potential infinite loop.
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-x", twTestTweet("999", "loop", "again", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
		}, "LOOP")
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllTwSearchPosts: %v", err)
	}
	if len(posts) > 2 {
		t.Errorf("repeated cursor should stop pagination, got %d posts", len(posts))
	}
}

func TestFetchAllTwSearchPostsNoProgressStops(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	page := 0
	newTwSearchServer(t, func(cursor, _ string) []byte {
		page++
		// Every page yields the same already-seen post and a fresh cursor.
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-1", twTestTweet("111", "alice", "one", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
		}, fmt.Sprintf("C%d", page))
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllTwSearchPosts: %v", err)
	}
	if len(posts) != 1 {
		t.Errorf("no-progress should stop with 1 post, got %d", len(posts))
	}
}

func TestFetchAllTwSearchPostsPartialFailureIsNotSilent(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	page := 0
	newTwSearchServer(t, func(cursor, _ string) []byte {
		page++
		if page == 1 {
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-1", twTestTweet("111", "alice", "one", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			}, "C1")
		}
		// Malformed/changed schema on the second page.
		return []byte(`{"data":{"something_changed":true}}`)
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 0, nil)
	if err == nil {
		t.Fatal("expected an error so a partial pagination failure is not reported as complete")
	}
	if len(posts) != 1 {
		t.Errorf("got %d partial posts, want 1", len(posts))
	}
	if !strings.Contains(err.Error(), "pagination stopped after 1") {
		t.Errorf("error = %v, want partial-progress context", err)
	}
}

func TestFetchAllTwSearchPostsLimitAcrossPages(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	newTwSearchServer(t, func(cursor, _ string) []byte {
		if cursor == "" {
			return twTestSearchResponse([]interface{}{
				twTestTweetItem("tweet-1", twTestTweet("111", "a", "1", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
				twTestTweetItem("tweet-2", twTestTweet("222", "b", "2", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			}, "C1")
		}
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-3", twTestTweet("333", "c", "3", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			twTestTweetItem("tweet-4", twTestTweet("444", "d", "4", "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
		}, "C2")
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 3, nil)
	if err != nil {
		t.Fatalf("FetchAllTwSearchPosts: %v", err)
	}
	if len(posts) != 3 {
		t.Errorf("limit=3 got %d posts", len(posts))
	}
}

func TestFetchAllTwSearchPostsIgnoresPromotedAndNoMedia(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	promoted := twTestTweetItem("promoted-1", twTestTweet("777", "ad", "buy now", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("AD")}, nil))
	promoted["entryId"] = "promoted-1"
	metaPromoted := twTestTweetItem("tweet-888", twTestTweet("888", "ad2", "sponsored", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("AD2")}, nil))
	metaPromoted["content"].(map[string]interface{})["itemContent"].(map[string]interface{})["promotedMetadata"] = map[string]interface{}{"impressionId": "x"}

	newTwSearchServer(t, func(cursor, _ string) []byte {
		return twTestSearchResponse([]interface{}{
			promoted,
			metaPromoted,
			twTestTweetItem("tweet-1", twTestTweet("111", "alice", "real", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("REAL")}, nil)),
		}, "")
	})

	posts, err := FetchAllTwSearchPosts(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllTwSearchPosts: %v", err)
	}
	if len(posts) != 1 || posts[0].RestID != "111" {
		t.Errorf("promoted/no-media filtering failed: %+v", posts)
	}
}

func TestFetchAllTwSearchPostsVisibilityAndModule(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	newTwSearchServer(t, func(cursor, _ string) []byte {
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-vis", twTestVisibilityTweet("555", "vis", "wrapped", []map[string]interface{}{twPhotoMedia("VIS")})),
			twTestModuleEntry("module-1", []interface{}{
				twTestTweet("666", "mod", "modular", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("MOD")}, nil),
			}),
		}, "")
	})

	tweets, _, err := fetchTwSearchPage(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", "", nil)
	if err != nil {
		t.Fatalf("fetchTwSearchPage: %v", err)
	}
	if len(tweets) != 2 {
		t.Fatalf("got %d tweets, want 2 (visibility + module)", len(tweets))
	}
}

func TestTwPostsFromResultsQuotedMediaDedup(t *testing.T) {
	quoted := twTestTweet("222", "quotedauthor", "quoted body", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("QUOTED")}, nil)
	outer := twTestTweet("111", "reposter", "look at this", "Mon Jan 15 10:00:00 +0000 2024", nil, map[string]interface{}{
		"quoted_status_result": map[string]interface{}{"result": quoted},
	})
	raw, _ := json.Marshal(outer)
	var t1 twTweetResult
	if err := json.Unmarshal(raw, &t1); err != nil {
		t.Fatal(err)
	}
	// Same quoted post appears again as a standalone result.
	rawQ, _ := json.Marshal(quoted)
	var t2 twTweetResult
	if err := json.Unmarshal(rawQ, &t2); err != nil {
		t.Fatal(err)
	}

	posts := twPostsFromResults([]twTweetResult{t1, t2}, "")
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2 (outer + quoted)", len(posts))
	}
	var quotedPost *twPost
	for i := range posts {
		if posts[i].Tweet.RestID == "222" {
			quotedPost = &posts[i]
		}
	}
	if quotedPost == nil {
		t.Fatal("quoted post missing")
	}
	if quotedPost.Author != "quotedauthor" {
		t.Errorf("quoted author = %q, want quotedauthor", quotedPost.Author)
	}
	if !quotedPost.Tweet.hasMedia() {
		t.Error("quoted post should carry media")
	}
}

// ---------------------------------------------------------------------------
// Items / paths / manifest
// ---------------------------------------------------------------------------

func twFixtureCollection(t *testing.T) *twCollection {
	t.Helper()
	photo := twTestTweet("111", "alice", "two photos", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("A"), twPhotoMedia("B")}, nil)
	video := twTestTweet("222", "bob", "a video", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twVideoMedia()}, nil)
	raw, _ := json.Marshal([]interface{}{photo, video})
	var tweets []twTweetResult
	if err := json.Unmarshal(raw, &tweets); err != nil {
		t.Fatal(err)
	}
	src := TwSource{Kind: TwSourceHashtag, Hashtag: "golang", Slug: "golang", Canonical: "x:#golang"}
	return &twCollection{Source: src, Posts: twPostsFromResults(tweets, "")}
}

func TestTwPostItemsDeterministicHashtagPaths(t *testing.T) {
	coll := twFixtureCollection(t)
	items := coll.items("/out", true)

	byDest := map[string]manifest.Item{}
	for _, it := range items {
		byDest[filepath.ToSlash(it.Destination)] = it
	}
	want := []string{
		"/out/twitter/hashtags/golang/posts/111.md",
		"/out/twitter/hashtags/golang/posts/222.md",
		"/out/twitter/hashtags/golang/images/111_0.jpg",
		"/out/twitter/hashtags/golang/images/111_1.jpg",
		"/out/twitter/hashtags/golang/videos/222_0.mp4",
	}
	for _, w := range want {
		if _, ok := byDest[w]; !ok {
			t.Errorf("missing expected destination %s (have %v)", w, keysOf(byDest))
		}
	}
	if got := byDest["/out/twitter/hashtags/golang/images/111_0.jpg"].Creator; got != "alice" {
		t.Errorf("creator = %q, want alice", got)
	}
	if got := byDest["/out/twitter/hashtags/golang/videos/222_0.mp4"].URL; got != "https://video.twimg.com/hi.mp4" {
		t.Errorf("video URL = %q, want hi.mp4", got)
	}
}

func keysOf(m map[string]manifest.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestScanTwitterHashtagManifest(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	cookies := writeTwCookies(t)
	newTwSearchServer(t, func(cursor, _ string) []byte {
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-1", twTestTweet("111", "alice", "hello", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("A")}, nil)),
		}, "")
	})

	m, err := ScanTwitter(context.Background(), "x:#golang", t.TempDir(), cookies, TwOptions{IncludePosts: true})
	if err != nil {
		t.Fatalf("ScanTwitter: %v", err)
	}
	if m.SourceURL != "x:#golang" {
		t.Errorf("SourceURL = %q", m.SourceURL)
	}
	if m.Site != "twitter" {
		t.Errorf("Site = %q", m.Site)
	}
	if len(m.Items) != 2 {
		t.Fatalf("got %d items, want 2 (photo + post)", len(m.Items))
	}
	for _, it := range m.Items {
		if it.Creator != "alice" {
			t.Errorf("item %s creator = %q, want alice", it.ID, it.Creator)
		}
		if it.PostID != "111" {
			t.Errorf("item %s postID = %q", it.ID, it.PostID)
		}
		if it.PublishedAt.IsZero() {
			t.Errorf("item %s missing published time", it.ID)
		}
	}
}

func TestScanTwitterResolvedHashtag(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	cookies := writeTwCookies(t)
	newTwSearchServer(t, func(cursor, _ string) []byte {
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-1", twTestTweet("111", "alice", "hello", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("A")}, nil)),
		}, "")
	})

	src, err := ScanTwitterResolved(context.Background(), "#golang", t.TempDir(), cookies, TwOptions{})
	if err != nil {
		t.Fatalf("ScanTwitterResolved: %v", err)
	}
	if src.CanonicalURL != "x:#golang" {
		t.Errorf("CanonicalURL = %q, want x:#golang", src.CanonicalURL)
	}
	if src.URL != "#golang" {
		t.Errorf("URL = %q, want original #golang", src.URL)
	}
	if src.Author != "" {
		t.Errorf("hashtag source Author = %q, want empty (hashtag is not a creator)", src.Author)
	}
	if len(src.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(src.Items))
	}
	if src.Items[0].Author != "alice" {
		t.Errorf("item author = %q, want alice", src.Items[0].Author)
	}
	if src.Items[0].URL != "https://x.com/alice/status/111" {
		t.Errorf("item URL = %q", src.Items[0].URL)
	}
}

func TestDownloadTwitterHashtagDryRunDoesNotWrite(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	cookies := writeTwCookies(t)
	outDir := filepath.Join(t.TempDir(), "downloads")
	newTwSearchServer(t, func(cursor, _ string) []byte {
		return twTestSearchResponse([]interface{}{
			twTestTweetItem("tweet-1", twTestTweet("111", "alice", "hello", "Mon Jan 15 10:00:00 +0000 2024", []map[string]interface{}{twPhotoMedia("A")}, nil)),
		}, "")
	})

	if err := DownloadTwitter(context.Background(), "x:#golang", outDir, cookies, TwOptions{IncludePosts: true}, true); err != nil {
		t.Fatalf("DownloadTwitter dry-run: %v", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Errorf("dry-run created output directory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func TestFetchTwSearchPageMalformedSchema(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	newTwSearchServer(t, func(cursor, rawQuery string) []byte {
		return []byte(`{"data":{"something_else":{}}}`)
	})
	_, _, err := fetchTwSearchPage(context.Background(), http.DefaultClient, "", "csrf", "#tag filter:media", "", nil)
	if err == nil {
		t.Fatal("expected malformed-schema error")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error = %v, want schema hint", err)
	}
}

func TestFetchTwSearchPageAuthError(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home" {
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	oldBase, oldRefresh := twSearchAPIBase, twSearchRefreshURL
	twSearchAPIBase, twSearchRefreshURL = srv.URL, srv.URL+"/home"
	defer func() { twSearchAPIBase, twSearchRefreshURL = oldBase, oldRefresh }()

	_, _, err := fetchTwSearchPage(context.Background(), srv.Client(), "", "csrf", "#tag filter:media", "", nil)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("error = %v, want authentication failure", err)
	}
}

func TestTwRequireAuthCookies(t *testing.T) {
	if err := twRequireAuthCookies(""); err == nil {
		t.Error("empty cookies path should error")
	}
	path := filepath.Join(t.TempDir(), "c.txt")
	if err := os.WriteFile(path, []byte("# Netscape\n.x.com\tTRUE\t/\tTRUE\t0\tfoo\tbar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := twRequireAuthCookies(path); err == nil {
		t.Error("cookies without auth_token/ct0 should error")
	}
	if err := twRequireAuthCookies(writeTwCookies(t)); err != nil {
		t.Errorf("valid cookies error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Query ID discovery
// ---------------------------------------------------------------------------

func TestTwSearchQueryIDDiscovery(t *testing.T) {
	t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "")
	resetTwQueryIDs()

	respond := func(cursor, rawQuery string) []byte {
		return twTestSearchResponse(nil, "")
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><script src="http://%s/main.abc123.js"></script></html>`, r.Host)
	})
	mux.HandleFunc("/main.abc123.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `var x={queryId:"discoveredqid",operationName:"SearchTimeline"};`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SearchTimeline") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(respond("", ""))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	oldBase, oldRefresh := twSearchAPIBase, twSearchRefreshURL
	oldClient := twitterRefreshClient
	twSearchAPIBase, twSearchRefreshURL = srvURL, srvURL+"/home"
	twitterRefreshClient = srv.Client()
	defer func() {
		twSearchAPIBase, twSearchRefreshURL = oldBase, oldRefresh
		twitterRefreshClient = oldClient
		resetTwQueryIDs()
	}()

	_, _, err := fetchTwSearchPage(context.Background(), srv.Client(), "", "csrf", "#tag filter:media", "", nil)
	if err != nil {
		t.Fatalf("fetchTwSearchPage: %v", err)
	}
	if got := twSearchQueryID(); got != "discoveredqid" {
		t.Errorf("discovered query ID = %q, want discoveredqid", got)
	}
}
