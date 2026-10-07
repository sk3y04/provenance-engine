package extractor

import (
	"context"
	"errors"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
)

func TestClassifyYtdlpError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		stderr string
		want   engineerr.Kind
	}{
		{"canceled", context.Canceled, "", engineerr.Canceled},
		{"deadline", context.DeadlineExceeded, "", engineerr.Canceled},
		{"unsupported", errors.New("yt-dlp: exit status 1"), "ERROR: Unsupported URL: https://x", engineerr.UnsupportedSource},
		{"auth", errors.New("yt-dlp: exit status 1"), "ERROR: HTTP Error 403: Forbidden", engineerr.AuthRequired},
		{"rate", errors.New("yt-dlp: exit status 1"), "ERROR: HTTP Error 429: Too Many Requests", engineerr.RateLimited},
		{"permanent", errors.New("yt-dlp: exit status 1"), "ERROR: HTTP Error 404: Not Found", engineerr.Permanent},
		{"other", errors.New("yt-dlp: exit status 1"), "some postprocessing failure", engineerr.ExternalTool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyYtdlpError("https://example.com/v", tc.err, tc.stderr)
			if got := engineerr.KindOf(err); got != tc.want {
				t.Fatalf("KindOf = %q, want %q (%v)", got, tc.want, err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("errors.Is does not reach the cause")
			}
		})
	}
}

func TestEventProgressEmitsStructuredEvents(t *testing.T) {
	var got []event.Event
	ep := newEventProgress(event.Func(func(_ context.Context, e event.Event) {
		got = append(got, e)
	}))

	ep.OnStart("u", "file.mp4", 100)
	ep.OnProgress("u", 40, 100)
	ep.OnDone("u", nil)

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Kind != event.KindProgress || got[0].Reason != "start" || got[0].Total != 100 || got[0].ItemRef != "file.mp4" {
		t.Fatalf("start event = %+v", got[0])
	}
	if got[1].Kind != event.KindProgress || got[1].Written != 40 || got[1].Total != 100 {
		t.Fatalf("progress event = %+v", got[1])
	}
	if got[2].Kind != event.KindItemDone || got[2].Err != nil {
		t.Fatalf("done event = %+v", got[2])
	}
	if got[0].Stage != event.StageDownloading {
		t.Fatalf("stage = %q, want downloading", got[0].Stage)
	}
}
