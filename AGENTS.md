# AGENTS.md — Public `provenance-engine` repository

## Repository role

This repository is the **public engine and CLI/TUI** repository.
It is currently named `provenance` and will be renamed to `provenance-engine`
(module path `github.com/sk3y04/provenance-engine`). A separate **private**
repository will later take the name `provenance` and own the SaaS product.

The end-user command and built binary remain named **`provenance`** across the
rename.

Dependency direction is strictly one way:

```text
private provenance -> public provenance-engine
```

The private product consumes a tagged release of this module. This repository
must never import, copy, or otherwise depend on private product code.

## Owned code

- Extractors: Instagram, X/Twitter, Reddit, yt-dlp, and headless-Chrome browser fallback.
- Resolver, downloader, media processing, ffmpeg/yt-dlp/chromedp integrations.
- Archive, vault, collection, catalog, citation, and importer primitives.
- Config, session, watch, history, manifest, resolve, worker, ratelimit, diagnose packages.
- Public CLI (`cmd/provenance`) and TUI as presentation adapters over the engine.
- Engine fixtures, tests, documentation, and tagged releases.

## Mandatory boundaries

- CLI and TUI are adapters over the same reusable execution paths a future private worker will call.
- Reusable execution packages must not print to stdout/stderr, call `os.Exit`/`log.Fatal`, panic for control flow, or require a terminal.
- Every operation accepts `context.Context`; cancellation must reach HTTP requests and subprocesses.
- User-facing progress is expressed as structured, presentation-neutral events; CLI/TUI render them.
- Public/exported types must not expose SQL, HTTP framework, River, S3, billing, or other private product concerns.
- Keep implementation under `internal/` where possible and expose a deliberately small facade.
- Preserve CLI compatibility (command names, flags, exit codes, persistence formats) unless a task explicitly approves a breaking release.
- Tag semantic releases and document breaking changes and migration steps.

## Forbidden in this repository

- Nuxt / Vue frontend, Chi API server, River job queue, SCS sessions.
- PostgreSQL **product** schema, product authentication, accounts, quotas, billing, or private SaaS code.
- Imports from the private `github.com/sk3y04/provenance` module.
- Copying private code or secrets into this repository, including fixtures.
- Global mutable stores/config, terminal output in reusable engine paths, or shell invocation with raw untrusted input.

## Required reading

1. This file.
2. `docs/ARCHITECTURE.md`.
3. `docs/MIGRATION_BASELINE.md` and, once written, `docs/ENGINE_REFACTOR.md`.
4. Domain docs relevant to the change (`docs/CLI.md`, `docs/EXTRACTORS.md`, `docs/CONFIGURATION.md`, `docs/TUI.md`, `TREEVIEW.md`).

## Canonical commands

```bash
make vet      # go vet ./...
make lint     # golangci-lint run ./...
make test     # go test -race ./...
make build    # go build -o provenance ./cmd/provenance/
gofmt -l .    # formatting check
```

CI additionally runs `go mod tidy` + diff check, installs ffmpeg, builds with
`CGO_ENABLED=0` on Linux/macOS/Windows, and runs Snyk/dependency review.

## Environment prerequisites

- Go 1.26+ (see `go.mod`).
- `golangci-lint` v2.x for `make lint`.
- `cc`/race support for `make test` (`-race`).
- `ffmpeg` and `yt-dlp` for full download/integration behavior (yt-dlp auto-installs on first use; tests must not depend on live services).
- Chrome/Chromium only for browser-extractor paths; not required for unit tests.
- Optional PostgreSQL (`PROVENANCE_DATABASE_URL`) for the vault/catalog/search commands.

## Approval required

Do not, without explicit approval:

- Change the repository visibility, module path, or license.
- Add a framework, ORM, queue, database driver, state manager, or new direct dependency.
- Remove public CLI commands, flags, or change documented exit codes.
- Weaken URL validation, subprocess isolation, or credential handling.
- Enable arbitrary URLs, yt-dlp flags, output templates, filesystem paths, or shell arguments from untrusted input.
- Perform broad dependency upgrades or unrelated refactors.

## Definition of done

- Acceptance criteria for the requested phase pass.
- `make vet`, `make lint`, `make test`, and `make build` pass, or failures are reported honestly.
- `gofmt` reports no unformatted files for changed code.
- Generated or documented artifacts are current.
- No private product code, secrets, or credentials were introduced.
- Changed files, commands run, results, and remaining risks are reported.