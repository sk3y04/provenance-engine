package extractor

import (
	"net/http"

	"github.com/sk3y04/provenance-engine/internal/redact"
)

// testDeps builds an engine-owned Deps for tests so no test depends on
// process-global mutable state.
func testDeps(client *http.Client) *Deps {
	return &Deps{HTTPClient: client, QueryIDs: &QueryIDCache{}}
}

var _ = redact.Detail
