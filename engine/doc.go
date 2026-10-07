// Package engine is the stable, importable facade over the provenance engine.
//
// It exposes a deliberately small API for resolving and downloading supported
// public sources without any CLI or terminal dependency. The CLI and TUI are
// adapters over the same internals a server-side worker can call in-process.
//
// # Scope
//
// The first release supports exactly two operations:
//
//   - Resolve discovers a source and its items without downloading.
//   - Download resolves as needed and writes artifacts under Config.WorkDir.
//
// Archive, vault, collection, encode, and import functionality remain
// CLI-local until a private consumer needs them.
//
// # Lifecycle
//
// Construct an Engine once with New and reuse it. Engine holds no package-level
// state and is safe for concurrent use by multiple goroutines; each call to
// Resolve or Download is independent. A per-host rate limiter is shared across
// calls, which is intentional. Operation-level parallelism is bounded by
// Config.Concurrency inside the engine.
//
// # Cancellation
//
// Every operation takes a context.Context and honors cancellation. If the
// context is already done before an operation starts, or becomes done while it
// runs, the returned error is categorized as ErrorCanceled and wraps the
// context error. Config.Timeout, when positive, bounds each call.
//
// # Events
//
// Callers observe progress through an EventSink. Events carry stable,
// presentation-neutral facts (kind, stage, URL, item id, byte counts, attempt,
// reason codes) rather than preformatted UI text. Passing a nil sink is safe:
// the engine substitutes a discarding sink and never writes to stdout/stderr
// from the converted execution paths.
//
// A sink must not block the engine for long periods, must not influence or
// cancel the operation, and may receive events concurrently from worker
// goroutines. The engine delivers events through an internal bounded,
// panic-recovering notifier, so a slow or misbehaving sink drops events rather
// than stalling extraction.
//
// # Temporary-file ownership
//
// Config.WorkDir is engine-owned scratch while an operation runs. Download
// reports every regular file written underneath it (except its own
// `_provenance_cache` and `.provenance` bookkeeping directories, and in-progress
// `.part` files) as an Artifact. Paths are absolute and owned by the caller
// after the call returns; a hosted caller should upload then delete them and
// use a fresh WorkDir per operation to avoid mixing concurrent jobs.
//
// # External tools
//
// The generic download path requires the yt-dlp executable; audio-only
// extraction and stream merging additionally require ffmpeg; browser fallback
// requires Chrome/Chromium. The engine auto-installs yt-dlp on first use, but
// hosted deployments should provision these tools ahead of time and set
// Config.ChromePath when needed. Tests in this package never contact a live
// social-media service and never invoke these tools.
package engine
