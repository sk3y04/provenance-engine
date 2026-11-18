package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchAllTwTweetsRepeatedCursorStops: a misbehaving API that keeps
// returning the same bottom cursor with non-empty pages must terminate
// pagination (the search path already guards this; the profile path must
// too). The context timeout keeps the unfixed behavior a fast failure
// instead of a hung test.
func TestFetchAllTwTweetsRepeatedCursorStops(t *testing.T) {
	page := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page++
		id := fmt.Sprintf("t%d", page)
		entries := []interface{}{
			twTestTweetItem("tweet-"+id, twTestTweet(id, "alice", "post "+id, "Mon Jan 15 10:00:00 +0000 2024", nil, nil)),
			twTestCursorEntry("LOOP"),
		}
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"result": map[string]interface{}{
						"timeline": map[string]interface{}{
							"timeline": map[string]interface{}{
								"instructions": []interface{}{
									map[string]interface{}{"type": "TimelineAddEntries", "entries": entries},
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldBase, oldRefresh := twAPIBase, twSearchRefreshURL
	twAPIBase, twSearchRefreshURL = srv.URL, srv.URL+"/home"
	t.Cleanup(func() { twAPIBase, twSearchRefreshURL = oldBase, oldRefresh })

	d := testDeps(srv.Client())
	// Pre-populate every query ID so refresh never touches the network.
	for _, op := range []string{"UserByScreenName", "UserMedia", "UserTweets", "SearchTimeline", "TweetResultByRestId"} {
		d.QueryIDs.set(op, "testqid")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	posts, err := FetchAllTwTweets(ctx, d, "", "csrf", "42", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllTwTweets: %v (repeated cursor must stop pagination, not loop until timeout)", err)
	}
	if len(posts) != 2 {
		t.Errorf("posts = %d, want 2 (one per page before the repeated cursor is detected)", len(posts))
	}
}

// TestFetchAllIgPostsRepeatedCursorStops: same guard for the Instagram
// profile feed paginator — a repeated next_max_id must end the scan.
func TestFetchAllIgPostsRepeatedCursorStops(t *testing.T) {
	page := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page++
		resp := map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{
					"id":         fmt.Sprintf("m%d", page),
					"code":       fmt.Sprintf("C%d", page),
					"media_type": 1,
				},
			},
			"next_max_id":    "LOOP",
			"more_available": true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldHosts := igAPIHosts
	igAPIHosts = []string{srv.URL}
	t.Cleanup(func() { igAPIHosts = oldHosts })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Empty username skips the server-rendered profile path and exercises
	// the API pagination loop directly.
	posts, err := FetchAllIgPosts(ctx, testDeps(srv.Client()), "", "", "42", "", 0, nil)
	if err != nil {
		t.Fatalf("FetchAllIgPosts: %v (repeated cursor must stop pagination, not loop until timeout)", err)
	}
	if len(posts) != 2 {
		t.Errorf("posts = %d, want 2 (one per page before the repeated cursor is detected)", len(posts))
	}
}
