package extractor

import (
	"sync"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/capability"
)

// TestDepsProbeConcurrentWithToolReads is the tight regression gate for the
// Capabilities-vs-operation race: Probe replaces the tool report while
// operation paths read it. Readers must use the synchronized accessors, the
// same ones runYtdlpInternal and ensureYtdlpForScan use. Run with -race.
func TestDepsProbeConcurrentWithToolReads(t *testing.T) {
	d := NewDeps()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = d.ToolAvailable(capability.ToolYtdlp)
				_ = d.FfmpegLocation()
			}
		}()
	}

	for i := 0; i < 200; i++ {
		d.Probe("")
	}
	close(stop)
	readers.Wait()
}
