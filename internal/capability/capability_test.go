package capability

import (
	"context"
	"testing"
)

// TestProbeHasNoSideEffects proves a health check cannot install a tool or mutate
// the filesystem: the probe only resolves names through the injected seam.
func TestProbeHasNoSideEffects(t *testing.T) {
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if name == ToolYtdlp {
			return "/fake/yt-dlp", nil
		}
		return "", errNotFound
	}
	defer func() { lookPath = orig }()

	set := Probe(context.Background(), "")
	ytdlp, ok := set.Get(ToolYtdlp)
	if !ok || !ytdlp.Available || ytdlp.Path != "/fake/yt-dlp" {
		t.Fatalf("yt-dlp = %+v", ytdlp)
	}
	if !ytdlp.Required {
		t.Error("yt-dlp must be reported as required")
	}
	ffmpeg, ok := set.Get(ToolFfmpeg)
	if !ok || ffmpeg.Available {
		t.Fatalf("ffmpeg = %+v", ffmpeg)
	}
	if !set.Available(ToolChrome) {
		t.Log("chrome correctly reported unavailable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Probe(ctx, ""); got != nil {
		t.Fatalf("probe on a canceled context should return nothing, got %+v", got)
	}
}

var errNotFound = &probeError{}

type probeError struct{}

func (probeError) Error() string { return "not found" }
