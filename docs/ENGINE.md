# Engine API

`github.com/sk3y04/provenance-engine/engine` is the stable, importable facade
over the provenance engine. It exists so a server-side worker (or any Go
program) can resolve and download supported public sources **in-process**
without invoking the CLI, initializing the TUI, or touching a terminal.

The CLI and TUI are adapters over the same internals. The facade wraps
`internal/*`; it does not copy extractors.

## Scope

The first release exposes exactly two operations:

| Operation | Purpose |
|-----------|---------|
| `Engine.Resolve` | Discover a supported public source and its items without downloading. |
| `Engine.Download` | Resolve as needed, write artifacts under `Config.WorkDir`, and report typed metadata. |

Archive, vault, collection, encode, and import functionality remain CLI-local
until a private consumer needs them.

## CLI consumption

The public CLI is a consumer of this facade: a plain `provenance grab` (http(s)
or X/Twitter hashtag sources, without `--dry-run`, `--filename-template`,
`--batch`, or `--session`) calls `Engine.Download` via
`cmd/provenance/engine_adapter.go` and renders its structured events to stderr
with a typed summary. Options the facade intentionally omits, and the richer
`scan` manifest/JSON contract, remain on the legacy internal path. See
[`ENGINE_REFACTOR.md`](ENGINE_REFACTOR.md) for the transitional boundary.

## Example

```go
eng, err := engine.New(engine.Config{
    WorkDir:     "/var/lib/provenance/jobs/42",
    Quality:     "720",
    Concurrency: 2,
    MaxItems:    50,
    Timeout:     10 * time.Minute,
})
if err != nil {
    return err
}

src, err := eng.Resolve(ctx, engine.ResolveRequest{URL: raw, Limit: 25}, sink)
if err != nil {
    return err
}

res, err := eng.Download(ctx, engine.DownloadRequest{URL: raw}, sink)
if err != nil {
    switch engine.ErrorKindOf(err) {
    case engine.ErrorAuthRequired, engine.ErrorRateLimited, engine.ErrorTemporary:
        // retryable
    case engine.ErrorCanceled:
        // stop
    default:
        // permanent for this request
    }
}
```

## Configuration

`New` validates `Config` and applies safe defaults; `WorkDir` is required.
Defaults: `Quality` `"best"`, `Concurrency` `4`, `MaxItems` `1000`. `MaxItems`
is the hard upper bound applied to a request's `Limit`, so a hosted caller
always gets bounded work even if a client sends an unbounded request.

The facade deliberately does **not** expose raw yt-dlp arguments, output
templates, proxy addresses, browser executable paths, or arbitrary headers.

## Lifecycle and concurrency

- Construct one `Engine` with `New` and reuse it. It holds no package globals
  and is safe for concurrent use.
- Each `Resolve`/`Download` call is independent; a shared per-host rate limiter
  is the only cross-call state, by design.
- `Config.WorkDir` is engine-owned scratch for the duration of a call. A hosted
  caller should use a fresh, dedicated directory per operation so concurrent
  jobs cannot mix.

## Cancellation

Every call accepts a `context.Context`. A context that is already done before
the call starts, or that becomes done during it, yields an error whose category
is `ErrorCanceled` wrapping the context error. `Config.Timeout`, when positive,
bounds each call.

## Events

Progress is reported through an `EventSink` as structured, presentation-neutral
events (kind, stage, URL, item id, byte counts, attempt, stable reason codes).
Terminal formatting lives only in the CLI/TUI adapters. A nil sink is safe: the
engine substitutes a discarding sink and never falls back to terminal output on
the converted execution paths.

Sinks must be best-effort, must not block the engine for long periods, and must
not influence or cancel the operation. The engine delivers events through an
internal bounded, panic-recovering notifier, so a slow or misbehaving sink drops
events rather than stalling extraction.

> Residual limitation: the native Instagram, X/Twitter, Reddit, Album, and
> browser-fallback extractors still write some narration directly to the
> terminal; typing and event conversion of those paths continues in Phases 4–5.
> The generic yt-dlp path and the top-level download coordination are fully
> event-driven.

## Artifacts

`Download` reports every regular file written under `Config.WorkDir` as an
`Artifact`, excluding the engine's own `_provenance_cache` and `.provenance`
bookkeeping directories and in-progress `.part` files. `Path` is absolute;
`SHA256`, `Size`, and `MIMEType` are computed for the caller. The caller owns
the files after the call returns (typically: upload, then delete).

A streaming `ArtifactSink` that avoids a shared filesystem is deferred to a
later phase.

## Errors

All failures are returned as `*engine.Error` with a stable `ErrorKind`,
reachable with `engine.ErrorKindOf` or `engine.IsErrorKind` and
`errors.As`/`errors.Is`. Categories: `validation`, `authentication_required`,
`unsupported_source`, `rate_limited`, `temporary`, `canceled`, `external_tool`,
and `permanent`. Cancellation is never re-categorized. Unknown causes map to
`temporary` (retryable by default).

## External tools

The generic download path requires the yt-dlp executable; audio-only extraction
and stream merging require ffmpeg; browser fallback requires Chrome/Chromium.
Hosted deployments should provision these tools ahead of time. The engine
auto-installs yt-dlp on first use.

## Verifying the public surface

```bash
go doc github.com/sk3y04/provenance-engine/engine
make engine-example   # compiles a standalone external-consumer module
```