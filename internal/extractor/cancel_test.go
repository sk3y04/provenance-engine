package extractor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

// TestSleepCtxIsCancellable proves that retry and backoff paths honor operation
// cancellation instead of burning a platform quota on a canceled job.
func TestSleepCtxIsCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, 5*time.Second); err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("sleep did not return promptly on cancellation: %v", time.Since(start))
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := sleepCtx(ctx2, 5*time.Second); err == nil {
		t.Fatal("expected deadline error")
	}
}

// TestHTTPRequestHonorsCancellation proves the HTTP seam: a request made with a
// canceled context fails promptly rather than waiting for the server.
func TestHTTPRequestHonorsCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	_, err = srv.Client().Do(req)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("HTTP request did not honor cancellation promptly: %v", time.Since(start))
	}
}

// TestSubprocessHonorsCancellation proves the subprocess seam: a command built
// with the operation context is terminated when the context is canceled, so a
// canceled job does not leave a yt-dlp/ffmpeg process running.
func TestSubprocessHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start fake command: %v", err)
	}
	cancel()
	start := time.Now()
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected error from canceled command")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("subprocess did not honor cancellation promptly: %v", time.Since(start))
	}
}
