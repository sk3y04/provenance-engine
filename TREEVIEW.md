# Treeview - File Reference

File-level reference for every Go source file in the module (283 files),
regenerated for the `v0.10.0` release. Test files are listed with their coverage.
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
| `main.go` | Cobra command tree (grab, scan, collect, archive, vault, watch, history, encode, soulseek, doctor, install, tui, completion); flag surface; signal context; auto-install pre-run; exit-code mapping |
| `engine_adapter.go` | CLI adapter over the public facade: `facadeGrabCompatible`, flag→`engine.Config` mapping, `runGrabViaFacade`, `cliEventSink`, `exitCode` |
| `engine_adapter_test.go` | Flag mapping, facade-compatibility decisions, event-sink rendering, exit-code regression |
| `torrent.go` | `torrent inspect`/`fetch`/`status` and the durable `torrent queue` commands (add, list, start, pause, retry, run, seed, unseed, remove, priority, live); discovery profiles, drain/`--watch` summaries, live rendering |
| `torrent_test.go` | Flag validation, inspect/fetch wiring, queue entry points |
| `torrent_inspect_test.go` | Read-only inspection output and exit codes |
| `torrent_status_test.go` | Human/JSON status, unreadable records, exit code |
| `soulseek.go` | `soulseek` group root and `settings show`/`set`, `health`, `test`; profile loading, secret masking, four-state health rendering |
| `soulseek_search.go` | `search`, `download`, `jobs`, `ingest`, `watch`, `cancel`, `retry`, `position`, `clear-completed`, `recover`, `webhook-listen`; group selection, job rendering, reconciler loop |
| `soulseek_shares.go` | `shares list`/`preview`/`add`/`remove`/`status`/`rescan`/`files` and `uploads list`/`cancel`/`clear-completed`; preview rendering, paginated browse |
| `soulseek_test.go` | Settings masking/round-trip, enable-without-key refusal, health states, env-key enable |
| `soulseek_search_test.go` | Group selection, download/jobs/recover/cancel CLI wiring, missing-mount refusal |
| `soulseek_shares_test.go` | Shares/uploads CLI wiring, configured-root requirement |
| `main_test.go` | Command tree, auto-install skip table (including `torrent` and `soulseek`), exit-code mapping |


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
| `engine_concurrent_test.go` | Concurrent `Resolve`/`Download` alongside `Capabilities` re-probing (race regression) |


## `internal/atomicfile/` — atomic replacement

| File | Purpose |
|---|---|
| `atomicfile.go` | Write to a unique temp file in the destination directory, fsync and rename, so a reader never observes a half-written file |
| `atomicfile_test.go` | Replacement, cleanup on failure, concurrent writers |

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
| `archive_test.go` | URL archive and link-cache handling |
| `batch_test.go` | Batch dispatch, counts and cancellation |
| `classify_test.go` | Exact-domain routing table (lookalike hosts are not routed natively) |


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
| `hosts.go` | `HostMatchesDomain`: exact domain or true subdomain, ports and trailing dots normalised |
| `hosts_test.go` | Domain-match table, lookalike and port cases |
| `pagination_test.go` | Pagination bounds and stop conditions |
| `deps_concurrent_test.go` | Engine-owned tool report read while `Capabilities` re-probes (race regression) |


## `internal/downloader/` — resumable HTTP

| File | Purpose |
|---|---|
| `downloader.go` | Resumable downloads (`.part` + Range), 416 recovery via Content-Range, context-aware speed limit, SHA-256 during copy, SSRF guards (`NewDialer`, `SafeRedirect`, private-IP refusal), progress reporting |
| `utls_transport.go` | Chrome-fingerprint uTLS transport for CDNs that block Go's handshake |
| `sniff.go` | Magic-byte media sniffing (`SniffExt`) |
| `downloader_test.go` | Empty-part restart, Range resume, 416 recovery/preservation, skip path, URL/dial/redirect guards, Content-Range parsing |
| `limitreader_test.go` | Speed-limit cancellation and throttle preservation |
| `sniff_test.go` | Magic-byte extension detection |

## `internal/filelock/` — cross-process locks

| File | Purpose |
|---|---|
| `filelock.go` | Advisory exclusive lock on a stable `path + ".lock"` sidecar (data files are replaced by rename, so locking the inode cannot serialise writers); `Acquire` (blocking, cancellable) and `TryAcquire` with `ErrLocked` |
| `lock_unix.go` | `flock` backend |
| `lock_windows.go` | `LockFileEx` backend |
| `filelock_test.go` | Two-process exclusion, `TryAcquire` sentinel, cancellation, release on close |

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
| `evict_bench_test.go` | Eviction benchmarks at capacity (new-host path vs cached-host path) |


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
| `capture_test.go` | Capture-to-record path and duplicate handling |


## `internal/manifest/` — scan & capture records

| File | Purpose |
|---|---|
| `manifest.go` | `Manifest`/`Item`/`Summary`, `FilterOptions`, size/CSV parsing, human display |
| `capture.go` | `CaptureManifest`/`CaptureItem`/`CaptureOptions` for post-download provenance |
| `provenance.go` | `.provenance/` filesystem I/O with SHA-256 verification |
| `manifest_test.go` | Filtering, parsing, display |
| `capture_key_test.go` | Capture-key derivation and scan-for-existing-key lookup |


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
| `pg_test.go` | PostgreSQL-backed catalog tests (gated on `PROVENANCE_DATABASE_URL`) |


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

## `internal/transfer/bittorrent/` — selective BitTorrent adapter

| File | Purpose |
|---|---|
| `bittorrent.go` | Adapter over `github.com/anacrolix/torrent v1.62.0`: add one magnet or `.torrent`, list files, download only a selection, verify pieces, report `Update{Written,Total,Verified}` |
| `capture.go` | Selection → capture manifest through the existing `internal/manifest` path |
| `discovery.go` | Discovery profiles (`public`/`tracker`/`direct`) and BEP 27 private enforcement |
| `identity.go` | Typed identity holding full v1 SHA-1 and v2 SHA-256 separately; job-id and storage-key derivation |
| `inspect.go` | Read-only inspection: no client for a local file, a short-lived client for a magnet |
| `job.go` | Durable job record (`provenance/torrent-job+2`): immutable selection, payload/capture/seeding state, attestation |
| `path.go` | Hostile metainfo path rejection (traversal, absolute, separators, device names, collisions, symlinks) |
| `queueapi.go` | Entry points the local queue needs: network-free identity, reopening a known job, selection validation |
| `recheck.go` | Offline recheck: do the selected working files still hash to the recorded digests |
| `seed.go` | Workspace-scoped seed session: open, verify against a fresh completion state, serve, close |
| `status.go` | Record-only status documents for humans and `--json` |
| `workspace.go` | Isolated per-job workspace: payload, resume state, capture, lock |
| `bittorrent_integration_test.go` | End-to-end selective fetch against a local seeder |
| `child_test.go` | Re-exec child helper for process-level tests |
| `corruption_test.go` | Retained-piece corruption is detected and re-fetched |
| `crossclient_lab_test.go` | Cross-client lab against a containerised qBittorrent (gated) |
| `crossclient_test.go` | Cross-client interop helpers and assertions |
| `discovery_test.go` | Profile validation and enforcement |
| `durability_test.go` | Restart/resume, persisted metainfo, capture idempotency |
| `hybrid_interop_test.go` | Foreign-encoder hybrid fixture; documents the pad-alignment limitation |
| `inspect_test.go` | Inspection leaves no payload, database, record or lock |
| `isolated_workspace_test.go` | Per-job workspace isolation |
| `job_test.go` | Job state machine and record format |
| `limiter_test.go` | Rate budget accounting and observation |
| `magnet_v2_test.go` | btmh and hybrid magnet admission, missing-layer refusal |
| `main_test.go` | Package test setup and seeder fixtures |
| `ownership_process_test.go` | Staging ownership across processes |
| `ownership_test.go` | Lock acquire/release points and busy-root refusal |
| `path_test.go` | Hostile path rejection table |
| `peer_retry_test.go` | Peer retry interval and backoff |
| `priority_test.go` | File-priority overrides never change the selection |
| `private_tracker_test.go` | BEP 27 refusal under `public` |
| `progress_bench_test.go` | `selectedVerified` benchmarks, including a storage-backed 65,536-piece case |
| `progress_test.go` | Verified selected bytes, boundary-piece shares, padding exclusion |
| `recheck_test.go` | Offline recheck outcomes |
| `seedbudget_test.go` | Seed upload budget is shared |
| `seedctl_test.go` | Seed start/stop control and slot lifetime |
| `seeder_test.go` | Local seeder fixture |
| `seed_test.go` | Seed session verification before serving |
| `status_process_test.go` | Status across processes without inferring liveness |
| `status_test.go` | Recorded-state rendering, unreadable records |
| `v2_test.go` | v2 identity, `bt2-` job ids, layer persistence |

## `internal/torrentqueue/` — durable queue and runner

| File | Purpose |
|---|---|
| `store.go` | Entry records under `.provenance/torrent/queue/`, store/runner locks, intents, priorities, non-destructive removal |
| `runner.go` | The foreground runner: claim, admit, download pool, joins, fatal-error shutdown, workspace ownership |
| `seed.go` | Managed seeding as a second pool with its own slots and persisted intent |
| `adapter.go` | Worker backend mapping onto the isolated fetch path |
| `admit.go` | One admission path for the CLI and the TUI |
| `actions.go` | Intent transitions shared by the CLI and the TUI |
| `live.go` | Read-only Unix-socket snapshots (`queue live`), progress projection and rate baseline per attempt |
| `actions_test.go` | Intent transitions and their guards |
| `concurrency_test.go` | Worker and slot concurrency |
| `filesource_probe_test.go` | Direct-vs-queue isolation probe |
| `integration_test.go` | End-to-end queue runs against a local seeder |
| `limiter_test.go` | Shared budgets and peer limits |
| `live_barrier_test.go` | Close barrier and slot lifetime |
| `live_progress_test.go` | Rate rules, attempt isolation, old/new snapshot compatibility |
| `live_test.go` | Snapshot endpoint behaviour |
| `overlap_test.go` | Download and seed pools occupied in one snapshot |
| `peers_test.go` | Per-job peer caps as observed |
| `priority_test.go` | Priorities are overrides within one selection |
| `probe_seeder_test.go` | Seeder probe helper |
| `process_test.go` | Child-process and SIGKILL behaviour |
| `queue_test.go` | Entry lifecycle, limits and backoff |
| `remove_test.go` | Removal never deletes data |
| `seed_gaps_test.go` | Seed lifecycle regression gaps |
| `seedpool_test.go` | Seed pool fatal paths |
| `seed_queue_test.go` | Seed intent through the queue |
| `watch_test.go` | `--watch` session, drain semantics and fatal shutdown |

## `internal/slskd/` — Soulseek through an external slskd

| File | Purpose |
|---|---|
| `types.go` | Package contract and typed API surface: composite state flags (`", "`-separated), terminal-failure reasons, version parsing/comparison, path-segment validation, millisecond `searchTimeout` |
| `client.go` | HTTP client over `/api/v0`: `X-API-Key` only, URL-base joining without doubled prefixes, escaped path segments, bounded bodies, per-operation timeouts, secret redaction, API-error classification |
| `settings.go` | Connection profile: `0600` JSON store with atomic rename, `PROVENANCE_SLSKD_*` env overrides, sentinel-preserving updates, secret masking, validation, future-format refusal |
| `health.go` | Four-state health probe: version floor `0.26.0`, amber warnings for `0.26.1`/`0.26.2` (`#1891`) and the `0.26.2` `/options` `403` (`#1905`), risky-option and download-mount diagnostics |
| `search.go` | Single-flight search coordinator: serialised creates, throttle retry, 35-second wall-clock cap with a final re-fetch, adaptive polling, ranked `(peer, folder)` groups, deferred `DELETE` after settle |
| `jobstore.go` | Durable job records under `.provenance/slskd/jobs/`: unsafe-id refusal, duplicate detection, pending/active item projection, concurrent-write safety, `ErrJobNotFound` |
| `transfers.go` | Batch enqueue persisted before posting with pinned `externalId`/destination, hostile-path and locked-file refusal, dedupe, terminal-state classification, queue-position probe, local snapshot cache |
| `recovery.go` | Boot-time reconciliation against slskd and the filesystem: adopt files on disk, `INTERRUPTED` for a job a volatile restart forgot, no guessing while slskd is down |
| `ingest.go` | Completed download → capture through `internal/manifest`: path-aware locate (exact/suffix/tail/`_{ticks}`), distinct identities for same-basename files, size/hash verification, job-unique run manifests, scoped cleanup, optional vault ingestion |
| `shares.go` | Curated shares: preview with exclusions, hardlink-only materialisation, fail-fast `EXDEV`, `Lstat` audit, stale-link pruning on re-add, share rules store, rescan, paginated contents browse |
| `uploads.go` | Upload visibility: duplicate `Queued, Locally` collapse (`#1609`), summaries, list/cancel/clear-completed |
| `webhook.go` | Optional receiver: digest-compare shared secret, bounded delivery-id dedupe, stale-timestamp rejection, `413` on oversized bodies, per-event wakeups |
| `client_test.go` | API-key-only header, URL joining, error classification/redaction, TLS, timeouts, batch outcomes, bounded bodies |
| `types_test.go` | Millisecond timeout contract, state-flag parsing, terminal reasons, version floor, path validation |
| `settings_test.go` | Round-trip/permissions, masking, sentinel updates, env overrides, validation, future-format refusal |
| `health_test.go` | All four states, version floor, amber warnings, `/options` `403` handling, path diagnostics, redaction |
| `search_test.go` | Serialisation, request contract, throttle retry/failure, wall-clock stop, cancel, ranking, settle delete |
| `jobstore_test.go` | Record lifecycle, unsafe/duplicate ids, concurrent writes, pending statuses, job-id shape |
| `transfers_test.go` | Persist-before-post, pinned ids, path/locked refusal, dedupe, terminal classification, snapshot-cache locality |
| `recovery_test.go` | Reconciliation outcomes, adoption, `INTERRUPTED`, young-job grace, refusal while slskd is down |
| `ingest_test.go` | Capture happy path, `_{ticks}` rename, symlink/traversal refusal, size/empty/missing failures, cleanup gating, vault ingestion |
| `shares_test.go` | Preview exclusions/symlink counting, hardlinks + rescan, `EXDEV`, overlapping roots, planted-symlink audit, pruning, pagination |
| `uploads_test.go` | `Queued, Locally` dedupe, summaries, list/cancel |
| `webhook_test.go` | Secret refusal, unconfigured refusal, dedupe bounds, stale timestamps, event dispatch, malformed JSON |
| `e2e_test.go` | End-to-end search → enqueue → poll → ingest against a fake slskd |
| `review_fixes_test.go` | Regressions for the ten review findings: invalid-profile health, partial-batch settle, final-window responses, same-basename identities, partial-import refusal, share pruning, permission warning, oversized webhook body, position throttle/shape, same-second captures, scoped cleanup |

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
| `torrentqueue.go` | Read-only queue view: durable reads, live overlay, single-flight refresh with an owed refresh |
| `torrentadd.go` | Paused-add picker with a masked source field and bounded inspection |
| `torrentqueue_test.go` | View rendering, refresh single-flight, progress rendering |
| `torrentqueue_actions_test.go` | Start/pause/retry/seed from the view |
| `torrentqueue_async_test.go` | Async boundaries pinned through top-level `Update` |
| `torrentqueue_live_test.go` | The view against the real local snapshot endpoint |
| `torrentadd_test.go` | Picker generation, masking and cancellation |


## `internal/version/` — reported version

| File | Purpose |
|---|---|
| `version.go` | Single source of truth for the version stamped into the CLI, capture manifests and archive revisions; overridable with `-ldflags -X` |

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
