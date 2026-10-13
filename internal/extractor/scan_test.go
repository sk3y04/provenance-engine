package extractor

import (
	"context"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/capability"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
)

// TestEnsureYtdlpForScanHasNoSideEffectsWhenAutoInstallOff is the gate for a
// hosted Resolve: a missing tool must be a typed error, never a filesystem
// mutation, unless AutoInstall was explicitly enabled.
func TestEnsureYtdlpForScanHasNoSideEffectsWhenAutoInstallOff(t *testing.T) {
	d := &Deps{QueryIDs: &QueryIDCache{}} // Tools empty, AutoInstall false
	err := ensureYtdlpForScan(context.Background(), d, "https://example.invalid/x")
	if engineerr.KindOf(err) != engineerr.ExternalTool {
		t.Fatalf("KindOf = %q, want external_tool (%v)", engineerr.KindOf(err), err)
	}

	available := &Deps{
		QueryIDs: &QueryIDCache{},
		Tools:    capability.Set{{Name: capability.ToolYtdlp, Required: true, Available: true, Path: "/fake/yt-dlp"}},
	}
	if err := ensureYtdlpForScan(context.Background(), available, "https://example.invalid/x"); err != nil {
		t.Fatalf("available tool should not error, got %v", err)
	}
}
