# Engine Refactor Plan (Phase 1 Audit)

This is the Phase 1 engine-coupling audit required before any file move,
export, or module rename. It is a **plan**, not an implementation. No runtime
Go source was changed by Phase 1.

Scope of the future public engine (see `AGENTS.md`): Instagram, X, Reddit,
yt-dlp and browser extractors; resolution, downloading, ffmpeg processing,
collections, archive primitives, CLI and TUI. It must never contain Nuxt, River,
product authentication, billing, quotas, or private SaaS code.

All references below are `path:line` against the Phase 0 baseline commit
`9a50a0f`.

---

## 1. Current dependency / call-path map

```text
cmd/provenance/main.go (cobra)
├── grab/download  main.go:217 downloadCmd
│     RunE → buildOptions(main.go:1496) → app.Download             internal/app/app.go:21
│           └── dispatcher.NewCountsReporter                       dispatcher.go:63
│           └── dispatcher.BatchDispatch / dispatcher.Dispatch     dispatcher.go:596 / :174
│           └── dispatcher.PrintSummary                            dispatcher.go:101
│     or runSessionDownload (main.go:1464) with session.Session as Reporter
├── scan           main.go:316 scanCmd
│     → app.Scan (app.go:39) → dispatcher.Scan                     dispatcher.go:296
│     → app.ScanResolved (app.go:52) → dispatcher.ScanResolved     dispatcher.go:350
│         → resolve.Source / resolve.Item                          resolve.go:1
├── collect sync   main.go collectCmd → collection.Sync            collection/sync.go:24
│     → dispatcher.Scan (:59) → app.Download (:98) → session.OpenOrCreate (:89)
├── archive        main.go archiveCmd → dispatcher.Dispatch / collection / session /
│     importers.Import* (importers/*.go) → archive/* + blobstore/*
├── vault          main.go vaultCmd → archive.IngestFromOutput     archive/ingest.go:224
│     → catalog.SetStore / catalog.Store                           catalog/interface.go:69/:73
├── encode         main.go:242 encodeCmd → encode.Run              encode/run.go:68
│     → worker.NewPool (:190) → encode.runOne (:236) → runner.go:30 exec.CommandContext
└── tui            main.go tuiCmd → tui.Run → dispatcher/app via adapters
```

### Dispatch → extractor routing

`dispatcher.Classify` (`dispatcher.go:145`) maps a host to a `Site`
(`dispatcher.go:116`). `dispatcher.Dispatch` (`:174`) forwards to:

- Patreon → `extractor.RunYtdlp` (`dispatcher.go:207`)
- X/Twitter → `extractor.DownloadTwitter` (`:210`)
- Reddit → `extractor.DownloadReddit` (`:220`)
- Instagram → `extractor.DownloadInstagram` (`:231`)
- Album → `extractor.DownloadAlbum` (`:241`)
- Generic → `extractor.RunYtdlpCaptured` then `browserFallback` (`:250`, `:263`)

`dispatcher.Scan` (`:296`) and `dispatcher.ScanResolved` (`:350`) mirror this
with `Scan*` / `Scan*Resolved` variants. Normalized resolution types live in
`internal/resolve/resolve.go` (`Source`, `Item`, `MediaAsset`, `TextContent`,
`SourceKind`, `MediaKind`).

---

## 2. Coupling inventory

### 2.1 Direct stdout/stderr writes below CLI presentation

These are the primary blocker for reusing execution paths from a web worker.

| Location | What it writes |
|---|---|
| `dispatcher.go:101` `PrintSummary` | count summary to `os.Stderr` |
| `dispatcher.go:268,272,275,280,282,289` `browserFallback` | crawl/sniff narration to stdout/stderr |
| `dispatcher.go:471,486,495,502,505,541,546,559,574,586,619,647` `downloadVideoLinks` / `BatchDispatch` | progress, skips, retries, failures |
| `app.go:34,36` `app.Download` | per-URL error + `diagnose.Hint` |
| `collection/sync.go:84,94,134,250,255,263,266` `syncCollection`/`recordCapture` | sync status + dry-run |
| `extractor/ytdlp.go:122-132` `runYtdlpInternal` | ffmpeg-missing warning |
| `extractor/ytdlp.go:189-190` | `os.Stdout`/`os.Stderr` progress writers |
| `extractor/instagram.go` (e.g. `:76,286,328,336,365,372,378,381,387,390,396,399,405,498,505,507,676,723,732,765,775,803,845,854,893,937,946,977,1051,1096,1105,1131,1138,1580,1591,1596,1623,1669`) | auth/rate-limit/pagination/scan narration |
| `extractor/twitter.go` (`:327,382,392,730,734,742,791,801,1125,1136,1141,1174`) | same |
| `extractor/twitter_search.go` (`:146,153,161,238,257,267,275`) | same |
| `extractor/reddit.go` (`:254,435,615,626,631,658`) | same |
| `extractor/album.go` (`:280,302,308,389,607,655,945,1014`) | same |
| `extractor/browser.go` (`:630,644,647,652,662,673`) | scrape/pagination narration |
| `encode/run.go:304,311` (`reporter.fail`/`linef`), `encode/encoder.go:145,190,236` | encode progress/dry-run |
| `importers/web.go:64` | capture failure |
| `session/store.go:186,228,326,330,334` | session save warnings/conflicts |

Already parameterized (safe): `manifest.PrintHuman(w io.Writer, m Manifest)`
(`manifest/manifest.go:154`) and `urlArchive` writes (`dispatcher.go:824`) which
write to a file, not the terminal.

### 2.2 `os.Exit`, `log.Fatal`, `panic`, process-global mutation

- `cmd/provenance/main.go:192` — the only `os.Exit(1)` in non-test code.
- No `log.Fatal` and no control-flow `panic(` found in non-test `cmd/`/`internal/`.
- **Process-global stream mutation**: `internal/tui/runner.go:310`
  `redirectStdStreams` reassigns `os.Stdout`/`os.Stderr` (`:329-330`,
  `:356-357`). This is how the TUI suppresses extractor output; it is unsafe to
  run concurrently with any other consumer of stdout (e.g. a future worker).
- Signal handling: `main.go:184` `signal.NotifyContext(ctx, os.Interrupt,
  syscall.SIGTERM)` — single root context; subprocesses are killed through it.

### 2.3 Terminal / TUI dependencies in reusable paths

- `internal/downloader/downloader.go:266-290` — when `Client.Progress == nil`
  the downloader falls back to `schollz/progressbar` writing to the terminal.
  A non-terminal caller must always supply `Progress`.
- The TUI directly imports and calls engine internals rather than going through
  a facade: `tui/runner.go:248` (`teaReporter` over `dispatcher.Reporter`),
  `tui/runner.go:290` (`fileReporter` over `downloader.ProgressReporter`),
  `tui/collections.go:259,267` (`collection.Sync`/`SyncAll`),
  `tui/update.go:255` and `tui/vault.go:19,79,154,158` (`catalog.Store()`),
  `tui/mainmenu.go:154,163` (`catalog.NewPgStore` + `catalog.SetStore`).
- `tui/views.go:971-975` shells out to `explorer`/`open`/`xdg-open` (presentation
  only; stays in the adapter).

### 2.4 Hard-coded filesystem / output assumptions

- Default output dir `"downloads"`: `dispatcher.go:451,702`,
  `extractor/ytdlp.go:140,228,494`.
- Cache/archive layout: `dispatcher.go:465` `_provenance_cache`,
  `:469` `archive.txt`, `:474` `ytdlp_archive.txt`, `:700` `writeLinkCache`.
- Per-file resume suffix `dest + ".part"`: `downloader.go:171`.
- Capture manifests under `<outputDir>/.provenance/`:
  `manifest/provenance.go:22` (`RunPath`), `collection/sync.go:250`,
  `archive/ingest.go:232`, `main.go:1109`.
- Sidecar metadata dir `_metadata`: `extractor/ytdlp.go:219-230`.
- Temp dirs: `importers/web.go:75` `os.MkdirTemp("", "provenance-web-")`,
  `importers/git.go:38` `os.MkdirTemp("", "provenance-git-")`.
- Persistence defaults via `os.UserCacheDir`: `session/store.go:436`,
  `history/store.go:239`, `watch/store.go:197`, `collection/store.go:191`;
  TUI config `XDG_CONFIG_HOME`: `tui/config.go:403`.

### 2.5 Package-level stores, registries, config, clients, mutable globals

| Global | Location |
|---|---|
| `defaultStore CatalogStore` + `SetStore`/`Store`/`HasStore` | `catalog/interface.go:67,69,73,77` |
| `mu sync.Mutex` guarding collections file | `catalog/catalog.go:15` |
| `twitterTransport` | `extractor/twitter.go:27` |
| `twQueryIDCache struct{...}` (+ `mu`) | `extractor/twitter.go:85,86` |
| `twFeatures` map | `extractor/twitter.go:218` |
| `igTransport`, `igAPIClient`, `igAPIHosts` | `extractor/instagram.go:26,39,59` |
| `ffmpegOnce`, `ffmpegPath`, `ffmpegWarn` | `extractor/ytdlp.go:28-32` |
| album globals (`albUserAgent`, `albAPIPathSuffixes`, regexes) | `extractor/album.go:60,91,97,137` |
| `lookPath = exec.LookPath` (test seam) | `encode/discover.go:82` |
| `rateLimiter *ratelimit.Manager` + ~40 flag vars | `main.go:37-95` |
| package-level persistence locks | `session/store.go:50`, `collection/store.go:19`, `history/store.go:20`, `watch/store.go:17` |

`ratelimit.Manager` itself is instance-based and injectable (`ratelimit.go:21`,
`dispatcher.Options.RateLimiter` `dispatcher.go:34`) — a good template.

### 2.6 Context cancellation propagation and lost contexts

Good propagation: the root context flows through `app.Download` →
`dispatcher.Dispatch` → extractor functions; `downloader` uses
`newContextReader` (`downloader.go:381-397`); `worker.NewPool` derives a child
context (`worker/pool.go:50`); yt-dlp builds its `exec.Cmd` with `ctx`
(`ytdlp.go:181`).

Lost or fabricated contexts:

- `importers.ImportGit/ImportPDF/ImportDocs/ImportOpenAPI` take **no
  `context.Context`** at all (`importers/git.go:33`, `importers/pdf.go:14`,
  `importers/docs.go:17`, `importers/openapi.go:28`). `ImportGit` runs
  `exec.Command` without a context (`git.go:49,54`).
- `importers/web.go:47` builds the chromedp allocator from
  `context.Background()` instead of a caller context.
- `main.go:177` falls back to `context.Background()` in
  `PersistentPreRunE`; `main.go:1160` calls
  `catalog.NewPgStore(context.Background(), connStr)`.
- TUI discards cancellable context throughout: `tui/collections.go:259,267,280,287`,
  `tui/update.go:255`, `tui/vault.go:19,79,154,158`.

### 2.7 Subprocess invocation

| Tool | Invocation |
|---|---|
| yt-dlp | `extractor/ytdlp.go:136-200` (go-ytdlp `cmd.BuildCommand(ctx, ...)`); scan `:442`, `:598`; install `EnsureInstalled :69` → `ytdlp.MustInstall` |
| ffmpeg / ffprobe | `encode/runner.go:30 exec.CommandContext`; `encode/discover.go:82`; detection `extractor/ytdlp.go:34` |
| Chrome (chromedp) | `extractor/browser.go:93,314,433` `chromedp.NewExecAllocator`; candidates `:43`; `FindChromeExecutable :56`; importers `importers/web.go:39-51` |
| git | `importers/git.go:49,54 exec.Command` (no context) |
| reveal-in-finder | `tui/views.go:971-975` (presentation only) |

### 2.8 Cookies and credential flow

- Cookie file `--cookies` → `YtdlpOptions.CookiesFile` (`ytdlp.go:48,171`);
  `--cookies-from-browser` (`ytdlp.go:49,517`).
- Twitter: `TWITTER_BEARER_TOKEN` (`extractor/twitter.go:71`) and query-id env
  overrides (`twitter.go:95,109,128`), consumed around `:382,730`.
- Reddit OAuth: `REDDIT_OAUTH_TOKEN`/`REDDIT_CLIENT_ID`/`REDDIT_CLIENT_SECRET`
  (`extractor/reddit.go`).
- Instagram: `INSTAGRAM_APP_ID` and exported cookies (`extractor/instagram.go`).
- Chrome path `CHROME_PATH` (`extractor/browser.go:57`, `importers/web.go:229`).
- PostgreSQL DSN `PROVENANCE_DATABASE_URL` (`main.go:1156,1178`).
- Persistence path overrides `PROVENANCE_SESSION_DIR`/`_WATCH_FILE`/
  `_HISTORY_FILE`/`_COLLECTION_FILE`.

Cookies are currently never logged, but extractor diagnostics print
`sanitizeErrorBody` previews (`instagram.go:1096` etc.) — see §7.

### 2.9 Current progress / reporting abstractions

- `dispatcher.Reporter` (`dispatcher.go:43-49`): `Queue/Start/Success/Failure/Skip`.
  Implemented by `CountsReporter` (`:58`), `session.Session`
  (`session/store.go:182-211`), `tui.teaReporter` (`tui/runner.go:250`).
- `dispatcher.Counts` atomic counters (`dispatcher.go:51`).
- `downloader.ProgressReporter` (`downloader.go:34-38`): `OnStart/OnProgress/OnDone`.
  Implemented by `tui.fileReporter` (`tui/runner.go:292`).
- yt-dlp progress protocol: marker `PROVENANCE_YTDLP_PROGRESS:`
  (`ytdlp.go:65`), parsed by `ytdlpProgressWriter` (`:240-363`).
- `encode.reporter` (`encode/run.go:268`) — stderr-only, no interface.

These two interfaces are the seed for the Phase 3 `EventSink`, but they are
URL-lifecycle and per-file oriented, not stage/summary/typed-event oriented.

### 2.10 Current normalized source / result types

- `internal/resolve/resolve.go` — `Source`, `Item`, `MediaAsset`, `TextContent`
  (already the "future" normalized model per its package comment).
- `internal/manifest/manifest.go` — `Item`, `Manifest`, `FilterOptions`,
  `Summary`; `manifest/capture.go` — capture manifest for the vault.
- `internal/config/config.go` — serializable `Config` embedded in
  `dispatcher.Options` (`dispatcher.go:30`).

### 2.11 Tests that protect CLI compatibility

- Dispatcher routing/archives: `internal/dispatcher/dispatcher_test.go`.
- Resolution shape: `internal/extractor/resolve_test.go`,
  `internal/extractor/ytdlp_test.go` (progress parsing),
  `internal/extractor/twitter_*_test.go`, `reddit_*_test.go`,
  `instagram_test.go`, `album_test.go`, `ytdlp_fixtures_test.go`.
- Persistence/session: `internal/session/store_test.go`,
  `internal/collection/store_test.go`, `internal/collection/sync_test.go`,
  `internal/watch/*_test.go`, `internal/history/store_test.go`.
- Worker/encode: `internal/worker/pool_test.go`,
  `internal/encode/{discover,encoder,run,integration}_test.go`.
- TUI: `internal/tui/{config,sessions}_test.go`.

---

## 3. Recommendation: public package location

**Recommendation: a dedicated `engine/` package at the module root, not the
module root itself.**

Rationale from the actual layout:

- The module root currently holds no Go package; all code is under `cmd/` and
  `internal/`. Introducing a root package would make the module import path
  itself a library and collide conceptually with `cmd/provenance`.
- Every implementation package already lives under `internal/`, so an `engine/`
  facade can wrap them without forcing any implementation to move during
  Phases 3–5.
- Phase 6's "external import test" is cleaner with an explicit
  `github.com/sk3y04/provenance-engine/engine` import path, and `go doc` output
  reads naturally.

This decision is **not implemented** in Phase 1; it is confirmed in the Phase 1
human gate.

---

## 4. Proposed minimal public engine facade

First release supports **Resolve and Download only** (per the phase plan).
Archive/vault/encode/imports stay CLI-local until a private consumer needs them.

```go
package engine // github.com/sk3y04/provenance-engine/engine

type Engine struct{ /* unexported deps; no package globals */ }

func New(cfg Config) (*Engine, error)

// Resolve discovers a source without downloading.
func (e *Engine) Resolve(ctx context.Context, req ResolveRequest, sink EventSink) (Source, error)

// Download resolves (if needed) and writes artifacts under the configured work dir.
func (e *Engine) Download(ctx context.Context, req DownloadRequest, sink EventSink) (Result, error)
```

Design rules:

- `Engine` holds explicit dependencies (HTTP client factory, rate-limiter
  manager, cookie source, tool paths). No `catalog.SetStore`-style setters.
- All public signatures use only `engine` types and standard library types.
- Implementation wraps existing `internal/extractor`, `internal/dispatcher`,
  `internal/downloader`, `internal/resolve` — nothing is copied.
- `cmd/provenance` and `internal/tui` become adapters (Phase 5).

### 4.1 Request / result / artifact contracts

```go
type Config struct {
    WorkDir          string        // scratch/output root; required
    CookiesFile      string        // optional Netscape cookies
    CookiesFromBrowser string
    Quality          string        // "best"|"1080"|"720"|"480"
    AudioOnly        bool
    Concurrency      int
    SpeedLimitBps    int64
    RateLimit        RateLimitConfig // per-host overrides; no raw yt-dlp flags
    ToolPaths        ToolPaths       // ffmpeg/ffprobe/chrome overrides, optional
    MaxItems         int             // mandatory bound for hosted use
    Timeout          time.Duration
}

type ResolveRequest struct {
    URL    string
    Limit  int             // max items to enumerate
    Filter FilterOptions   // extension/size/title filters
}

type DownloadRequest struct {
    URL           string
    Limit         int
    Filter        FilterOptions
    IncludePosts  bool
    IncludeComments bool
    // No OutputTemplate / raw yt-dlp args in the public contract.
}

type Source struct {
    URL, CanonicalURL string
    Kind              SourceKind    // feed|single|playlist
    Extractor         string
    Title, Author     string
    Items             []Item
}

type Item struct {
    ExternalID string
    URL        string
    Title      string
    Author     string
    PublishedAt *time.Time
    Media      []MediaAsset
    Text       *TextContent
}

type MediaAsset struct {
    URL, Filename, Extension string
    Size                     int64
    Kind                     MediaKind // image|video|audio
}

type Artifact struct {
    Path      string // absolute path inside WorkDir (engine-owned)
    Filename  string
    Size      int64
    SHA256    string
    MIMEType  string
    ItemRef   string // Item.ExternalID it belongs to
}

type Result struct {
    Source    Source
    Artifacts []Artifact
    Counts    ResultCounts // discovered/succeeded/failed/skipped
    Warnings  []string
}
```

`Source`/`Item`/`MediaAsset` above are exported projections of
`internal/resolve` (`resolve.go:24-88`); the facade maps them so internal types
never appear in public signatures.

### 4.2 Event and typed-error contracts

```go
type EventKind string
const (
    EventStageChanged EventKind = "stage_changed" // resolving|downloading|processing|done
    EventProgress     EventKind = "progress"      // bytes/items, monotonic
    EventWarning      EventKind = "warning"
    EventItemDone     EventKind = "item_done"
    EventRetry        EventKind = "retry"
    EventSummary      EventKind = "summary"
)

type Event struct {
    Kind    EventKind
    Stage   Stage
    URL     string
    ItemRef string
    Written int64
    Total   int64 // -1 when unknown
    Attempt int
    Message string // stable fact, NOT preformatted UI text
    At      time.Time
}

// EventSink receives structured events. Implementations MUST NOT block the
// extraction path indefinitely and MUST NOT be able to cancel it. A slow sink
// must drop events, not stall; sink panics are recovered and ignored.
type EventSink interface {
    Emit(ctx context.Context, e Event)
}
```

Typed errors — one construct so callers branch with `errors.Is`/`errors.As`
without string matching:

```go
type ErrorKind string
const (
    KindValidation        ErrorKind = "validation"
    KindAuthRequired      ErrorKind = "authentication_required"
    KindUnsupportedSource ErrorKind = "unsupported_source"
    KindRateLimited       ErrorKind = "rate_limited"
    KindTemporary         ErrorKind = "temporary"
    KindCanceled          ErrorKind = "canceled"
    KindExternalTool      ErrorKind = "external_tool"
    KindPermanent         ErrorKind = "permanent"
)

type Error struct {
    Kind  ErrorKind
    Op    string
    URL   string
    Err   error
}
func (e *Error) Error() string
func (e *Error) Unwrap() error

// Cancellation is never re-categorized: if errors.Is(err, context.Canceled) or
// context.DeadlineExceeded, Kind is KindCanceled and Unwrap returns the context error.
```

Mapping source: `dispatcher.isPermanentYtdlpFailure` (`dispatcher.go:673`),
`dispatcher.shouldBrowserFallback` (`dispatcher.go:404`), `worker.Permanent`
(`worker/pool.go:21`), and `internal/diagnose` (`diagnose/diagnose.go:6`
`Hint`) currently
encode these categories implicitly via strings — Phase 3 replaces the string
matching with `ErrorKind` while keeping `diagnose.Hint` for CLI display.

### 4.3 Artifact ownership

Initial release keeps **a controlled work directory** (`Config.WorkDir`),
matching current behavior where downloads land under an output dir
(`dispatcher.go:449-452`). An `ArtifactSink` abstraction (streaming artifacts
out of the engine without a shared filesystem) is **deferred**; the Phase 1
gate asks the human to confirm this. `Result.Artifacts[i].Path` is documented as
engine-owned scratch until the private worker uploads then deletes it.

---

## 5. Adapter boundaries

```text
cmd/provenance (CLI)  ─┐
internal/tui (TUI)    ─┼─> engine.Engine ──> internal/dispatcher ──> internal/extractor
future private worker ─┘        │                     │                    │
                                │                     └── internal/downloader, ratelimit, worker
                                └── engine.EventSink ──> CLI/TUI render / worker persistence
```

- **CLI adapter**: maps cobra flags (`main.go:37-95`) → `engine.Config` /
  `DownloadRequest`; implements `EventSink` by rendering to stderr (replaces
  `dispatcher.PrintSummary`, `app.Download` prints). Owns exit codes.
- **TUI adapter**: `tui.runner.go` `teaReporter`/`fileReporter` (`:250`,`:292`)
  become `EventSink` implementations; the global `redirectStdStreams`
  (`runner.go:310`) is retired once engine paths stop writing to the terminal.
- **Future worker adapter** (private repo): implements `EventSink` to persist
  bounded job events; calls `Resolve`/`Download` in-process. Never shells out to
  the CLI.
- **Presentation-only code stays out of the engine**: colors/prompts
  (`internal/tui`), `reveal-in-finder` (`tui/views.go:971`), `diagnose.Hint`
  rendering.

---

## 6. Incremental file-by-file sequence

Ordered so each step is independently testable and behavior-preserving.

1. **`internal/resolve`** — extend with any missing fields needed by the facade
   (e.g. per-item kind already present). No behavior change.
2. **`internal/engineerr` (new internal package)** — define typed error kinds
   and constructors; add unit tests for `errors.Is`/`errors.As`. No callers yet.
3. **`internal/extractor`** — add an optional `EventSink` (or accept the Phase 3
   sink type) alongside `downloader.ProgressReporter`; keep all existing
   `fmt.Fprintf(os.Stderr, ...)` calls working when the sink is nil. Convert one
   narrow path first: `extractor.RunYtdlp`/`runYtdlpInternal`
   (`ytdlp.go:106-201`).
4. **`internal/dispatcher`** — thread the sink through `Dispatch`/`Scan`/
   `ScanResolved`; move `PrintSummary` (`:101`) and `browserFallback` narration
   behind the sink; keep `Reporter` for session compatibility.
5. **`internal/app`** — allow `app.Download` (`app.go:21-38`) to accept a sink
   and stop printing directly.
6. **`internal/downloader`** — make the progressbar fallback (`downloader.go:266-290`)
   opt-in; require an explicit sink for non-terminal use.
7. **`engine/` (new public package)** — `Config`, requests, DTOs, `Event`,
   `EventSink`, `Engine.Resolve`, `Engine.Download`; wrappers over
   `internal/dispatcher`. Compile-time external-consumer example.
8. **`cmd/provenance/main.go`** — map flags to `engine` requests; implement the
   CLI `EventSink`; delete direct `dispatcher.PrintSummary` use.
9. **`internal/tui`** — adapt `teaReporter`/`fileReporter`; remove
   `redirectStdStreams`.
10. **Deferred**: `engine.ArtifactSink`, archive/vault/encode/import facades —
    only when the private product needs them.

No file is moved between packages in Phases 3–5 except creating `engine/` and
`internal/engineerr/`; `internal/*` implementations stay where they are.

---

## 7. Compatibility and security risks

### Compatibility

- **Terminal output is de facto API.** CLI users and scripts may scrape
  `[provenance]`-prefixed stderr lines; moving them behind an adapter must
  preserve text and stream. `PrintSummary` (`dispatcher.go:101`) format is a
  compatibility surface.
- **yt-dlp progress protocol** `PROVENANCE_YTDLP_PROGRESS:` (`ytdlp.go:65`) and
  the TUI's stripping of it (`tui/runner.go:345`) must remain consistent.
- **`scan --json`** emits `resolve.Source` (`resolve.go`); changing exported
  JSON field names breaks scripts.
- **Exit codes**: `0` success / `1` error (`main.go:187-193`); Phase 5 must map
  typed errors to the same codes by default.
- **Persistence formats/paths**: sessions (`session/store.go`), history, watch,
  collections, `_provenance_cache/archive.txt`, `.part` resume files, and
  `.provenance/` capture manifests must not change.
- **Auto-install** `PersistentPreRunE` (`main.go:171-182`,
  `shouldSkipAutoInstall :196`) runs before every command and calls
  `extractor.EnsureInstalled` — the engine will need an equivalent explicit
  setup step; the CLI adapter keeps current behavior.

### Security

- **Cookies/credentials** must never appear in `Event.Message` or logs; the
  extractor diagnostic previews use `sanitizeErrorBody` but still print to
  stderr (e.g. `instagram.go:1096`, `album.go:1014`). Events must carry stable
  facts, not raw response bodies.
- **SSRF**: `downloader` blocks private networks (`downloader.go:103-135`
  `isPrivateHost`, `SafeRedirect`) and `web.go` validates scheme
  (`importers/web.go:28`). The facade must not weaken this and must reject
  caller-supplied arbitrary URLs/redirects for hosted use (Phase 12).
- **Subprocess input**: `importers/git.go:27` rejects `-`-prefixed refs and
  `ext::`; `validateGitArgs` (`:14`) is the pattern to preserve. The facade must
  not expose raw yt-dlp args, output templates, proxy, or paths (see §4.1).
- **Process-global stdout mutation** (`tui/runner.go:329`) must be gone before
  the engine runs in a server; otherwise concurrent jobs corrupt each other.
- **Global catalog store** (`catalog/interface.go:67`) is process-wide state; a
  hosted worker must construct its own store, never `SetStore`.

---

## 8. Required tests for each later step

| Step | Required tests |
|---|---|
| Typed errors (§6.2) | `errors.Is/As` table tests for every `ErrorKind`, including cancellation not re-categorized |
| Event contract (§6.3) | event ordering for a fixture download; no-op sink; failing/panicking sink does not stall or cancel extraction |
| Extractor conversion (§6.3) | existing `extractor/*_test.go` still pass; new test asserts no `os.Stderr` write when a sink is supplied |
| Dispatcher conversion (§6.4) | `dispatcher_test.go` plus a test that `PrintSummary`-equivalent facts are emitted as `EventSummary` |
| Downloader sink (§6.6) | test that `Progress == nil` + non-terminal still downloads without progressbar when configured; context cancel mid-stream returns `context.Canceled` |
| Facade (§6.7) | external-module compile test importing only `engine`; fake HTTP server fixtures; `Engine.Resolve`/`Download` without any TUI init; request validation/limits |
| CLI adapter (§6.8) | golden stderr for `grab`/`scan` stability; exit-code mapping per `ErrorKind`; `scan --json` shape unchanged |
| TUI adapter (§6.9) | existing `tui` tests plus assertion that no global stdout mutation remains |

All tests must use fixtures/fake servers; none may call live social-media
services (matches CONTRIBUTING.md and the Phase 0 baseline).

---

## 9. Phase 1 acceptance status

- Every coupling claim above includes a `file:line` / symbol reference — yes.
- The proposed API exposes no internal extractor implementation — yes
  (facade uses only `engine` + stdlib types).
- The plan preserves CLI/TUI behavior — yes (adapters preserve streams, exit
  codes, persistence, protocol markers).
- No runtime code changes — yes; this phase adds documentation only.

## Open decisions for the Phase 1 human gate

1. Public package location: `engine/` subpackage (recommended) vs module root.
2. First web-supported operations: `Resolve` + `Download` only (recommended).
3. Event model and typed-error categories as specified in §4.2.
4. Artifact output: controlled `WorkDir` for now (recommended) vs introducing
   `ArtifactSink` immediately.

---

## 10. Phase 3 implementation status

Phase 3 introduced the contracts and converted the yt-dlp single-URL
Download/Resolve path. The public `engine/` facade is still Phase 4.

### Added

- `internal/engineerr` — `Kind` categories, `Error`, `New`/`Newf`, `KindOf`,
  `Is`, `As`; cancellation is never re-categorized. Tests cover `errors.Is`/`As`,
  every category, and cancellation.
- `internal/event` — `Event`, `Kind`, `Stage`, `Counts`, `Sink`, `Nop`, `Func`,
  `Emit`, and `Notifier` (bounded, drop-on-full, panic-recovering delivery so a
  slow or foreign sink can neither stall nor cancel extraction). Tests cover
  ordering, panic recovery, non-blocking drop, close idempotency, and
  cancellation dropping.
- `internal/render` — CLI `Sink` rendering structured events to an `io.Writer`;
  terminal formatting lives only here. Tests pin exact output.
- `internal/dispatcher.Options.Events` and `extractor.YtdlpOptions.Events`.

### Converted (event-driven, no terminal when a sink is set)

- `extractor.RunYtdlp` / `runYtdlpInternal`, `ScanYtdlp`, `ScanYtdlpResolved`:
  emit stage/warning/progress/item events, use an event→`ProgressReporter`
  adapter, suppress the ffmpeg warning and progress fallback writes, and return
  `engineerr`-categorized failures (`UnsupportedSource`, `AuthRequired`,
  `RateLimited`, `Permanent`, `ExternalTool`, `Canceled`).
- `dispatcher.Dispatch` / `Scan` / `ScanResolved` (generic/yt-dlp branch) and
  `browserFallback`: emit events when a sink is set, legacy `[provenance]` output
  byte-for-byte when it is nil.
- `app.Download`: emits `ItemDone` failure events and an `EventSummary` (from the
  same `Counts`), and returns a `Validation` error for empty input, instead of
  printing.

### Deviations from §4.2 (recorded per the phase prompt)

- The event carries `Reason` (stable code), optional `Detail`,
  `Err error`, and `Summary *Counts` instead of a single `Message` string. This
  keeps stable facts machine-readable and avoids preformatted UI strings.
- Public `engine` names are deferred; internal packages use `event.Sink` /
  `engineerr.Kind`. Phase 4 will expose stable public aliases/DTOs.

### Deferred (documented transitional paths)

- CLI/TUI are **not** wired to the sink yet; they still use the nil-sink legacy
  path, so user-visible behavior is unchanged. Wiring is Phase 5.
- `downloader` progressbar fallback (§6.6) is unchanged; it remains opt-out via a
  non-nil `Progress`/`Events` adapter.
- `dispatcher.downloadVideoLinks` / `BatchDispatch` narration remains legacy;
  their yt-dlp calls receive `Events` so per-file progress does not hit the
  terminal, but the surrounding start/OK/FAILED lines are still direct writes.
- Custom extractors (Twitter, Reddit, Instagram, Album, browser narration) still
  write to the terminal; typing and event conversion of those paths continues in
  Phases 4–5 as needed for Resolve/Download.

### Verified

- `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...`, and
  `go build -o provenance ./cmd/provenance` all pass.
- New tests: `internal/engineerr`, `internal/event`, `internal/render`,
  `internal/dispatcher/events_test.go`, `internal/extractor/events_test.go`,
  `internal/app/app_test.go` (asserts no stderr with a sink and legacy stderr
  without one). No test contacts a live service.

---

## 11. Phase 4 implementation status

Phase 4 created the public `engine/` facade (§3/§4) wrapping the Phase 3
event-driven internals. No `internal/*` implementation modules were moved or
copied.

### Added

- `engine/` public package: `Config`, `ResolveRequest`, `DownloadRequest`,
  `FilterOptions`, `Source`/`Item`/`MediaAsset`/`TextContent`, `Artifact`,
  `Counts`, `Result`; `Event`/`EventKind`/`Stage`/`EventSink` plus `NopSink`
  and `SinkFunc`; `Error`/`ErrorKind` with `ErrorKindOf`/`IsErrorKind`.
- `Engine.New` (explicit construction, no globals), `Engine.Config`,
  `Engine.Resolve`, `Engine.Download`.
- Bounded, panic-recovering event delivery via the Phase 3
  `internal/event.Notifier`; a nil public sink becomes a discarding sink so the
  wrapped paths never fall back to terminal output.
- Artifact discovery: every regular file under `Config.WorkDir`, excluding
  `_provenance_cache`/`.provenance` and `.part` files, with absolute path,
  size, MIME type, and SHA-256.
- Boundary defenses: request/URL/scheme/filter validation, `MaxItems` clamping,
  per-call `Timeout`, pre-flight cancellation checks, and conversion of
  recovered panics (e.g. the third-party yt-dlp installer) into typed errors so
  a server cannot crash on them.
- `engine/externaltest/` — a standalone external Go module (`replace ../..`)
  importing only the facade; `make engine-example` builds it.
- Tests: `engine/engine_test.go` (public API), `engine/engine_internal_test.go`
  (mapping, artifact scan, error categorization, sinks),
  `engine/example_test.go` (compile-time external usage). All hermetic; no live
  service and no yt-dlp/ffmpeg invocation.
- `docs/ENGINE.md`; `docs/ARCHITECTURE.md` facade layer and package entry.

### Decisions taken (matching the approved document)

- Public package location: `engine/` subpackage at the module root.
- First web-supported operations: `Resolve` + `Download` only.
- Error categories as §4.2; cancellation never re-categorized; unknown causes
  map to `temporary` so they stay retryable by default.
- Artifact output: controlled `WorkDir` only; `ArtifactSink` remains deferred.
- The public event type keeps the Phase 3 field set (`Reason`, `Detail`, `Err`,
  `Summary`) rather than the single `Message` string drafted in §4.2, so stable
  facts stay machine-readable.

### Deferred (unchanged from Phase 3)

- CLI/TUI are not yet wired to the facade; wiring is Phase 5.
- Native custom extractors (Instagram/X/Reddit/Album/browser narration) still
  write some text directly to the terminal; event conversion of those paths
  continues as needed. The generic yt-dlp path and top-level coordination are
  fully event-driven.
- `ArtifactSink`, and facades for archive/vault/encode/import remain deferred.

### Verified

- `go test -race ./engine/`, `go vet ./engine/`, and
  `golangci-lint run ./...` pass; `go build ./cmd/provenance` still succeeds.
- `cd engine/externaltest && go build ./...` compiles against the facade with
  only exported identifiers.