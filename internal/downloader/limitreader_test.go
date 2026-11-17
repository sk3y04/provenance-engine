package downloader

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestLimitReaderHonorsCancellation proves a canceled download aborts during
// the speed-limit wait instead of sleeping it out.
func TestLimitReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := bytes.Repeat([]byte("x"), 4096)
	r := newLimitReader(ctx, bytes.NewReader(data), 100) // 100 B/s

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	buf := make([]byte, 64) // 64 bytes at 100 B/s would wait ~640ms
	start := time.Now()
	n, err := r.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != 64 {
		t.Fatalf("n = %d, want the bytes already read (64)", n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("speed-limit wait ignored cancellation: %v", elapsed)
	}
}

// TestLimitReaderStillThrottles proves the rate limiting itself is unchanged.
func TestLimitReaderStillThrottles(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 4096)
	r := newLimitReader(context.Background(), bytes.NewReader(data), 200) // 200 B/s
	buf := make([]byte, 32)                                               // ~160ms wait
	start := time.Now()
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 32 {
		t.Fatalf("n = %d, want 32", n)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("speed limit not applied: %v", elapsed)
	}
}
