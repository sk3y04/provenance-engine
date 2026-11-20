// Package version is the single source of truth for the provenance version
// stamped into the CLI, capture manifests, and archive revisions.
package version

// Version is the release version. Release builds may override it:
//
//	go build -ldflags "-X github.com/sk3y04/provenance-engine/internal/version.Version=1.2.3"
var Version = "0.10.1"
