package extractor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
)

// fakeTransport serves a canned body for every request so a scan path can be
// driven offline without contacting a live platform.
type fakeTransport struct{ body []byte }

func (f *fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(f.body)),
	}, nil
}

// TestNoTerminalOutputForEveryFacadeExtractor is the acceptance gate: a facade
// scan path with a sink on the operation context emits events and writes nothing
// to the terminal, for every supported extractor.
func TestNoTerminalOutputForEveryFacadeExtractor(t *testing.T) {
	cases := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"reddit", func(ctx context.Context) error {
			d := &Deps{HTTPClient: &http.Client{Transport: &fakeTransport{body: loadTestFixture(t, "reddit_subreddit.json")}}, QueryIDs: &QueryIDCache{}}
			_, err := ScanRedditResolved(ctx, "https://www.reddit.com/r/wallpapers/", t.TempDir(), "", RdOptions{Deps: d, RateLimiter: ratelimit.New()})
			return err
		}},
		{"instagram", func(ctx context.Context) error {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(loadTestFixture(t, "instagram_profile.json"))
			}))
			defer srv.Close()
			old := igAPIHosts
			igAPIHosts = []string{srv.URL}
			defer func() { igAPIHosts = old }()
			d := &Deps{HTTPClient: srv.Client(), QueryIDs: &QueryIDCache{}}
			_, err := ScanInstagramResolved(ctx, "https://www.instagram.com/username/", t.TempDir(), "", IgOptions{Deps: d, RateLimiter: ratelimit.New()})
			return err
		}},
		{"twitter", func(ctx context.Context) error {
			ts := newTwSearchServer(t, nil)
			_, err := ScanTwitterResolved(ctx, "https://x.com/testuser", t.TempDir(), "", TwOptions{Deps: ts.deps, RateLimiter: ratelimit.New()})
			return err
		}},
		{"album", func(ctx context.Context) error {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"items":[]}}`))
			}))
			defer srv.Close()
			d := swapAlbumCDN(t, srv)
			_, err := ScanAlbumResolved(ctx, "https://example.invalid/album/1", t.TempDir(), "", AlbumOptions{Deps: d, RateLimiter: ratelimit.New()})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			sink := event.Func(func(_ context.Context, _ event.Event) { got++ })
			ctx := event.WithSink(context.Background(), sink)

			stdout, stderr := withStdRedirects(t, func() {
				_ = tc.run(ctx)
				// narrate must route to the sink (not the terminal) on this path.
				narratef(ctx, kindInfo, reasonInfo, "", "", "[test] probe\n")
			})
			if stdout != "" || stderr != "" {
				t.Fatalf("terminal output from %s scan with a sink: stdout=%q stderr=%q", tc.name, stdout, stderr)
			}
			if got == 0 {
				t.Fatalf("%s scan did not carry the sink on the operation context", tc.name)
			}
		})
	}
}
