package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newWebhookHandler(wake *int32, reason *string) *WebhookHandler {
	return &WebhookHandler{
		Secret: "s3cret-value",
		Wake: func(r string) {
			if wake != nil {
				atomic.AddInt32(wake, 1)
			}
			if reason != nil {
				*reason = r
			}
		},
	}
}

func postWebhook(t *testing.T, h http.Handler, secret string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/slskd/webhook", strings.NewReader(body))
	if secret != "" {
		req.Header.Set(WebhookHeader, secret)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWebhookRejectsMissingOrWrongSecret is the only authentication boundary
// this integration exposes to the network.
func TestWebhookRejectsMissingOrWrongSecret(t *testing.T) {
	var wake int32
	h := newWebhookHandler(&wake, nil)
	body := `{"id":"e1","type":"DownloadFileComplete","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`

	if rec := postWebhook(t, h, "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret: %d", rec.Code)
	}
	if rec := postWebhook(t, h, "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", rec.Code)
	}
	if wake != 0 {
		t.Fatal("an unauthenticated delivery triggered a wakeup")
	}
}

// TestWebhookRefusesWhenUnconfigured proves an empty secret disables the endpoint
// instead of accepting anything.
func TestWebhookRefusesWhenUnconfigured(t *testing.T) {
	h := &WebhookHandler{}
	rec := postWebhook(t, h, "", `{"id":"e1","type":"DownloadFileComplete"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

// TestWebhookDeduplicatesByID proves a redelivered event does not trigger a
// second reconcile.
func TestWebhookDeduplicatesByID(t *testing.T) {
	var wake int32
	h := newWebhookHandler(&wake, nil)
	body := `{"id":"dup-1","type":"DownloadFileComplete","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	for i := 0; i < 3; i++ {
		if rec := postWebhook(t, h, "s3cret-value", body); rec.Code != http.StatusNoContent {
			t.Fatalf("delivery %d: code = %d", i, rec.Code)
		}
	}
	if wake != 1 {
		t.Fatalf("wakeups = %d, want 1", wake)
	}
}

// TestWebhookIgnoresStaleTimestamp proves a replayed old delivery is accepted but
// not acted on.
func TestWebhookIgnoresStaleTimestamp(t *testing.T) {
	var wake int32
	h := newWebhookHandler(&wake, nil)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	rec := postWebhook(t, h, "s3cret-value", `{"id":"old-1","type":"DownloadFileComplete","timestamp":"`+old+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	if wake != 0 {
		t.Fatal("a stale delivery triggered a reconcile")
	}
}

// TestWebhookDispatchesByEventType proves only the events that matter wake the
// right loop, and everything else is a no-op.
func TestWebhookDispatchesByEventType(t *testing.T) {
	recent := time.Now().UTC().Format(time.RFC3339Nano)
	cases := map[string]string{
		"DownloadFileComplete":       WakeTransfers,
		"DownloadDirectoryComplete":  WakeTransfers,
		"SoulseekClientConnected":    WakeHealth,
		"SoulseekClientDisconnected": WakeHealth,
	}
	for eventType, wantReason := range cases {
		var wake int32
		var reason string
		h := newWebhookHandler(&wake, &reason)
		n := time.Now().UnixNano()
		body := `{"id":"e-` + eventType + `-` + time.Duration(n).String() + `","type":"` + eventType + `","timestamp":"` + recent + `"}`
		if rec := postWebhook(t, h, "s3cret-value", body); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: code = %d", eventType, rec.Code)
		}
		if wake != 1 || reason != wantReason {
			t.Fatalf("%s: wake=%d reason=%q, want %q", eventType, wake, reason, wantReason)
		}
	}

	var wake int32
	h := newWebhookHandler(&wake, nil)
	rec := postWebhook(t, h, "s3cret-value", `{"id":"e-other","type":"UploadFileComplete","timestamp":"`+recent+`"}`)
	if rec.Code != http.StatusNoContent || wake != 0 {
		t.Fatalf("unrelated event: code=%d wake=%d", rec.Code, wake)
	}
}

// TestWebhookRejectsMalformedJSON keeps a broken sender from triggering work.
func TestWebhookRejectsMalformedJSON(t *testing.T) {
	var wake int32
	h := newWebhookHandler(&wake, nil)
	if rec := postWebhook(t, h, "s3cret-value", "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
	if wake != 0 {
		t.Fatal("malformed body triggered a wakeup")
	}
}

// TestWebhookDedupeCacheIsBounded proves a flood of distinct ids cannot grow the
// dedupe cache without bound.
func TestWebhookDedupeCacheIsBounded(t *testing.T) {
	h := newWebhookHandler(nil, nil)
	h.maxIDs = 8
	for i := 0; i < 50; i++ {
		h.duplicate("id-" + time.Duration(i).String())
	}
	h.mu.Lock()
	size := len(h.seen)
	h.mu.Unlock()
	if size > 8 {
		t.Fatalf("dedupe cache grew to %d entries", size)
	}
}

// TestWebhookConfigSnippetKeepsSecretsOutOfTheURL proves the documented slskd
// configuration sends the secret as a header.
func TestWebhookConfigSnippetKeepsSecretsOutOfTheURL(t *testing.T) {
	snippet := WebhookConfigSnippet("http://provenance:8080/api/integrations/slskd/webhook", "top-secret")
	if strings.Contains(snippet, "top-secret") && strings.Contains(snippet, "url:") &&
		strings.Contains(strings.SplitN(snippet, "\n", 3)[1], "top-secret") {
		t.Fatal("secret appears in the webhook URL")
	}
	if !strings.Contains(snippet, WebhookHeader+": top-secret") {
		t.Fatalf("snippet does not send the secret header:\n%s", snippet)
	}
	for _, want := range []string{EventDownloadFileComplete, EventDownloadDirectoryComplete,
		EventSoulseekClientConnected, EventSoulseekClientDisconnected} {
		if !strings.Contains(snippet, want) {
			t.Errorf("snippet is missing event %s", want)
		}
	}
}

// TestWebhookTestCallsNoop proves the self-test path uses the documented endpoint
// and reports a real id.
func TestWebhookTestCallsNoop(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "evt-1", "type": "Noop", "version": 0})
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WebhookTest(context.Background(), c); err != nil {
		t.Fatalf("WebhookTest: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v0/events/Noop" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
}
