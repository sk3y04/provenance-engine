package slskd

// Webhook receiver.
//
// slskd emits a small set of outbound webhooks (integrations.webhooks, plural).
// They are a low-latency wakeup only: the reconciler's REST polling is the
// authoritative state, because there are no search, byte-progress or
// transfer-failure webhooks and slskd drops webhooks with a volatile restart.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WebhookHeader is the header slskd must be configured to send. slskd's webhook
// configuration supports custom headers, so the secret travels as its own header
// rather than in a URL, where it would be logged by every proxy on the way.
const WebhookHeader = "X-Provenance-Webhook-Secret"

// maxWebhookBody bounds a delivery: slskd's payloads are tiny, so a large body is
// either a bug or an attack.
const maxWebhookBody = 64 << 10

// Webhook event types that matter here.
const (
	EventDownloadFileComplete       = "DownloadFileComplete"
	EventDownloadDirectoryComplete  = "DownloadDirectoryComplete"
	EventSoulseekClientConnected    = "SoulseekClientConnected"
	EventSoulseekClientDisconnected = "SoulseekClientDisconnected"
)

// Wake reasons handed to the callback.
const (
	WakeTransfers = "transfers"
	WakeHealth    = "health"
)

// WebhookPayload is the body slskd posts.
type WebhookPayload struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Version   int       `json:"version"`
}

// WebhookHandler authenticates and dispatches inbound slskd webhooks.
type WebhookHandler struct {
	// Secret is the shared secret. An empty secret disables the handler: it
	// refuses every delivery rather than accepting unauthenticated wakeups.
	Secret string
	// Wake is invoked for a relevant event. It must not block for long.
	Wake func(reason string)
	// DedupeTTL bounds how long a delivery id is remembered (default 5m).
	DedupeTTL time.Duration
	// MaxAge rejects a delivery whose timestamp is older than this (default 5m).
	MaxAge time.Duration
	// Now is injectable for tests.
	Now func() time.Time

	mu     sync.Mutex
	seen   map[string]time.Time
	maxIDs int
}

func (h *WebhookHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

// ServeHTTP implements http.Handler.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(h.Secret) == "" {
		http.Error(w, "webhook receiver is not configured", http.StatusServiceUnavailable)
		return
	}
	got := r.Header.Get(WebhookHeader)
	// Compare digests, not the raw strings: ConstantTimeCompare returns 0 for
	// different lengths, which leaks the secret's length to an attacker.
	gotDigest := sha256.Sum256([]byte(got))
	wantDigest := sha256.Sum256([]byte(h.Secret))
	if subtle.ConstantTimeCompare(gotDigest[:], wantDigest[:]) != 1 {
		http.Error(w, "invalid webhook secret", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxWebhookBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var payload WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.ID != "" && h.duplicate(payload.ID) {
		// A duplicate delivery is a success for the sender; nothing to do.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !payload.Timestamp.IsZero() {
		age := h.now().Sub(payload.Timestamp)
		maxAge := h.MaxAge
		if maxAge <= 0 {
			maxAge = 5 * time.Minute
		}
		if age > maxAge {
			// Stale: a late replay must not drive a fresh reconcile loop.
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if h.Wake != nil {
		switch payload.Type {
		case EventDownloadFileComplete, EventDownloadDirectoryComplete:
			h.Wake(WakeTransfers)
		case EventSoulseekClientConnected, EventSoulseekClientDisconnected:
			h.Wake(WakeHealth)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// duplicate records a delivery id and reports whether it was already seen.
func (h *WebhookHandler) duplicate(id string) bool {
	ttl := h.DedupeTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[string]time.Time{}
	}
	if h.maxIDs == 0 {
		h.maxIDs = 4096
	}
	if at, ok := h.seen[id]; ok && now.Sub(at) < ttl {
		return true
	}
	// Prune expired ids, and hard-cap the map so a hostile sender cannot grow it.
	for k, at := range h.seen {
		if now.Sub(at) >= ttl {
			delete(h.seen, k)
		}
	}
	if len(h.seen) >= h.maxIDs {
		for k := range h.seen {
			delete(h.seen, k)
			break
		}
	}
	h.seen[id] = now
	return false
}

// WebhookConfigSnippet renders the slskd YAML an operator pastes into slskd.yml.
// It is documentation, never applied by this program: writing slskd's
// configuration over HTTP requires remote_configuration and exposes secrets.
func WebhookConfigSnippet(endpoint, secret string) string {
	return fmt.Sprintf(`integrations:
  webhooks:
    - id: provenance
      url: %s
      secret: ""            # unused by slskd; Provenance verifies the header below
      content_type: application/json
      on:
        - %s
        - %s
        - %s
        - %s
      headers:
        %s: %s
`, endpoint,
		EventDownloadFileComplete, EventDownloadDirectoryComplete,
		EventSoulseekClientConnected, EventSoulseekClientDisconnected,
		WebhookHeader, secret)
}

// WebhookTest triggers a sample event in slskd, which is the only way to prove
// the delivery path end to end without waiting for a download.
func WebhookTest(ctx context.Context, c *Client) error {
	if c == nil {
		return errors.New("no slskd client")
	}
	ev, err := c.RaiseEvent(ctx, "Noop")
	if err != nil {
		return fmt.Errorf("raise test event: %w", err)
	}
	if ev == nil || ev.ID == "" {
		return errors.New("slskd accepted the test event but returned no id")
	}
	return nil
}
