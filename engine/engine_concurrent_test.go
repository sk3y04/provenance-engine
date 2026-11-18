package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/engine"
)

// TestCapabilitiesConcurrentWithResolve gates the documented concurrent-use
// contract (docs/ENGINE.md "Lifecycle and concurrency"): one Engine is reused
// across goroutines, and a health check may run while an operation is in
// flight. Capabilities refreshes the engine-owned tool report while Resolve
// reads it on every call; the two must not race.
//
// The URL is a parseable-but-dead host, so the operation exercises the real
// tool-report read path without contacting a live service. Run with -race.
func TestCapabilitiesConcurrentWithResolve(t *testing.T) {
	eng, err := engine.New(engine.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Operations read the tool report (yt-dlp availability) on every Resolve.
	var ops sync.WaitGroup
	for i := 0; i < 2; i++ {
		ops.Add(1)
		go func() {
			defer ops.Done()
			for j := 0; j < 3; j++ {
				_, _ = eng.Resolve(ctx, engine.ResolveRequest{URL: "https://example.invalid/x"}, nil)
			}
		}()
	}

	// Continuous health probes, as a hosted worker would run alongside jobs.
	stop := make(chan struct{})
	var probes sync.WaitGroup
	for i := 0; i < 4; i++ {
		probes.Add(1)
		go func() {
			defer probes.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				eng.Capabilities(ctx)
			}
		}()
	}

	ops.Wait()
	close(stop)
	probes.Wait()
}
