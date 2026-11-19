package slskd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestClient builds a Client pointed at an httptest server.
func newTestClient(t *testing.T, srv *httptest.Server, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{BaseURL: srv.URL, APIKey: "test-api-key-0123456789"}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestClientSendsAPIKeyOnly pins the credential transport. slskd rejects
// Authorization: Bearer with 401 invalid_token, so a regression here breaks
// every call in a way that looks like a bad key.
func TestClientSendsAPIKeyOnly(t *testing.T) {
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, 200, map[string]any{"version": map[string]any{"current": "0.26.0.0"}})
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv, nil).GetApplication(context.Background()); err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if gotKey != "test-api-key-0123456789" {
		t.Fatalf("X-API-Key = %q, want the configured key", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header was sent (%q); slskd requires X-API-Key", gotAuth)
	}
}

// TestClientURLBaseJoining proves the urlBase prefix is joined exactly once and
// never produces a double slash, which slskd answers with 404.
func TestClientURLBaseJoining(t *testing.T) {
	for _, urlBase := range []string{"/slskd", "slskd/", "/slskd/"} {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			writeJSON(w, 200, map[string]any{"state": "Connected, LoggedIn", "isConnected": true, "isLoggedIn": true})
		}))
		c := newTestClient(t, srv, func(cfg *Config) { cfg.URLBase = urlBase })
		if _, err := c.GetServer(context.Background()); err != nil {
			t.Fatalf("urlBase %q: GetServer: %v", urlBase, err)
		}
		srv.Close()
		if gotPath != "/slskd/api/v0/server" {
			t.Fatalf("urlBase %q produced path %q, want /slskd/api/v0/server", urlBase, gotPath)
		}
	}
}

// TestClientRejectsMalformedBaseURL keeps an obvious configuration mistake from
// turning into a confusing transport error at call time.
func TestClientRejectsMalformedBaseURL(t *testing.T) {
	for _, base := range []string{"", "   ", "127.0.0.1:5030", "ftp://example.com"} {
		if _, err := New(Config{BaseURL: base, APIKey: "k"}); err == nil {
			t.Fatalf("New accepted base URL %q", base)
		}
	}
	if _, err := New(Config{BaseURL: "http://127.0.0.1:5030"}); err == nil {
		t.Fatal("New accepted an empty API key")
	}
}

// TestClientAPIErrorClassification proves a 401 becomes a typed error the health
// probe can classify without string matching.
func TestClientAPIErrorClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := newTestClient(t, srv, nil).Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError", err)
	}
	if apiErr.StatusCode != 401 {
		t.Fatalf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if !IsAuthError(err) {
		t.Fatal("IsAuthError(401) = false")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error text %q does not name the status", err.Error())
	}
}

// TestClientRedactsSecretsFromErrors proves the API key and webhook secret never
// reach an error string, including when slskd echoes the request back.
func TestClientRedactsSecretsFromErrors(t *testing.T) {
	const key = "super-secret-api-key-value"
	const secret = "super-secret-webhook-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rejected key "+key+" and secret "+secret, http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, func(cfg *Config) {
		cfg.APIKey = key
		cfg.WebhookSecret = secret
	})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaked the API key: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the webhook secret: %v", err)
	}
}

// TestClientTLSVerification proves TLS is strict by default and relaxed only on
// explicit opt-in: a self-signed slskd behind a reverse proxy is a real
// deployment, but trusting it silently is not acceptable.
func TestClientTLSVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, "0.26.0.0")
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("self-signed certificate was accepted without opt-in")
	}

	c = newTestClient(t, srv, func(cfg *Config) { cfg.InsecureSkipVerify = true })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("opt-in insecureSkipVerify still failed: %v", err)
	}
}

// TestClientPerOperationTimeouts proves a slow endpoint yields a bounded error
// instead of hanging the caller.
func TestClientPerOperationTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	c := newTestClient(t, srv, func(cfg *Config) { cfg.HealthTimeout = 50 * time.Millisecond })
	start := time.Now()
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Ping took %v, timeout not applied", elapsed)
	}
	if !IsTimeout(err) {
		t.Fatalf("IsTimeout(%v) = false", err)
	}
}

// TestClientEscapesPathSegments proves peer names and ids are escaped rather
// than concatenated into the URL, so a name containing a slash cannot reach a
// different endpoint.
func TestClientEscapesPathSegments(t *testing.T) {
	var gotPath, gotEscaped string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEscaped = r.URL.EscapedPath()
		writeJSON(w, 200, map[string]any{"position": 3})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if _, err := c.QueuePosition(context.Background(), "peer/with slash", "id-1"); err != nil {
		t.Fatalf("QueuePosition: %v", err)
	}
	if strings.Count(gotEscaped, "/api/v0/transfers/downloads/") != 1 {
		t.Fatalf("unexpected path %q (escaped %q)", gotPath, gotEscaped)
	}
	if !strings.Contains(gotEscaped, "peer%2Fwith%20slash") {
		t.Fatalf("peer name was not escaped: %q", gotEscaped)
	}
}

// TestClientBatchEnqueueOutcomes pins the three success-shaped status codes:
// 201 all accepted, 207 partial (failures reported, no error), 200 all failed
// (still an error, because nothing was queued).
func TestClientBatchEnqueueOutcomes(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		wantErr    bool
		wantFailed int
	}{
		{201, `{"batch":{"id":"b1","username":"peer","transfers":[{"id":"t1","state":"Queued, Remotely"}]}}`, false, 0},
		{207, `{"batch":{"id":"b1"},"failures":[{"filename":"x.flac","message":"queued twice"}]}`, false, 1},
		{200, `{"failures":[{"filename":"x.flac","message":"peer rejected"}]}`, true, 1},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, tc.status, json.RawMessage(tc.body))
		}))
		resp, err := newTestClient(t, srv, nil).EnqueueBatch(context.Background(), EnqueueBatchRequest{
			Username: "peer",
			Files:    []EnqueueBatchItem{{Filename: "music\\x.flac", Size: 1}},
		})
		srv.Close()
		if tc.wantErr != (err != nil) {
			t.Fatalf("status %d: err = %v, wantErr %v", tc.status, err, tc.wantErr)
		}
		if tc.wantErr {
			continue
		}
		if len(resp.Failures) != tc.wantFailed {
			t.Fatalf("status %d: %d failures, want %d", tc.status, len(resp.Failures), tc.wantFailed)
		}
	}
}

// TestClientNoContentResponses proves 204 endpoints (DELETE/PUT) decode cleanly
// instead of failing on an empty body.
func TestClientNoContentResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	ctx := context.Background()
	if err := c.DeleteSearch(ctx, "id"); err != nil {
		t.Fatalf("DeleteSearch: %v", err)
	}
	if err := c.CancelDownload(ctx, "peer", "id", true); err != nil {
		t.Fatalf("CancelDownload: %v", err)
	}
	if err := c.RescanShares(ctx); err != nil {
		t.Fatalf("RescanShares: %v", err)
	}
}

// TestClientBodyIsBounded proves a hostile or broken slskd cannot make the client
// buffer unbounded memory.
func TestClientBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		chunk := strings.Repeat("a", 1<<16)
		for i := 0; i < maxResponseBytes/(1<<16)+4; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	err := newTestClient(t, srv, nil).Ping(context.Background())
	if err == nil {
		t.Fatal("expected a body-too-large error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error %q does not explain the size limit", err)
	}
}

// TestClientSearchRoundTrip proves the search document decodes including the
// lockedFiles array that carries files a peer has locked.
func TestClientSearchRoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/searches"):
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			writeJSON(w, 200, map[string]any{"id": "s1", "state": "InProgress", "isComplete": false})
		case strings.HasSuffix(r.URL.Path, "/searches/s1"):
			if r.URL.Query().Get("includeResponses") != "true" {
				t.Errorf("includeResponses missing")
			}
			writeJSON(w, 200, map[string]any{
				"id": "s1", "state": "Completed, TimedOut", "isComplete": true,
				"responseCount": 1, "fileCount": 2,
				"responses": []any{map[string]any{
					"username": "peer", "uploadSpeed": 1290178, "hasFreeUploadSlot": true,
					"files":       []any{map[string]any{"filename": `music\a.flac`, "size": 10, "extension": ""}},
					"lockedFiles": []any{map[string]any{"filename": `music\b.flac`, "size": 20}},
				}},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, nil)
	if _, err := c.CreateSearch(context.Background(), SearchRequest{ID: "s1", SearchText: "q", SearchTimeoutMS: 15000}); err != nil {
		t.Fatalf("CreateSearch: %v", err)
	}
	if got, _ := gotBody["searchTimeout"].(float64); got != 15000 {
		t.Fatalf("searchTimeout sent as %v, want 15000 ms", gotBody["searchTimeout"])
	}
	s, err := c.GetSearch(context.Background(), "s1", true)
	if err != nil {
		t.Fatalf("GetSearch: %v", err)
	}
	if len(s.Responses) != 1 || len(s.Responses[0].Files) != 1 || len(s.Responses[0].LockedFiles) != 1 {
		t.Fatalf("unexpected search decode: %+v", s)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	switch raw := v.(type) {
	case json.RawMessage:
		_, _ = w.Write(raw)
	default:
		_ = json.NewEncoder(w).Encode(v)
	}
}

// TestClientConcurrentUse proves one Client is safe for concurrent readers; the
// reconciler and the UI poll simultaneously.
func TestClientConcurrentUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"state": "None"})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Ping(context.Background())
		}()
	}
	wg.Wait()
}
