// This module is compiled only through the repository-root go.work workspace,
// which provides github.com/sk3y04/provenance-engine. It intentionally carries
// no replace directive and no pinned require: a version-pinned require would
// force a network fetch of the not-yet-tagged module. Once v0.1.0 is tagged,
// real consumers require the tagged version directly with no workspace. Do not
// run `go mod tidy` here before the tag. See docs/RELEASING.md.
module github.com/sk3y04/provenance-engine/engine/externaltest

go 1.27.2