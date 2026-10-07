# Migration Baseline

This document records the state of the public repository at the start of
Track A (public engine extraction), as required by Phase 0. It is a factual
snapshot, not a plan. The refactor plan is `docs/ENGINE_REFACTOR.md` (Phase 1).

## Repository state

| Item | Observed |
|------|----------|
| Working copy | `/home/anima/projects/provenance-workspace/provenance-engine` |
| Git repository | Initialized for this phase (`git init`); no prior history on disk |
| Branch | `master` (matches the branch targeted by CI) |
| Baseline commit | `9a50a0f` — `chore: import public provenance baseline before phase 0` |
| Working tree at baseline | Clean; binary and `downloads/` excluded by `.gitignore` |

### How this working copy was assembled

The Phase 0 checkout was staged from the canonical working tree at
`/home/anima/projects/provenance`, whose tracked engine source is byte-identical
to this copy. The following tracked repository files were missing from the
staging copy and were restored from that canonical checkout before the baseline
commit, because Phase 0 must inventory CI and the canonical lint command:

- `.github/workflows/ci.yml`
- `.github/workflows/dependency-review.yml`
- `.github/workflows/security.yml`
- `.golangci.yml`
- `.gitignore`

Two items present in the canonical checkout are intentionally **not** part of
this repository and remain at the workspace root:

- `implementation-phases.md` (the migration execution plan).
- `provenance-agent-rules/` (the private-repository rules pack, to be installed
  in Phase 7).

Local-only state (`.idea/`, `downloads/`, the built `provenance` binary) is
git-ignored and was neither copied nor committed.

## Module path and Go version

| Item | Value |
|------|-------|
| Module path (current) | `github.com/sk3y04/provenance` |
| Module path (target, Phase 2) | `github.com/sk3y04/provenance-engine` |
| Go directive | `go 1.26.0` |
| Toolchain observed | `go1.26.8-X:nodwarf5 linux/amd64` |
| Binary / command name | `provenance` (must remain unchanged) |
| License | GPL-3.0 |
| Tracked root files | `AGENTS.md`, `CHANGELOG`, `CONTRIBUTING.md`, `LICENSE`, `Makefile`, `README.md`, `TREEVIEW.md`, `go.mod`, `go.sum`, `.gitignore`, `.golangci.yml`, `.github/` |

## Package inventory

22 Go packages (`go list ./...`), 93 Go files, 29 test files.

| Package | Role | Test files |
|---------|------|-----------|
| `cmd/provenance` | Cobra CLI entry point | 0 |
| `internal/app` | Application coordination (`Download`, `Scan`, `ScanResolved`) | 0 |
| `internal/archive` | Immutable vault domain model, ingest, diff | 0 |
| `internal/blobstore` | SHA-256 content-addressed storage | 1 |
| `internal/catalog` | Archive persistence (JSON + PostgreSQL), GC, retention | 0 |
| `internal/citation` | Stable `provenance://` citations | 0 |
| `internal/collection` | Named collection sync | 2 |
| `internal/config` | Serializable download config | 0 |
| `internal/diagnose` | Error-string → hint matching | 1 |
| `internal/dispatcher` | URL classification, routing, batch, browser fallback | 1 |
| `internal/downloader` | Resumable HTTP client, uTLS transport | 1 |
| `internal/encode` | Hardware-accelerated AV1 transcode via ffmpeg | 4 |
| `internal/extractor` | Instagram, X, Reddit, yt-dlp, browser, helpers | 9 |
| `internal/history` | Download history persistence | 1 |
| `internal/importers` | PDF, Git, docs, OpenAPI, web page importers | 0 |
| `internal/manifest` | Scan/capture result types and filtering | 1 |
| `internal/ratelimit` | Per-host rate limiter factory | 1 |
| `internal/resolve` | Normalized result types and error categories | 0 |
| `internal/session` | Named resumable sessions (optimistic concurrency) | 2 |
| `internal/tui` | Bubble Tea terminal UI | 2 |
| `internal/watch` | Recurring watch subscriptions | 2 |
| `internal/worker` | Bounded goroutine pool with retry/permanent errors | 1 |

## Command inventory (`cmd/provenance/main.go`)

Root binary: `provenance`. Registered commands:

- `grab [URL...]`
- `scan [URL...]` (including `--json` emitting `resolve.Source`)
- `install`
- `status SESSION`
- `resume SESSION`
- `retry-failed SESSION`
- `sessions` → `list`, `export SESSION FILE`, `clean SESSION`, `failed SESSION [FILE]`
- `watch` → `add NAME URL`, `list`, `remove NAME`, `run [NAME]`
- `collect` → `add NAME URL`, `list`, `show NAME`, `remove NAME`, `sync [NAME]`
- `manifest` → `show PATH`, `verify DIR`
- `archive` → `url URL`, `collection NAME`, `session NAME`, `import DIR`, `import-pdf PATH`, `import-git URL`, `import-docs URL`, `import-openapi PATH`, `import-web URL`
- `vault` → `init`, `show ID`, `cite REVISION_ID`, `diff ID_A ID_B`, `gc`, `retention prune`, `backup`
- `search QUERY`
- `tui`
- `encode [flags]`
- `completion [bash|zsh|fish|powershell]`

Global persistent flag: `--chrome-path`.

## CI / release workflow inventory

Only GitHub Actions workflows exist; there is no release automation,
GoReleaser config, Dockerfile, or compose file.

| Workflow | Trigger | Does |
|----------|---------|------|
| `.github/workflows/ci.yml` | PR/push/merge-group to `master`, manual | `go mod download` + `go mod tidy` diff check, `make vet`, install ffmpeg, `make test` (`-race`); separate `lint` job (golangci-lint v2.13.2); `build` matrix (`ubuntu`, `macos`, `windows`) with `CGO_ENABLED=0` producing `provenance` |
| `.github/workflows/security.yml` | push to `master`, weekly cron, manual | Snyk Open Source scan against `go.mod`, SARIF upload; uses `secrets.SNYK_TOKEN` and `vars.SNYK_ORG` |
| `.github/workflows/dependency-review.yml` | PR to `master` | `actions/dependency-review-action`, fails on high runtime vulnerabilities |

Branch targeted by CI is `master`, not `main`.

## Baseline check results

Run from the repository root without modifying behavior.

| Command | Result |
|---------|--------|
| `go version` | `go1.26.8-X:nodwarf5 linux/amd64` |
| `go list ./...` | 22 packages listed |
| `go vet ./...` | Exit 0, no output |
| `gofmt -l .` | **`internal/extractor/album_test.go` is unformatted** (pre-existing) |
| `go test -race ./...` | Exit 0, all packages pass (no live-service tests) |
| `go build -o /tmp/opencode/provenance-baseline ./cmd/provenance` | Exit 0, 27,767,890-byte binary |
| `golangci-lint run ./...` | Exit 0, `0 issues` (v2.13.2) |
| `go mod tidy -diff` | Exit 0, no changes (module files clean) |

There are **no failing tests**. The only baseline defect is the unformatted
test file noted above. It was deliberately not reformatted in Phase 0 because
Phase 0 forbids runtime/Go source changes.

### Environment prerequisites

- Go 1.26+.
- `golangci-lint` v2.x (v2.13.2 present locally).
- C compiler / race support for `go test -race`.
- `ffmpeg` present (`/usr/bin/ffmpeg`) and `yt-dlp` present
  (`/usr/bin/yt-dlp`); yt-dlp auto-installs on first use in a clean
  environment.
- Chrome/Chromium present (`/usr/bin/google-chrome`) — only needed for
  browser-extractor and web-importer paths, not unit tests.
- Optional PostgreSQL (`PROVENANCE_DATABASE_URL`) for `vault`, `search`, and
  the PostgreSQL `catalog` adapter.
- CI Snyk job requires `SNYK_TOKEN` secret and `SNYK_ORG` variable.

## Behavior that must remain compatible

- Binary and end-user command name `provenance` (both CLI and `go install`).
- Command names, subcommand tree, and documented flags in `docs/CLI.md`.
- Exit codes: `0` success, `1` error (message on stderr, optional diagnostic hint).
- Signal handling: `SIGINT`/`SIGTERM` cancel the root context; running yt-dlp
  subprocesses are killed; on-disk sessions survive.
- Auto-install behavior and its `PersistentPreRunE` skip list.
- Persistence formats and default paths (JSON, not a database).
- `provenance scan --json` `resolve.Source` shape and `--record` capture
  manifests under `<outputDir>/.provenance/`.
- Vault layout, content-addressed blob paths, and `provenance://` citation URI.
- yt-dlp progress protocol (`PROVENANCE_YTDLP_PROGRESS:`-prefixed JSON lines).
- Cookie/credential behavior (Netscape `cookies.txt`, browser cookie import,
  Reddit OAuth2, Twitter guest bearer + query-ID refresh).
- Output layout presets, quality selectors, and per-host rate limits.

## Known secrets and configuration locations

No secret values are recorded here; only locations.

| Location | Content |
|----------|---------|
| `$PROVENANCE_DATABASE_URL` | PostgreSQL connection string for vault/catalog/search |
| `$TWITTER_BEARER_TOKEN` | Optional X/Twitter bearer override |
| `$TWITTER_QUERY_USER_BY_SCREEN_NAME`, `$TWITTER_QUERY_USER_TWEETS`, `$TWITTER_QUERY_SEARCH_TIMELINE` | Optional GraphQL query-ID overrides |
| `$REDDIT_OAUTH_TOKEN`, `$REDDIT_CLIENT_ID`, `$REDDIT_CLIENT_SECRET` | Reddit OAuth2 credentials |
| `$INSTAGRAM_APP_ID` | Instagram API app identifier |
| `$CHROME_PATH` | Chrome/Chromium executable path |
| `$PROVENANCE_SESSION_DIR`, `$PROVENANCE_WATCH_FILE`, `$PROVENANCE_HISTORY_FILE`, `$PROVENANCE_COLLECTION_FILE` | Persistence path overrides |
| `$XDG_CONFIG_HOME` / `~/.config/provenance/tui-config.json` | TUI config persistence |
| `cookies.txt` (user-supplied, referenced by `--cookies`) | Platform session cookies |
| `~/.cache/provenance/` | Default sessions/watch/history/collections storage |
| GitHub `secrets.SNYK_TOKEN`, `vars.SNYK_ORG` | CI-only credentials/variables |

No `.env` file, credential file, or committed secret was found in the
repository. `.gitignore` excludes the build binary, `downloads/`,
`_provenance_cache/`, and IDE files.

## Phase 0 acceptance status

- Existing build/test status recorded honestly — yes (above).
- Root `AGENTS.md` describes the public repository boundary — yes.
- No runtime Go source changed — yes; the only tracked changes are agent and
  documentation files.
- `git diff` contains only agent/documentation changes — verifiable now that
  the repository is under version control; see `git status` / `git diff`
  against baseline commit `9a50a0f`.