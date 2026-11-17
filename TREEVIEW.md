# Treeview - File Reference

File-level reference for every Go source file in the module (152 files),
regenerated at commit `4da8a9a`. Test files are listed with their coverage.
Package-level design lives in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md);
the public facade is documented in [`docs/ENGINE.md`](docs/ENGINE.md).

## Root

| File | Purpose |
|---|---|
| `go.mod` / `go.sum` | Module `github.com/sk3y04/provenance-engine`, Go 1.27+ |
| `go.work` / `go.work.sum` | Local workspace linking the external-consumer module |
| `Makefile` | `test` (race), `vet`, `lint`, `fmt`, `build`, `engine-example`, `release-dry-run` |
| `README.md` | Quick start, four levels (grab/collect/archive/import), extractor table |
| `CHANGELOG` | Keep-a-Changelog history |
| `CONTRIBUTING.md` | Build/test conventions (hermetic fixture tests, no live services) |
| `LICENSE` | MIT |
| `.golangci.yml` | Lint config: errcheck, govet, ineffassign, staticcheck, unused, misspell, unconvert, gosec |
| `.github/workflows/` | CI (test/lint/build/external consumer), release, security, dependency review |

## `cmd/provenance/` — CLI entry point

| File | Purpose |
|---|---|
| `main.go` | Cobra command tree (grab, scan, collect, archive, vault, watch, history, encode, doctor, install, tui, completion); flag surface; signal context; auto-install pre-run; exit-code mapping |
| `engine_adapter.go` | CLI adapter over the public facade: `facadeGrabCompatible`, flag→`engine.Config` mapping, `runGrabViaFacade`, `cliEventSink`, `exitCode` |
| `engine_adapter_test.go` | Flag mapping, facade-compatibility decisions, event-sink rendering, exit-code regression |

## `engine/` — public facade

| File | Purpose |
|---|---|
| `doc.go` | Package documentation: stable importable API, boundary rules |
| `engine.go` | `Engine.New/Resolve/Download/Capabilities/Config`; validation, timeouts, panic recovery, workspace-scoped artifact discovery with SHA-256/MIME, event redaction at the boundary |
| `types.go` | Facade DTOs: `Config`, `ResolveRequest`, `DownloadRequest`, `FilterOptions`, `Source`/`Item`/`MediaAsset`/`TextContent`, `Artifact`, `Counts`, `Result`, defaults |
| `event.go` | Public `Event`/`EventKind`/`Stage`/`EventSink`, `NopSink`, `SinkFunc`; internal↔public event mapping |
| `errors.go` | Public `*Error` with stable `ErrorKind`s; `ErrorKindOf`/`IsErrorKind`; cancellation never re-categorized |
| `engine_test.go` | Public API behavior (hermetic) |
| `engine_internal_test.go` | Mapping, artifact scan, error categorization, sinks |
| `example_test.go` | Compile-time external usage examples |
| `capability_test.go` | Capability report shape |
| `no_terminal_test.go` | No terminal output with a sink (and nil sink); slow/panicking sink regression |
| `externaltest/consumer.go` | Standalone external module importing only the facade (`make engine-example`) |

## `internal/app/` — application coordination

| File | Purpose |
|---|---|
| `app.go` | `Download`/`Scan`/`ScanResolved`; event-driven item/summary emission; history recording for every transport |
| `app_test.go` | No stderr with a sink; legacy stderr without one; validation; dry-run not recorded |

## `internal/dispatcher/` — routing & batch dispatch

| File | Purpose |
|---|---|
| `dispatcher.go` | `Classify`/`Dispatch`/`Scan`/`ScanResolved`/`BatchDispatch`; site routing; URL archives; link caches; browser-fallback orchestration; `Options`/`Reporter`/`Counts` |
| `narrate.go` | Transport-neutral narration helper, redacted `emitEvent`, `depsFor` |
| `dispatcher_test.go` | Routing, archives, summary compatibility |
| `events_test.go` | Structured event emission (summary facts, response errors) |

## `internal/extractor/` — native & tool-backed extractors

| File | Purpose |
|---|---|
| `ytdlp.go` | yt-dlp wrapper: install, download/scan, progress protocol (`PROVENANCE_YTDLP_PROGRESS:`), archives, sidecar metadata, typed failures |
| `twitter.go` | X GraphQL client: user resolution, timeline pagination, single post (`TweetResultByRestId`), media selection, query-ID cache/refresh, guest bearer |
| `twitter_search.go` | Authenticated hashtag media search (`SearchTimeline` POST, `x:#tag` sources) |
| `twitter_source.go` | Twitter URL/source classification (profile, status, hashtag) |
| `reddit.go` | Reddit JSON API: subreddits/users, galleries, videos/embeds via yt-dlp, comments, OAuth support |
| `instagram.go` | Instagram REST v1: profiles, posts, reels, stories, highlights; host cascade; server-rendered page fallback; cookie domain filtering |
| `album.go` | Album sites: index resolution, signing API, CDN fetch with retry/backoff |
| `browser.go` | Headless Chrome (chromedp): playlist link scraping, pagination, media-URL sniffing fallback |
| `helpers.go` | Shared helpers: cookie loading/filtering, filename sanitization, error-preview truncation, file writes |
| `deps.go` | Per-engine `Deps` (HTTP client, query-ID cache, tool paths); shared concurrency-safe transport with private-network dial guard |
| `emit.go` | `narrate`/`narratef`/`narrateOut`, cancellable `sleepCtx`, capped `retryAfterDelay`, event-backed progress adapter |
| `events.go` | Event emission helpers and progress-reporter bridging for extractor paths |
| `backoff_test.go` | Retry-After cap table; cancel-mid-429/5xx across all fetchers; cap warning event; canary bodies absent from events/errors |
| `cancel_test.go` | Cancellable sleep, HTTP, and subprocess seams |
| `narrate_test.go` | Narration transport neutrality; secret canary redaction |
| `no_terminal_test.go` | Facade-supported extractors write nothing to the terminal with a sink |
| `events_test.go` | Extractor event emission contracts |
| `testdeps_test.go` | `testDeps` helper (engine-owned Deps for tests) |
| `ytdlp_test.go` / `ytdlp_fixtures_test.go` | Progress parsing, install behavior, yt-dlp output fixtures |
| `twitter_single_test.go` | Single-post resolution via fixture GraphQL server |
| `twitter_search_test.go` / `twitter_source_test.go` | Hashtag search pagination/media; source classification |
| `reddit_test.go` / `reddit_fixtures_test.go` | Reddit parsing and API fixtures |
| `instagram_test.go` | Instagram parsing, cookie filtering, host fallback fixtures |
| `album_test.go` | Album API/signing resolution fixtures |
| `resolve_test.go` / `scan_test.go` | Resolved-source mapping; scan preflight |

## `internal/downloader/` — resumable HTTP

| File | Purpose |
|---|---|
| `downloader.go` | Resumable downloads (`.part` + Range), 416 recovery via Content-Range, context-aware speed limit, SHA-256 during copy, SSRF guards (`NewDialer`, `SafeRedirect`, private-IP refusal), progress reporting |
| `utls_transport.go` | Chrome-fingerprint uTLS transport for CDNs that block Go's handshake |
| `sniff.go` | Magic-byte media sniffing (`SniffExt`) |
| `downloader_test.go` | Empty-part restart, Range resume, 416 recovery/preservation, skip path, URL/dial/redirect guards, Content-Range parsing |
| `limitreader_test.go` | Speed-limit cancellation and throttle preservation |
| `sniff_test.go` | Magic-byte extension detection |

## `internal/event/` — structured events

| File | Purpose |
|---|---|
| `event.go` | `Event`/`Kind`/`Stage`/`Counts`, `Sink` contract, `Nop`/`Func`, context-carried sinks, bounded panic-recovering `Notifier` |
| `reasons.go` | Stable machine-readable reason codes (lifecycle, decisions, diagnostics, tools) |
| `event_test.go` | Ordering, panic recovery, non-blocking drop, close idempotency, cancellation |

## `internal/engineerr/` — categorized errors

| File | Purpose |
|---|---|
| `engineerr.go` | Internal error kinds; `New`/`Newf`/`KindOf`/`Is`/`As`; cancellation never re-categorized |
| `engineerr_test.go` | `errors.Is`/`As` tables for every kind |

## `internal/render/` — CLI event rendering

| File | Purpose |
|---|---|
| `render.go` | The only terminal formatting of structured events (CLI sink adapter) |
| `render_test.go` | Exact output pins |

## `internal/redact/` — event safety

| File | Purpose |
|---|---|
| `redact.go` | `Detail` bounding/normalization/secret stripping; `URL` query/fragment removal; `Error` wrapper preserving Unwrap |
| `redact_test.go` | Canary cookie/token/query redaction |

## `internal/workspace/` — operation-scoped output

| File | Purpose |
|---|---|
| `workspace.go` | Records pre-existing files; reports only operation-created files; excludes bookkeeping dirs and `.part`; isolated subdirectory mode |
| `workspace_test.go` | Artifact scoping and concurrent-workspace isolation |

## `internal/capability/` — tool probing

| File | Purpose |
|---|---|
| `capability.go` | Side-effect-free probe: yt-dlp (incl. go-ytdlp versioned cache), ffmpeg, ffprobe, Chrome |
| `capability_test.go` | Probe shape and absence of side effects |

## `internal/doctor/` — prerequisite checks

| File | Purpose |
|---|---|
| `doctor.go` | Read-only checks (tools, cookies, cache dir, archive DB ping) + table report |
| `doctor_test.go` | Check outcomes and exit semantics |

## `internal/ratelimit/` — per-host throttling

| File | Purpose |
|---|---|
| `ratelimit.go` | Per-hostname `rate.Limiter` factory with platform-specific rates; idle eviction |
| `ratelimit_test.go` | Host classification and limiter behavior |

## `internal/worker/` — concurrency

| File | Purpose |
|---|---|
| `pool.go` | Bounded goroutine pool; 3-retry cancellable backoff; `Permanent` errors; success/failure hooks |
| `pool_test.go` | Retry, permanent-error, cancellation, hook semantics |

## `internal/session/` — resumable sessions

| File | Purpose |
|---|---|
| `store.go` | Session persistence: optimistic versioning, atomic writes, conflict reload that re-applies pending transitions, `Reporter` implementation, resume/reset |
| `store_test.go` | Lifecycle, invalid names, conflict-injection transition recovery |
| `fixtures_test.go` | Resume/retry-failed/interrupted/watch-skip fixture scenarios |

## `internal/watch/` — subscriptions

| File | Purpose |
|---|---|
| `store.go` | Watch persistence; run status/duration recording; `MarkInterruptedIfRunning` crash recovery |
| `store_test.go` | Lifecycle, run-status recording, interrupted-run recovery/idempotency |
| `fixtures_test.go` | Watch fixture scenarios |

## `internal/history/` — run history

| File | Purpose |
|---|---|
| `store.go` | Last-25-runs persistence shared by grab/watch/collect; atomic writes |
| `store_test.go` | Recording, trimming, load/save |

## `internal/collection/` — named source sync

| File | Purpose |
|---|---|
| `store.go` | Collection persistence (options, seen IDs, sync results); atomic writes |
| `sync.go` | Incremental sync: scan → dedup against seen IDs → download → record; reload-before-record TOCTOU guard; capture recording |
| `store_test.go` / `sync_test.go` | Persistence and sync/dedup behavior |

## `internal/manifest/` — scan & capture records

| File | Purpose |
|---|---|
| `manifest.go` | `Manifest`/`Item`/`Summary`, `FilterOptions`, size/CSV parsing, human display |
| `capture.go` | `CaptureManifest`/`CaptureItem`/`CaptureOptions` for post-download provenance |
| `provenance.go` | `.provenance/` filesystem I/O with SHA-256 verification |
| `manifest_test.go` | Filtering, parsing, display |

## `internal/resolve/` — shared result types

| File | Purpose |
|---|---|
| `resolve.go` | `Source`/`Item`/`MediaAsset`/`TextContent` + kinds; constructors (the `scan --json` contract) |
| `errors.go` | `Error` with `ErrorCategory`; `Is`/`Unwrap` semantics |
| `resolve_test.go` | JSON field-name pins, round trip, constructors, error-category matching |

## `internal/config/` — serializable configuration

| File | Purpose |
|---|---|
| `config.go` | `Config` shared by dispatcher/session/watch/history; `CaptureOptionsFromConfig` |
| `config_test.go` | Persisted JSON field names, omitempty shape, round trip, capture mapping |

## `internal/archive/` — vault domain

| File | Purpose |
|---|---|
| `types.go` | `ArchiveCollection`, `Source`, `Revision`, `Entity`, `Artifact`, `Document`, `Relation` |
| `ingest.go` | Capture-manifest ingest: path-traversal rejection, hash verification, dedupe, deterministic revision IDs, `WriteRevision` |
| `diff.go` | Field-level entity diffing between revisions |
| `ingest_test.go` / `diff_test.go` | Ingest invariants (traversal, hash mismatch, missing files, dedupe, round trip); diff behavior |

## `internal/blobstore/` — content-addressed storage

| File | Purpose |
|---|---|
| `blobstore.go` | SHA-256 blob store: concurrent-safe Put (unique temp, re-hash during copy, atomic rename), Get/Exists/Remove/List (hex-only names) |
| `blobstore_test.go` | Put/Get/dedupe/remove; concurrent same-content Put; List filtering |

## `internal/catalog/` — vault persistence

| File | Purpose |
|---|---|
| `interface.go` | `CatalogStore` interface; process-global `SetStore`/`Store` (CLI-local) |
| `catalog.go` | JSON filesystem adapter |
| `pg.go` | PostgreSQL adapter: transactional revision inserts, GIN tsvector search, FK-ordered cascade deletes |
| `migrations.go` | Embedded schema migration |
| `gc.go` | Garbage collection: unreferenced blobs, dry-run safety |
| `retention.go` | Newest-N-per-collection pruning |
| `gc_test.go` / `retention_test.go` | Referenced blobs survive, dry-run deletes nothing; retention scoping |

## `internal/citation/` — stable references

| File | Purpose |
|---|---|
| `citation.go` | `provenance://<collection>@<revision>#<entity>` references; Parse/Format (first-@/first-# split, empty-component rejection); Markdown/Plain/JSON formatters; `FromEntity`; `Short` |
| `citation_test.go` | Round trips incl. @/#-bearing entity URLs, parse errors, formatters, entity mapping |

## `internal/importers/` — knowledge importers

| File | Purpose |
|---|---|
| `common.go` | Revision construction/persistence, blob storage helper, UTF-8-safe truncation, transport-neutral narration |
| `git.go` | `ImportGit`: arg validation (ext::/scheme allowlist/dash refs), ctx-aware shallow clone + rev-parse, doc walk |
| `git_unix.go` | Process-group setup and group-kill on cancellation (Unix) |
| `git_windows.go` | Default kill + bounded WaitDelay (Windows) |
| `pdf.go` | `ImportPDF`: blob + per-page text extraction |
| `docs.go` | `ImportDocs`: bounded context-aware HTTP crawl, headings/text extraction, scheme validation |
| `openapi.go` | `ImportOpenAPI`: spec blob + endpoint documents |
| `web.go` | `ImportWeb`: headless-Chrome page capture, link discovery within scope, caller-context allocator |
| `importers_test.go` | Git-arg validation, mid-clone/mid-crawl cancellation, PDF/OpenAPI ingest with blob verification, docs scope/bounds, web preflight, truncation, narration contract |

## `internal/encode/` — AV1 transcode

| File | Purpose |
|---|---|
| `run.go` | Batch orchestration: input collection, per-device pools, output verification before success/`--remove-original`, result tallies |
| `encoder.go` | QSV/AMF/NVENC backends: quality/preset mapping, ffmpeg arg construction |
| `runner.go` | `CommandRunner` seam (`exec.CommandContext`) |
| `discover.go` | Codec probing, encoder selection, ffprobe fps detection |
| `devices.go` | Vendor-matched render-node auto-detection (`/sys/class/drm`) |
| `emit.go` | Transport-neutral encode narration (events or legacy stderr) |
| `run_test.go` | Fan-out, skipping, remove-original success/failure/phantom-output, dry-run |
| `encoder_test.go` / `discover_test.go` / `devices_test.go` | Arg construction, probing/selection, device pinning |
| `emit_test.go` | Event narration contract |
| `integration_test.go` | Real-ffmpeg integration (skipped when unavailable) |

## `internal/diagnose/` — failure hints

| File | Purpose |
|---|---|
| `diagnose.go` | Pattern-matches error strings into actionable recovery hints |
| `diagnose_test.go` | Hint matching |

## `internal/tui/` — terminal UI

| File | Purpose |
|---|---|
| `tui.go` | Bubble Tea program setup, model/view enum, root context |
| `update.go` | Message dispatcher and per-view key handling |
| `views.go` | View rendering (lists, forms, detail screens; reveal-in-finder is presentation-only) |
| `mainmenu.go` | Main menu actions incl. archive/import commands on `m.ctx` |
| `form.go` | New-grab form |
| `scanpick.go` | Scan preview & item picking |
| `sessions.go` | Sessions list/detail/resume |
| `watches.go` | Watches screen |
| `history.go` | History screen |
| `collections.go` | Collections list/detail/sync on `m.ctx` |
| `vault.go` | Vault collections/revisions/diff on `m.ctx` |
| `runner.go` | Live download runner; `teaSink` event consumption (no global stream mutation) |
| `config.go` | TUI config persistence (last-used form values) |
| `helpers.go` | Shared TUI helpers |
| `config_test.go` / `sessions_test.go` | Config persistence; session screen behavior |
| `sink_test.go` | No process-global stream mutation |

## `docs/`

| File | Contents |
|---|---|
| `CLI.md` | Full command/flag reference |
| `EXAMPLES.md` | Cookbook |
| `EXTRACTORS.md` | Per-extractor internals |
| `CONFIGURATION.md` | Config fields, flag mapping, env vars |
| `TROUBLESHOOTING.md` | Common issues |
| `COOKIES.md` | Authentication formats and per-site requirements |
| `TUI.md` | Views, keys, state model |
| `SESSIONS.md` | Session lifecycle and concurrency |
| `WATCHES.md` | Recurring subscriptions |
| `ARCHITECTURE.md` | Layers, data flow, design decisions, package reference |
| `ENGINE.md` | Public facade guide |
| `ENGINE_REFACTOR.md` | Phase 1 audit + implementation status |
| `MIGRATION.md` / `MIGRATION_BASELINE.md` | Module rename migration |
| `RELEASING.md` | Release process |
