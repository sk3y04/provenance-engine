package archive

import (
	"fmt"
	"os"
	"strings"

	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

// ResolveLocator opens the immutable bytes a page/passage locator points at.
// It is an offline operation: it reads the revision manifest and the blobstore
// only, never the original source path or the network. The locator's artifact
// must be part of the named revision and its page index must be non-negative.
func ResolveLocator(vaultRoot, revisionID string, loc Locator) (*os.File, error) {
	if strings.TrimSpace(vaultRoot) == "" {
		return nil, fmt.Errorf("vault root is required")
	}
	if !validRevisionID(revisionID) {
		return nil, fmt.Errorf("invalid revision id %q", revisionID)
	}
	if loc.PageIndex < 0 {
		return nil, fmt.Errorf("invalid page index %d: page indices are zero-based and non-negative", loc.PageIndex)
	}
	sha := strings.ToLower(strings.TrimSpace(loc.ArtifactSha256))
	if !validBlobHash(sha) {
		return nil, fmt.Errorf("locator artifact is not a sha256 digest")
	}
	rev, err := ReadRevision(vaultRoot, revisionID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, a := range rev.Artifacts {
		if strings.EqualFold(a.Sha256, sha) {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("locator artifact %s is not part of revision %s", sha, revisionID)
	}
	return blobstore.New(vaultRoot).Get(sha)
}
