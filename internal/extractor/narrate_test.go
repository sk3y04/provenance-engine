package extractor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/event"
)

// withStdRedirects captures os.Stdout and os.Stderr for the duration of fn and
// returns what each stream received.
func withStdRedirects(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()

	done := make(chan struct{})
	var ob, eb bytes.Buffer
	go func() { _, _ = io.Copy(&ob, outR); close(done) }()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	_, _ = io.Copy(&eb, errR)
	<-done
	return ob.String(), eb.String()
}

// TestNarrateIsTransportNeutral proves the gate: with a sink on the context no
// terminal write happens, and without a sink the legacy CLI text is preserved
// byte-for-byte.
func TestNarrateIsTransportNeutral(t *testing.T) {
	cases := []struct {
		name   string
		kind   event.Kind
		reason string
		legacy string
		args   []any
	}{
		{"pagination", kindInfo, event.ReasonPageResults, "[twitter] page %d: %d tweets (%d total)\n", []any{2, 5, 9}},
		{"rate limit", kindWarn, event.ReasonRateLimited, "[instagram] rate limited (429), waiting %v...\n", []any{"2s"}},
		{"dry run", kindInfo, event.ReasonDryRun, "[dry-run] reddit: %s -> %s\n", []any{"https://example.com/a.jpg", "/tmp/a.jpg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []event.Event
			sink := event.Func(func(_ context.Context, e event.Event) { got = append(got, e) })

			_, stderr := withStdRedirects(t, func() {
				narratef(event.WithSink(context.Background(), sink), tc.kind, tc.reason, "", "", tc.legacy, tc.args...)
			})
			if stderr != "" {
				t.Fatalf("terminal write with a sink: %q", stderr)
			}
			if len(got) != 1 {
				t.Fatalf("events = %d, want 1", len(got))
			}
			if got[0].Kind != tc.kind || got[0].Reason != tc.reason {
				t.Fatalf("event = %+v", got[0])
			}

			stdout, stderr := withStdRedirects(t, func() {
				narratef(context.Background(), tc.kind, tc.reason, "", "", tc.legacy, tc.args...)
			})
			want := fmt.Sprintf(tc.legacy, tc.args...)
			if stdout+stderr != want {
				t.Fatalf("legacy output changed: got %q want %q", stdout+stderr, want)
			}
		})
	}
}

// TestNarrateRedactsSecrets proves that a canary cookie, token, or query string
// never reaches an event Detail.
func TestNarrateRedactsSecrets(t *testing.T) {
	canary := "sessionid=CANARYCOOKIEVALUE"
	var got []event.Event
	sink := event.Func(func(_ context.Context, e event.Event) { got = append(got, e) })
	ctx := event.WithSink(context.Background(), sink)

	narratef(ctx, kindWarn, event.ReasonResponseError, "https://x.com/a?token=CANARYQUERY", "",
		"[twitter] response: %s\n", "auth_token=CANARYTOKEN; "+canary)

	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	e := got[0]
	for _, secret := range []string{"CANARYCOOKIEVALUE", "CANARYQUERY", "CANARYTOKEN"} {
		if strings.Contains(e.Detail, secret) || strings.Contains(e.URL, secret) {
			t.Fatalf("canary %q leaked into event: %+v", secret, e)
		}
	}
	if !strings.Contains(e.URL, "https://x.com/a") {
		t.Fatalf("url = %q", e.URL)
	}
}
