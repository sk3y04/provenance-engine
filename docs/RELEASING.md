# Releasing `provenance-engine`

This document describes how to prepare, verify, and publish a tagged release of
the public engine, and records the licensing questions that require owner or
legal review before the first tag.

## Module and artifacts

| Item | Value |
|------|-------|
| Go module | `github.com/sk3y04/provenance-engine` |
| Public facade | `github.com/sk3y04/provenance-engine/engine` |
| CLI binary / command | `provenance` (unchanged across the repository rename) |
| License | MIT (`LICENSE`) |
| Go version | 1.27 (see `go.mod`) |

Published release assets are the `provenance` CLI, named
`provenance_<version>_<os>_<arch>[.exe]`, plus `checksums.txt`. The Go module
itself is consumed by import path at a tagged version; it has no build step.

## Quality gate

Run from a clean checkout before tagging:

```bash
gofmt -l .                 # must print nothing
go vet ./...
golangci-lint run ./...
go test -race ./...
go build -o provenance ./cmd/provenance
make engine-example        # external-consumer module compiles
make release-dry-run       # cross-compiles + checksums into dist/
```

`make release-dry-run` mirrors `.github/workflows/release.yml`. Both build the
`provenance` CLI for linux/darwin/windows on amd64/arm64 with `CGO_ENABLED=0`
and `-trimpath`, then write `dist/checksums.txt`.

## Exported API surface

Release review covers every exported identifier in `engine/`. The current
surface is intentional and is limited to:

- `Engine`, `New`, `Config`, and the `Resolve`/`Download` methods;
- request/selection types `ResolveRequest`, `DownloadRequest`, `FilterOptions`;
- result types `Source`, `Item`, `MediaAsset`, `TextContent`, `Artifact`,
  `Counts`, `Result`, and their kind string types;
- event types `Event`, `EventKind`, `Stage`, `EventSink`, `NopSink`,
  `SinkFunc`;
- error types `Error`, `ErrorKind`, `ErrorKindOf`, `IsErrorKind`;
- the documented default constants.

`internal/*` packages are not importable outside the module. No SQL, HTTP,
queue, object-storage, billing, or other private-product concern appears in the
public signatures. Audit with:

```bash
go doc github.com/sk3y04/provenance-engine/engine
go list -f '{{.ImportPath}}' ./engine/...
```

## External-consumer verification

`engine/externaltest` is a separate Go module that imports only the facade using
exported identifiers. It is compiled by `make engine-example` and by the
`External consumer` CI job.

Because no tag exists yet, the repository root carries a `go.work` workspace
that provides the parent module to `engine/externaltest`. This avoids a `replace`
directive in `engine/externaltest/go.mod`; the trade-off is that the module has
no pinned `require` and must not be tidied before the first tag. After `v0.1.0`
is published, a real consumer depends on the tag directly:

```bash
go get github.com/sk3y04/provenance-engine@v0.1.0
```

At that point the workspace can be deleted and the external test can pin the
tag (then `go mod tidy` is safe again).

## Versioning

- Follow [Semantic Versioning](https://semver.org/). The module path has no
  `/vN` suffix, so breaking exported-API changes require a major tag and, when
  major > 1, a module-path change.
- The CHANGELOG already consumed `0.1.0` through `0.6.1` and the CLI reports
  `0.7.0`, so the first engine release is **`v0.7.0`**, continuing the existing
  sequence rather than restarting at `v0.1.0`.
- The CLI's `--version` string is hardcoded (`0.8.0` in `cmd/provenance/main.go`)
  and is kept in step with each release tag; alternatively inject the version
  via `-ldflags`.
- The rename from `github.com/sk3y04/provenance` is a breaking import-path
  change; it is recorded in `CHANGELOG` and `docs/MIGRATION.md` and must be
  called out in the release notes.
- The module was also relicensed from GPL-3.0 to MIT in the same release; the
  release notes must state this.

## Licensing and third-party notices

- The repository is licensed **MIT** (`LICENSE`). It was relicensed from
  GPL-3.0 for the `v0.7.0` release so the private product can embed the engine
  without copyleft obligations. Only the copyright holder can relicense; the
  change was made by the repository owner.
- There is **no `NOTICE` or third-party-license file**. Go dependencies are
  declared in `go.mod`/`go.sum`; a transitive license audit has not been
  performed.
- Release artifacts contain only the statically linked `provenance` binary.
  `yt-dlp`, `ffmpeg`/`ffprobe`, and Chrome/Chromium are **not** bundled; the
  engine invokes them if present and can auto-install `yt-dlp` at runtime.

### Questions requiring owner/legal review

These are recorded as open questions; no legal conclusion is drawn here.

1. **Bundled vs. runtime-fetched tools.** If a future release bundles `yt-dlp`
   or `ffmpeg` rather than fetching them at runtime, their licenses (for
   example, ffmpeg builds may be LGPL or GPL depending on configuration) must be
   reviewed and a third-party notice added.
2. **Transitive Go dependency licenses.** A tool such as `go-licenses` should be
   run and its output reviewed; the MIT license requires retaining third-party
   copyright notices in distributions.

## Tagging (manual, after review)

The current release is `v0.8.0` (see Versioning above). After the human review
gate:

```bash
git tag -a v0.8.0 -m "Upgrade to Go 1.27 and refresh dependencies"
git push origin master --tags
```

The `Release` workflow builds `provenance` for every published platform and
attaches the binaries and `checksums.txt` to the GitHub release. A manual
`workflow_dispatch` run builds artifacts without publishing.