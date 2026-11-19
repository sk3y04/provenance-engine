# ADR: Soulseek (`slskd`) integration

- **Status:** Accepted (Milestone 0)
- **Branch:** `feat/slskd-integration`
- **Starting commit:** `61f1cb3e94a51c208af036558bddd4bc416deee0` (`master`, "docs(release): prepare v0.9.0")
- **Date:** 2026-11-20
- **Upstream dependency:** `slskd/slskd` `>= 0.26.0`, spoke against `0.26.0` and `0.26.2`
- **Spike evidence:** `docs/findings-slskd-0.26-contract.md`
- **Locked schemas:** `internal/slskd/testdata/openapi-0.26.0.json`, `internal/slskd/testdata/openapi-0.26.2.json`

## 1. Why this ADR exists

The execution spec (`hermes_autonomous_execution_spec_provenance_engine_slskd_integration.md`) was
written against an assumed deployment shape: a Go module with an HTTP server, router/handlers, a
browser frontend under `web/`, an ORM with migrations, and a multi-role user model. **Provenance
Engine is none of those things.** Every path, table and view in the spec therefore has to be
re-mapped onto the code that actually exists before a line of product code is written. This ADR is
that mapping, plus the contract facts the spike proved and the spec got wrong.

## 2. What the repository actually is (audited)

| Concern | Reality at `61f1cb3` |
| --- | --- |
| Shape | Single Go binary `provenance` (module `github.com/sk3y04/provenance-engine`), CI green at baseline |
| User surfaces | Cobra CLI (`cmd/provenance/`) and a Bubble Tea TUI (`internal/tui/`) |
| HTTP server | **None.** No router, no handlers, no SSE/WebSocket push layer, no browser frontend |
| Database | **None for local state.** Optional PostgreSQL (`internal/catalog`, `vault cite/diff`) gated on `PROVENANCE_DATABASE_URL` |
| Durable local state | JSON records under `<root>/.provenance/...`, written with `internal/atomicfile`, read-modify-write serialised by `internal/filelock` (pattern: `internal/torrentqueue/store.go`) |
| Settings | Per-run JSON config (`internal/config`), TUI settings at `$XDG_CONFIG_HOME/provenance/tui-config.json` (fallback `~/.config/provenance/`) |
| Capture/ingest | `internal/manifest` (`CaptureItem`, `CaptureManifest`, `WriteCaptureItem`, `RunPath`, `VerifyCaptureDir`) → `internal/archive.IngestCaptureManifest` into a `blobstore` vault |
| Transport abstraction | `package engine` facade (`engine/`) is transport-agnostic and exposes only `Resolve`/`Download`; feature packages live under `internal/` |
| Auth / roles | **None.** A local single-user tool; there is no user model, no admin role, no session auth |
| Background work | Foreground runners/`internal/worker.Pool`; `context.Context` shutdown honoured everywhere |

Consequences that shape the whole design:

- "Browser-to-`slskd` traffic must proxy through the backend" and "never expose the API key to
  browser clients" are satisfied *structurally*: there are no browser clients. The API key lives in
  a `0600` settings file and never crosses a HTTP boundary that a page could read.
- "DB migration" becomes a versioned JSON record store (the `torrentqueue` precedent), because a
  schema-less local store is what this repo does and adding an ORM for four tables would be a much
  larger change than the feature.
- "UI view" becomes a TUI view and/or a CLI verb. Both are first-class surfaces in this repo.

## 3. Component mapping (spec proposal → this repo)

| Spec deliverable | Landed as |
| --- | --- |
| `internal/slskd/client.go` | `internal/slskd/client.go` — typed HTTP client, `X-API-Key` only, `urlBase` joining, per-op timeouts, TLS policy |
| `internal/slskd/health.go` | `internal/slskd/health.go` — 4-state enum + amber warnings, version floor |
| Settings persistence + backend routes (`POST /slskd/test`, `GET /slskd/health`) | `internal/slskd/settings.go` (file store, masking, env override) + CLI `provenance soulseek settings|health|test` |
| `slskd_download_jobs` / `slskd_download_items` tables | `internal/slskd/jobstore.go` — one JSON record per job under `<root>/.provenance/slskd/jobs/<externalID>.json` (`0600`, atomic rename, `filelock` on `store.lock`) |
| `internal/slskd/search_coordinator.go` | `internal/slskd/search.go` — single-flight queue, ms timeout, wall-clock stop, settle-then-DELETE |
| `internal/slskd/transfer_reconciler.go` | `internal/slskd/transfers.go` — batch enqueue, 255-byte pre-check, adaptive poll, terminal-state classification, snapshot cache |
| `internal/slskd/ingestor.go` | `internal/slskd/ingest.go` — path mapping + containment, `_{ticks}` discovery, size verify, manifest handoff, `IMPORTED`/`IMPORT_FAILED` |
| `internal/slskd/webhook_handler.go` + route `POST /api/integrations/slskd/webhook` | `internal/slskd/webhook.go` (handler + constant-time secret check + TTL dedupe) and CLI `provenance soulseek webhook-listen` as the optional listener (polling stays authoritative) |
| `internal/slskd/recovery.go` | `internal/slskd/recovery.go` — boot reconcile against `slskd` + on-disk staging |
| `slskd_share_rules` table | `internal/slskd/sharerules.go` — JSON records under `<root>/.provenance/slskd/shares/` |
| `internal/slskd/share_manager.go` | `internal/slskd/shares.go` — preview, hardlink materialiser, scan control, paginated browse, zero-share guard |
| `internal/slskd/uploads_service.go` | `internal/slskd/uploads.go` — list/dedupe/cancel/clear |
| `SoulseekSearchView`, `Download Progress Drawer`, `Shares & Uploads` tab, `SoulseekStatusBanner`, `PeerBrowseView` (M5) | CLI verbs under `provenance soulseek ...` for every workflow, plus TUI views (see §8 for what is shipped vs planned) |
| `deploy/docker-compose.soulseek*.yml` (M6) | `deploy/` at repo root (this repo has no `deploy/` today; M6 creates it) |
| DB lineage / `ingested_asset_id` | `manifest.CaptureItem` + `archive.Revision` ID — the repo's existing provenance record, no new lineage store |

## 4. Ownership & persistence boundary (adapted)

| State | Owner | Persisted here? | Rule in this repo |
| --- | --- | --- | --- |
| Soulseek login, listen port, quotas, blacklist | `slskd` | No | Never duplicated; Provenance stores only a connection profile |
| Connection profile (`baseUrl`, `urlBase`, `apiKey`, `webhookSecret`, path maps, TLS) | Provenance | Yes — `$XDG_CONFIG_HOME/provenance/slskd-settings.json` `0600` | Masked on read; `PROVENANCE_SLSKD_*` env overrides win; never logged |
| Active/recent search responses | `slskd` | No — short in-process TTL cache | `DELETE` after the settle grace |
| Download jobs | Provenance | Yes — JSON job records | Survives `slskd` retention expiry and `flags.volatile` restarts |
| Live transfer progress | `slskd` | No — 2–3 s snapshot cache | Invalidated on every mutation |
| Share opt-in rules | Provenance | Yes — JSON rule records | Tracks materialised collections and hardlink counts |
| Indexed share tree, active uploads | `slskd` | No | Fetched on demand, paginated in-process, never cloned into the job store |
| Completed files | `slskd` → Provenance | Yes, post-ingest | Staging file preserved unless cleanup is explicitly enabled |

## 5. Autonomous defaults, as applied

| Decision | Applied choice | Rationale / override |
| --- | --- | --- |
| Tenancy | **Single owner, single Soulseek identity.** One connection profile, one API key, one download root. | This binary is a single-user local tool with no roles; multiplexing identities is impossible by construction. |
| `package engine` coupling | `package engine` stays untouched. The integration is a sibling feature package `internal/slskd`, and the CLI/TUI are adapters over it. | `engine` exposes only `Resolve`/`Download`; widening it for a peer-to-peer transport would be a facade change with no consumer. |
| Secret storage | Settings file `0600` under `$XDG_CONFIG_HOME/provenance/`, with `PROVENANCE_SLSKD_API_KEY` / `_WEBHOOK_SECRET` env override. Masked (`apiKey: "****"` sentinel) on every read; the sentinel on write means "keep stored". | Repo has no keyring and no encryption-at-rest helper; inventing one is out of scope. Env override is the documented preference for headless use. |
| `0.26.1` / `0.26.2` | Non-blocking amber warnings; hard block only `< 0.26.0`. | `#1891` (180 s enqueue crash) and `#1905` (`/options` `403`) are both survivable; verified `403` on `0.26.2` with a `readwrite` key. |
| Post-import disposition | Import via the capture pipeline; **staging file preserved** unless `postImportCleanup` is enabled. Cleanup only after a verified revision. | Matches the repo rule that a fetch hands off to the manifest/archive path and never deletes on failure. |
| Alternate-peer fallback | User-initiated (`--retry --another-source` / TUI action). | Same as spec. |
| `EXDEV` on share targets | Fail fast, actionable error naming both roots. | No safe copy helper with quota check exists in this repo. |
| Recursive cap (M5) | 250 files per recursive enqueue, configurable. | Same as spec. |
| Post-MVP priority | Compose template → wishlist → peer bookmarks → blocklist; chat behind a disabled flag. | Same as spec. |

## 6. Contract facts locked (spike, `0.26.0` / `0.26.2`)

Full evidence, commands and timings: `docs/findings-slskd-0.26-contract.md`.

1. **`searchTimeout` is in milliseconds** — empirically proven (`5` ended the search 53 ms after
   start). The 0.26.0 Swagger *and* `SearchRequest.cs` doc comment both say "seconds"; both upstream
   statements are wrong. This repo sends milliseconds and the contract test pins it.
2. `X-API-Key` is the only accepted credential header; no `Authorization: Bearer`.
3. Composite transfer states serialise as flags with comma+space: `"Completed, TimedOut"`,
   `"Completed, FileLimitReached"` (search), `"InProgress"`, `"Queued, Remotely"`.
4. `GET /api/v0/transfers/downloads?includeRemoved=true` returns a **flat array** of transfers
   (not grouped by batch); correlate by `batchId`, then `(username, filename)`.
5. Batch enqueue status codes: `201` all accepted, `207` partial, `200` all failed, `404` peer
   offline (`"User X appears to be offline"`), `500` message-connection failure, `409` duplicate
   batch id, `429` throttled.
6. `GET /api/v0/shares` returns a map `{"local": [Share...]}` — the key is `local`, **not** the
   alias. `GET /api/v0/shares/contents` returns `[{name, fileCount, files[]}]` with
   **Windows-style backslash** paths.
7. `POST /api/v0/searches` returns the search document inline (`201`/`200`) with
   `state`, `isComplete`, `responseCount`, `token`; polling `?includeResponses=true` returns
   `responses[]` each carrying `username`, `files[]`, `lockedFiles[]`, `uploadSpeed`,
   `hasFreeUploadSlot`, `queueLength`.
8. `GET /api/v0/events` is a **paginated event-record list**, not an SSE stream. There is no SSE
   surface at all; webhooks (`POST /api/v0/events/{type}` raises a test event, `201`) plus adaptive
   polling are the only push mechanisms.
9. `GET /api/v0/server` returns the flattened client-state object
   (`state`, `isConnected`, `isConnecting`, `isLoggedIn`, `isLoggingIn`, `isTransitioning`) — not the
   `ServerState` schema the Swagger advertises. `GET /api/v0/application` wraps it with
   `version{full,current,latest,isUpdateAvailable,...}`, `pendingReconnect`, `pendingRestart`,
   `connectionWatchdog`.
10. **`slskd`'s share scanner follows symlinks and indexes files outside the share root.** With the
    share mounted `:ro`, symlinks `evil.txt -> /etc/hostname` and `shadow.txt -> /etc/shadow` were
    both scanned and advertised (`Sharing 1 directories and 7 files`). Same-filesystem **hardlinks
    are also indexed** (that is what makes the curated-share design work). This is the single
    strongest argument for the hardlink-only materialiser and for refusing symlinks outright.
11. `feature.swagger: true` in YAML enables `/swagger/v0/swagger.json` (200). `web.feature.swagger`
    is not a valid key at 0.26.
12. `GET /api/v0/options` is `200` on `0.26.0` for a `readwrite` key and **`403` on `0.26.2`** for the
    same key (administrator key: `200`). `#1905` reproduced.
13. 0.26.2 vs 0.26.0 path delta: `+ /api/v0/logs/files`, `/logs/files/{filename}`, `/logs/live`;
    `- /application/dump`, `/application/gc`, `/application/loopback`, `/api/v0/logs`. Changed
    schemas: `FlagsOptions` (adds `dumpable`), `LoggerOptions`, `RetentionOptions`,
    `SoulseekOptions+ConnectionOptions`.

Not verified in M0 (carried as non-claims): `#1891`'s 180 s crash path, `FileService.MoveFile`
resulting permissions under `permissions.mode: 755` vs `775` (no real peer download was performed),
`_{ticks}` rename collisions on disk, `SLSKD_SHARED_DIR` vs YAML array merging, and the 255-byte
filename failure mode. These are covered by hermetic unit/integration tests instead, and labelled as
such wherever they appear.

## 7. Where this deliberately diverges from the spec

1. **`searchTimeout` units.** Spec says ms; Swagger and source say seconds; the running binary says
   ms. Implemented as **ms** (`SearchTimeoutMS`), with a contract test that fails if the field is
   ever sent in seconds.
2. **No DB, therefore no migrations.** Durable state is versioned JSON records (`format` field +
   `atomicfile` + `filelock`), following `internal/torrentqueue`. `slskd_download_jobs` and
   `slskd_download_items` collapse into one job record with an `items[]` array because an item never
   outlives its job here and a two-file write is a torn state.
3. **No backend HTTP routes.** Settings/health/test/search/enqueue/cancel/retry/share/uploads are CLI
   verbs over the same Go API a future server would call. The only listener is the optional
   `webhook-listen` command.
4. **No browser UI, no role gate.** "Restrict share toggles to admin/owner" is satisfied by there
   being exactly one local operator.
5. **Ingestion target is the manifest/archive pipeline**, not an "ingested asset" table: one
   `CaptureItem` + one `CaptureManifest` per completed job, then `archive.IngestCaptureManifest`.

## 8. Surface plan (what "UI" means here)

- **M1:** `provenance soulseek settings show|set`, `provenance soulseek health`, `test`.
  Health output is the 4-state contract plus amber warnings.
- **M2:** `provenance soulseek search <query>`, `download <searchID> --peer ...` / `--file ...`,
  `jobs [--watch]`, `ingest` (runs automatically on reconcile).
- **M3:** `cancel`, `retry [--another-source]`, `queue-position`, `clear-completed`,
  `webhook-listen`, boot recovery wired into `jobs --watch`.
- **M4:** `shares preview|add|remove|list|rescan`, `uploads list|cancel|clear`.
- **TUI:** a `Soulseek` menu entry with search → results → enqueue, a jobs panel with cancel/retry,
  and a shares/uploads panel, following `references/tui.md` wiring rules. TUI work is sequenced
  after the backend gates are green; if it does not land in this slice it is reported as not shipped,
  never as done.

## 9. Risks / limits accepted

- A single process is the only writer of the job store (enforced by `store.lock`); two concurrent
  `provenance soulseek` processes will serialise, not merge.
- Search results are cached in memory only, so `download --search-id` requires the same process to
  resolve candidates; the CLI therefore stores the ranked candidates in the job record at enqueue
  time (candidates are not secrets, but peer names are still personal data, so the store is `0600`).
- Webhooks are optional and cannot carry search or failure events; polling remains the reconciler.
- `slskd` symlink-following (finding 10) means the curated share root must be hardlink-only and
  audited after materialisation, and the operator must not place symlinks there themselves.