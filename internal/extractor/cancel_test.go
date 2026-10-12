package extractor

import (
	"context"
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
