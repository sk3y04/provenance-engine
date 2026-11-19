package slskd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// searchServer is a scripted slskd search endpoint.
type searchServer struct {
	mu            sync.Mutex
	creates       []map[string]any
	createStatus  []int // per attempt; the last value repeats
	polls         int
	completeAfter int // number of polls before isComplete
	responses     []any
	stopCalls     int32
	deleteCalls   int32
	maxConcurrent int32
	concurrent    int32
	createDelay   time.Duration
}

func (s *searchServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v0/searches"):
			cur := atomic.AddInt32(&s.concurrent, 1)
			for {
				max := atomic.LoadInt32(&s.maxConcurrent)
				if cur <= max || atomic.CompareAndSwapInt32(&s.maxConcurrent, max, cur) {
					break
				}
			}
			if s.createDelay > 0 {
				time.Sleep(s.createDelay)
			}
			defer atomic.AddInt32(&s.concurrent, -1)

			body, _ := io.ReadAll(r.Body)
			var decoded map[string]any
			_ = json.Unmarshal(body, &decoded)
			s.mu.Lock()
			s.creates = append(s.creates, decoded)
			status := http.StatusOK
			if len(s.createStatus) > 0 {
				idx := len(s.creates) - 1
				if idx >= len(s.createStatus) {
					idx = len(s.createStatus) - 1
				}
				status = s.createStatus[idx]
			}
			s.mu.Unlock()
			if status != http.StatusOK {
				http.Error(w, "throttled", status)
				return
			}
			id, _ := decoded["id"].(string)
			writeJSON(w, 200, map[string]any{"id": id, "state": "InProgress", "isComplete": false})

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v0/searches/"):
			s.mu.Lock()
			s.polls++
			poll := s.polls
			completeAfter := s.completeAfter
			responses := s.responses
			s.mu.Unlock()
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body := map[string]any{
				"id": id, "state": "InProgress", "isComplete": false,
				"responseCount": len(responses), "fileCount": countFiles(responses),
			}
			if completeAfter >= 0 && poll >= completeAfter {
				body["state"] = "Completed, ResponseLimitReached"
				body["isComplete"] = true
				body["responses"] = responses
			}
			writeJSON(w, 200, body)

		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/api/v0/searches/"):
			atomic.AddInt32(&s.stopCalls, 1)
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v0/searches/"):
			atomic.AddInt32(&s.deleteCalls, 1)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.NotFound(w, r)
		}
	})
}

func countFiles(responses []any) int {
	n := 0
	for _, r := range responses {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if files, ok := m["files"].([]any); ok {
			n += len(files)
		}
		if files, ok := m["lockedFiles"].([]any); ok {
			n += len(files)
		}
	}
	return n
}

func twoPeerResponses() []any {
	return []any{
		map[string]any{
			"username": "slowpeer", "uploadSpeed": 20000, "hasFreeUploadSlot": false, "queueLength": 60,
			"files": []any{
				map[string]any{"filename": `music\Album\01 track.flac`, "size": 100},
				map[string]any{"filename": `music\Album\02 track.flac`, "size": 200},
			},
		},
		map[string]any{
			"username": "fastpeer", "uploadSpeed": 1290178, "hasFreeUploadSlot": true, "queueLength": 0,
			"files": []any{
				map[string]any{"filename": `music\Album\01 track.flac`, "size": 100},
			},
		},
	}
}

func newSearchCoordinator(t *testing.T, h http.Handler) (*SearchCoordinator, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "k", SearchTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return NewSearchCoordinator(c), srv
}

// TestSearchSerialisesConcurrentCreates is the #1483 defence: slskd permits one
// search at a time and answers 429 to the second, so two callers must never have
// a create in flight simultaneously.
func TestSearchSerialisesConcurrentCreates(t *testing.T) {
	ss := &searchServer{completeAfter: 1, responses: twoPeerResponses(), createDelay: 40 * time.Millisecond}
	sc, _ := newSearchCoordinator(t, ss.handler())

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sc.Search(context.Background(), SearchOptions{
				Query: "beethoven", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
			}); err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&ss.maxConcurrent); got != 1 {
		t.Fatalf("max concurrent creates = %d, want 1", got)
	}
	if len(ss.creates) != 3 {
		t.Fatalf("%d creates, want 3", len(ss.creates))
	}
}

// TestSearchRequestContract pins the create payload: milliseconds, and the
// filter/limit fields the spec requires.
func TestSearchRequestContract(t *testing.T) {
	ss := &searchServer{completeAfter: 1, responses: twoPeerResponses()}
	sc, _ := newSearchCoordinator(t, ss.handler())
	if _, err := sc.Search(context.Background(), SearchOptions{
		Query: "  beethoven  ", TimeoutMS: 15000, FileLimit: 500, ResponseLimit: 50,
		PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	}); err != nil {
		t.Fatal(err)
	}
	body := ss.creates[0]
	if body["searchText"] != "beethoven" {
		t.Fatalf("searchText = %v, want the trimmed query", body["searchText"])
	}
	if body["searchTimeout"] != float64(15000) {
		t.Fatalf("searchTimeout = %v, want 15000 (milliseconds)", body["searchTimeout"])
	}
	if body["fileLimit"] != float64(500) || body["responseLimit"] != float64(50) {
		t.Fatalf("limits = %v/%v", body["fileLimit"], body["responseLimit"])
	}
	if body["filterResponses"] != true {
		t.Fatalf("filterResponses = %v", body["filterResponses"])
	}
}

// TestSearchRetriesThrottle proves a transient 429 is retried with backoff and
// never surfaces as a user-visible failure.
func TestSearchRetriesThrottle(t *testing.T) {
	ss := &searchServer{
		createStatus:  []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK},
		completeAfter: 1,
		responses:     twoPeerResponses(),
	}
	sc, _ := newSearchCoordinator(t, ss.handler())
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("Search after throttle: %v", err)
	}
	if len(ss.creates) != 3 {
		t.Fatalf("create attempts = %d, want 3", len(ss.creates))
	}
	if len(res.Groups) != 2 {
		t.Fatalf("groups = %d", len(res.Groups))
	}
}

// TestSearchPersistentThrottleFails proves the retry is bounded.
func TestSearchPersistentThrottleFails(t *testing.T) {
	ss := &searchServer{createStatus: []int{http.StatusTooManyRequests}, completeAfter: 1}
	sc, _ := newSearchCoordinator(t, ss.handler())
	if _, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", PollInitial: time.Millisecond, PollMax: time.Millisecond, SettleGrace: -1,
	}); err == nil {
		t.Fatal("persistent 429 was reported as success")
	}
}

// TestSearchWallClockStopsRunaway is the guard for slskd resetting its own
// timeout on every peer response: a search that never completes must be stopped
// by us, on the clock.
func TestSearchWallClockStopsRunaway(t *testing.T) {
	ss := &searchServer{completeAfter: -1, responses: nil}
	sc, _ := newSearchCoordinator(t, ss.handler())
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", WallClock: 120 * time.Millisecond,
		PollInitial: 20 * time.Millisecond, PollMax: 20 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.Stopped {
		t.Fatal("runaway search was not marked stopped")
	}
	if atomic.LoadInt32(&ss.stopCalls) == 0 {
		t.Fatal("PUT /searches/{id} was never called; the search keeps running in slskd")
	}
	if res.Duration > 2*time.Second {
		t.Fatalf("search ran %v, wall clock was 120ms", res.Duration)
	}
}

// TestSearchContextCancelStops proves a cancelled caller stops the slskd search
// rather than leaving it running.
func TestSearchContextCancelStops(t *testing.T) {
	ss := &searchServer{completeAfter: -1}
	sc, _ := newSearchCoordinator(t, ss.handler())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	res, err := sc.Search(ctx, SearchOptions{
		Query: "q", PollInitial: 10 * time.Millisecond, PollMax: 10 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.Stopped {
		t.Fatal("cancelled search was not marked stopped")
	}
	if atomic.LoadInt32(&ss.stopCalls) == 0 {
		t.Fatal("stop was not issued on cancellation")
	}
}

// TestSearchGroupsAndRanks proves results are grouped per (peer, folder), locked
// files are counted but not offered, and a peer with a free slot ranks first.
func TestSearchGroupsAndRanks(t *testing.T) {
	responses := []any{
		map[string]any{
			"username": "lockedpeer", "uploadSpeed": 999999, "hasFreeUploadSlot": true,
			"files":       []any{},
			"lockedFiles": []any{map[string]any{"filename": `music\Album\x.flac`, "size": 10, "isLocked": true}},
		},
		map[string]any{
			"username": "slowpeer", "uploadSpeed": 20000, "hasFreeUploadSlot": false, "queueLength": 60,
			"files": []any{
				map[string]any{"filename": `music\Album\01.flac`, "size": 100},
				map[string]any{"filename": `music\Album\02.flac`, "size": 200},
			},
		},
		map[string]any{
			"username": "fastpeer", "uploadSpeed": 1290178, "hasFreeUploadSlot": true, "queueLength": 0,
			"files": []any{map[string]any{"filename": `music\Album\01.flac`, "size": 100}},
		},
	}
	ss := &searchServer{completeAfter: 1, responses: responses}
	sc, _ := newSearchCoordinator(t, ss.handler())
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (locked-only peer dropped): %+v", len(res.Groups), res.Groups)
	}
	if res.Groups[0].Username != "fastpeer" {
		t.Fatalf("first group = %q, want the peer with a free slot", res.Groups[0].Username)
	}
	if res.Groups[0].Directory != "music/Album" {
		t.Fatalf("directory = %q", res.Groups[0].Directory)
	}
	if res.Groups[1].TotalBytes != 300 {
		t.Fatalf("total bytes = %d", res.Groups[1].TotalBytes)
	}
}

// TestSearchDropsIgnoredPeers covers the download-side blocklist.
func TestSearchDropsIgnoredPeers(t *testing.T) {
	ss := &searchServer{completeAfter: 1, responses: twoPeerResponses()}
	sc, _ := newSearchCoordinator(t, ss.handler())
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", IgnoredPeers: []string{"FASTPEER"},
		PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range res.Groups {
		if strings.EqualFold(g.Username, "fastpeer") {
			t.Fatal("ignored peer appeared in results")
		}
	}
}

// TestSearchDeletesAfterSettle is the #1819 guard: the DELETE must happen, and
// only after the settle grace.
func TestSearchDeletesAfterSettle(t *testing.T) {
	ss := &searchServer{completeAfter: 1, responses: twoPeerResponses()}
	sc, _ := newSearchCoordinator(t, ss.handler())
	if _, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: 80 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&ss.deleteCalls); n != 0 {
		t.Fatalf("DELETE happened immediately (%d calls before the settle grace)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sc.WaitCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&ss.deleteCalls); n != 1 {
		t.Fatalf("DELETE calls = %d, want 1 after the grace", n)
	}
}

// TestSearchDeleteFailureDoesNotFailSearch proves a cleanup failure is not
// allowed to discard results the user already has.
func TestSearchDeleteFailureDoesNotFailSearch(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(w, 200, map[string]any{"id": "s1", "isComplete": false})
		case r.Method == http.MethodGet:
			writeJSON(w, 200, map[string]any{"id": "s1", "isComplete": true, "responses": twoPeerResponses()})
		default:
			http.Error(w, "nope", http.StatusInternalServerError)
		}
	})
	sc, _ := newSearchCoordinator(t, h)
	res, err := sc.Search(context.Background(), SearchOptions{
		Query: "q", PollInitial: 5 * time.Millisecond, PollMax: 5 * time.Millisecond, SettleGrace: -1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Groups) == 0 {
		t.Fatal("results were discarded")
	}
}

// TestSearchEmptyQueryIsRejected keeps a pointless broadcast from reaching the
// network.
func TestSearchEmptyQueryIsRejected(t *testing.T) {
	sc, _ := newSearchCoordinator(t, (&searchServer{}).handler())
	for _, q := range []string{"", "   "} {
		if _, err := sc.Search(context.Background(), SearchOptions{Query: q}); err == nil {
			t.Fatalf("Search(%q) accepted", q)
		}
	}
}
