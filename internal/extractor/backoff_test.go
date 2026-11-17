package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/event"
)

// TestRetryAfterDelay proves the server-supplied Retry-After is honored only
// within the documented cap, and that junk or hostile values fall back safely.
func TestRetryAfterDelay(t *testing.T) {
	const fallback = 20 * time.Second
	cases := []struct {
		header string
		want   time.Duration
		capped bool
	}{
		{"", fallback, false},
		{"junk", fallback, false},
		{"0", fallback, false},
		{"-5", fallback, false},
		{"30", 30 * time.Second, false},
		{"300", maxRetryAfter, false},
		{"301", maxRetryAfter, true},
		{"999999", maxRetryAfter, true},
		{"9223372036854775807", maxRetryAfter, true},
		// Beyond int64: Atoi fails, so the safe fallback applies.
		{"99999999999999999999999", fallback, false},
	}
	for _, tc := range cases {
		delay, capped := retryAfterDelay(tc.header, fallback)
		if delay != tc.want || capped != tc.capped {
			t.Errorf("retryAfterDelay(%q) = (%v, %v), want (%v, %v)",
				tc.header, delay, capped, tc.want, tc.capped)
		}
	}
}

// rateLimitServer answers every request with 429 and the given Retry-After,
// signaling each hit so a test can cancel while the extractor backs off.
func rateLimitServer(t *testing.T, retryAfter string) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	hit := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
		select {
		case hit <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hit
}

// TestRateLimitBackoffHonorsCancellation proves the real extractor paths — not
// just the sleepCtx helper — stop promptly when the operation is canceled
// during a rate-limit backoff, even with an enormous server Retry-After.
func TestRateLimitBackoffHonorsCancellation(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, srvURL string)
		call  func(ctx context.Context, d *Deps, srvURL string) error
	}{
		{
			name: "instagram media page",
			call: func(ctx context.Context, d *Deps, u string) error {
				_, _, err := fetchIgMediaPageHost(ctx, d, "", "", "12345", "", u, nil)
				return err
			},
		},
		{
			name: "instagram stories",
			call: func(ctx context.Context, d *Deps, u string) error {
				_, err := fetchIgStoriesHost(ctx, d, "", "", "12345", u, nil)
				return err
			},
		},
		{
			name: "reddit page",
			setup: func(t *testing.T, u string) {
				old := rdAPIBase
				rdAPIBase = u
				t.Cleanup(func() { rdAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps, u string) error {
				_, _, err := fetchRdPage(ctx, d, "", RdTarget{Name: "test", IsSubreddit: true}, "", nil)
				return err
			},
		},
		{
			name: "album html",
			call: func(ctx context.Context, d *Deps, u string) error {
				_, err := albFetchHTML(ctx, d, u, "", nil)
				return err
			},
		},
		{
			name: "twitter single post",
			setup: func(t *testing.T, u string) {
				t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
				old := twAPIBase
				twAPIBase = u
				t.Cleanup(func() { twAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps, u string) error {
				_, err := fetchTwTweetByRestID(ctx, d, "", "csrf", "1234567890123456789", nil)
				return err
			},
		},
		{
			name: "twitter search",
			setup: func(t *testing.T, u string) {
				t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
				old := twSearchAPIBase
				twSearchAPIBase = u
				t.Cleanup(func() { twSearchAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps, u string) error {
				_, _, err := fetchTwSearchPage(ctx, d, "", "csrf", "#golang", "", nil)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hit := rateLimitServer(t, "999999")
			if tc.setup != nil {
				tc.setup(t, srv.URL)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				select {
				case <-hit:
					// Give the client time to read the response and enter the
					// backoff sleep before canceling.
					time.Sleep(100 * time.Millisecond)
					cancel()
				case <-done:
				}
			}()
			start := time.Now()
			err := tc.call(ctx, testDeps(srv.Client()), srv.URL)
			close(done)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("backoff ignored cancellation: %v", elapsed)
			}
		})
	}
}

// TestServerErrorBackoffHonorsCancellation covers the 5xx retry sleep seam.
func TestServerErrorBackoffHonorsCancellation(t *testing.T) {
	hit := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
		select {
		case hit <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-hit:
			time.Sleep(100 * time.Millisecond)
			cancel()
		case <-done:
		}
	}()
	start := time.Now()
	_, _, err := fetchIgMediaPageHost(ctx, testDeps(srv.Client()), "", "", "12345", "", srv.URL, nil)
	close(done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("5xx backoff ignored cancellation: %v", elapsed)
	}
}

// TestRateLimitCapEmitsWarning proves an over-cap Retry-After is clipped to the
// documented maximum and surfaced as a stable retry_after_capped warning.
func TestRateLimitCapEmitsWarning(t *testing.T) {
	srv, _ := rateLimitServer(t, "999999")

	var mu sync.Mutex
	var events []event.Event
	sink := event.Func(func(_ context.Context, e event.Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(event.WithSink(context.Background(), sink))
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, _, err := fetchIgMediaPageHost(ctx, testDeps(srv.Client()), "", "", "12345", "", srv.URL, nil)
		errCh <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, e := range events {
			if e.Reason == event.ReasonRetryAfterCapped {
				found = true
				if !strings.Contains(e.Detail, maxRetryAfter.String()) {
					t.Errorf("cap warning detail = %q, want it to name %s", e.Detail, maxRetryAfter)
				}
			}
		}
		mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no retry_after_capped warning emitted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capped backoff ignored cancellation")
	}
}

// TestResponseEventsCarryStableFacts proves error-response narration emits a
// stable status fact — never the platform response body — when a sink is set.
func TestResponseEventsCarryStableFacts(t *testing.T) {
	const canary = "CANARYBODYSECRET auth_token=CANARYTOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"errors":[{"message":%q}]}`, canary)
	}))
	defer srv.Close()

	cases := []struct {
		name     string
		setup    func(t *testing.T)
		call     func(ctx context.Context, d *Deps) error
		wantFact bool // false for paths that return the error without narrating
	}{
		{
			name:     "instagram media page",
			wantFact: true,
			call: func(ctx context.Context, d *Deps) error {
				_, _, err := fetchIgMediaPageHost(ctx, d, "", "", "12345", "", srv.URL, nil)
				return err
			},
		},
		{
			name:     "reddit page",
			wantFact: true,
			setup: func(t *testing.T) {
				old := rdAPIBase
				rdAPIBase = srv.URL
				t.Cleanup(func() { rdAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps) error {
				_, _, err := fetchRdPage(ctx, d, "", RdTarget{Name: "test", IsSubreddit: true}, "", nil)
				return err
			},
		},
		{
			name:     "album html",
			wantFact: true,
			call: func(ctx context.Context, d *Deps) error {
				_, err := albFetchHTML(ctx, d, srv.URL, "", nil)
				return err
			},
		},
		{
			name: "twitter single post",
			setup: func(t *testing.T) {
				t.Setenv("TWITTER_QUERY_TWEET_RESULT_BY_REST_ID", "testqid")
				old := twAPIBase
				twAPIBase = srv.URL
				t.Cleanup(func() { twAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps) error {
				_, err := fetchTwTweetByRestID(ctx, d, "", "csrf", "1234567890123456789", nil)
				return err
			},
		},
		{
			name:     "twitter search",
			wantFact: true,
			setup: func(t *testing.T) {
				t.Setenv("TWITTER_QUERY_SEARCH_TIMELINE", "testqid")
				old := twSearchAPIBase
				twSearchAPIBase = srv.URL
				t.Cleanup(func() { twSearchAPIBase = old })
			},
			call: func(ctx context.Context, d *Deps) error {
				_, _, err := fetchTwSearchPage(ctx, d, "", "csrf", "#golang", "", nil)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			var got []event.Event
			sink := event.Func(func(_ context.Context, e event.Event) { got = append(got, e) })
			ctx := event.WithSink(context.Background(), sink)
			err := tc.call(ctx, testDeps(srv.Client()))
			if err == nil {
				t.Fatal("expected an error from a 400 response")
			}
			if strings.Contains(err.Error(), "CANARYBODYSECRET") || strings.Contains(err.Error(), "CANARYTOKEN") {
				t.Fatalf("response body leaked into error: %v", err)
			}
			statusFact := false
			for _, e := range got {
				if strings.Contains(e.Detail, "CANARYBODYSECRET") || strings.Contains(e.Detail, "CANARYTOKEN") {
					t.Fatalf("response body leaked into event: %+v", e)
				}
				if e.Detail == "status 400" {
					statusFact = true
				}
			}
			if !statusFact && tc.wantFact {
				t.Fatalf("no stable status fact emitted; events = %+v", got)
			}
		})
	}
}
