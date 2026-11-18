# CLI Reference

Full command and flag reference for `provenance`.

## Command Tree

```
provenance
├── grab <URL...> [flags]            Grab media
├── collect                          Manage named collection syncs
│   ├── add <NAME> <URL> [flags]     Add or update a collection
│   ├── list                         List collections
│   ├── show <NAME>                  Show collection details
│   ├── sync [NAME]                  Sync one or all collections
│   └── remove <NAME>                Remove a collection
├── scan <URL...> [flags]            Preview items without downloading
├── doctor [flags]                   Check prerequisites (tools, cookies, cache, DB)
├── status <SESSION>                 Show session progress
├── resume <SESSION>                 Resume pending + failed URLs
├── retry-failed <SESSION>           Retry only failed URLs
├── sessions                         Manage reservations
│   ├── list                         List all sessions
│   ├── export <SESSION> <FILE>      Export session to JSON file
│   ├── clean <SESSION>              Delete a session
│   └── failed <SESSION> [FILE]      Show or save failed URLs
├── watch                            Manage subscriptions
│   ├── add <NAME> <URL> [flags]     Add or update a watch
│   ├── list                         List watches
│   ├── run [NAME] [flags]           Run once, or repeat with --every
│   └── remove <NAME>                Remove a watch
├── history                          List and inspect past runs
│   ├── list                         List recent runs (newest first)
│   ├── show <ID>                    Show one run (ID or unique prefix)
│   ├── delete <ID>                  Delete one run
│   └── clear --yes                  Delete all runs
├── manifest                          Show and verify capture manifests
│   ├── show <PATH>                   Display a capture manifest
│   └── verify <DIR>                  Verify files against SHA-256 hashes
├── archive                            Preserve content in the durable vault
│   ├── url <URL>                      Archive a single URL
│   ├── collection <NAME>              Archive a collection's content
│   ├── session <NAME>                Archive a session's downloads
│   ├── import <DIR>                  Import a directory with capture manifests
│   ├── import-pdf <PATH>             Import a PDF into the vault
│   ├── import-git <URL>              Import a Git repository
│   ├── import-docs <URL>             Import a static documentation site
│   └── import-openapi <PATH>         Import an OpenAPI specification
│   └── import-web <URL>               Archive a web page via headless Chrome
├── vault                               Manage the durable vault
│   ├── init                            Initialize vault + PostgreSQL schema
│   ├── show <ID>                       Show revision details
│   ├── cite <ID>                       Generate citation for a revision
│   ├── diff <IDA> <IDB>                Diff two revisions
│   ├── gc                              Garbage-collect orphaned blobs
│   ├── retention prune                 Prune old revisions (--collection, --keep)
│   └── backup                          Backup the vault directory
├── search <QUERY>                      Full-text search across archived content
├── install                           Pre-install yt-dlp + ffmpeg
├── tui                              Launch interactive terminal UI
├── encode [flags]                   Transcode local videos to AV1 (hardware-accelerated)
├── torrent                          Selective BitTorrent fetch into staging
│   ├── inspect <SOURCE>             List files without downloading payload
│   ├── fetch <SOURCE> --select PATH Download only the selected files
│   └── status [JOB_ID]              Recorded job state (read-only, no lock)
└── completion <bash|zsh|fish|powershell>
```

---

## `provenance grab <URL...> [flags]`

Download media from one or more URLs.

### URL treatment

- Individual positional URLs are dispatched **sequentially**.
- URLs from `--batch <file>` are dispatched **in parallel** (worker pool sized by `--concurrency`).
- Within batch, Patreon/Generic URLs use a yt-dlp worker pool. Twitter/Reddit URLs fall back to sequential dispatch (their extracts handle internal parallelism).

### Twitter/X hashtag sources

`grab` (and `scan`) accept native X hashtag searches in addition to profile URLs:

| Form | Example | Notes |
|------|---------|-------|
| Canonical shorthand | `x:#golang` | Recommended CLI syntax |
| Bare hashtag | `#golang` | **Must be quoted** — an unquoted `#` starts a shell comment |
| Hashtag URL | `https://x.com/hashtag/golang` | Also `twitter.com` |

All forms normalize to the canonical identity `x:#golang`. Cookies are
required (the search uses X's private web GraphQL API). Media is written under
`twitter/hashtags/<slug>/{images,videos,posts}`. A `--limit` is strongly
recommended for high-volume tags.

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-o`, `--output` | string | `./downloads` | Output directory (created if missing) |
| `-c`, `--cookies` | string | - | Path to Netscape `cookies.txt` file |
| `--cookies-from-browser` | string | - | Browser name for yt-dlp's built-in cookie extraction: `chrome`, `edge`, `firefox`, `brave`, `opera`, `vivaldi`, `chromium` |
| `--concurrency` | int | `4` | Max parallel HTTP downloads and yt-dlp workers |
| `--quality` | string | `best` | Video quality: `best`, `1080`, `720`, `480` |
| `--audio-only` | bool | `false` | Extract audio only. Outputs MP3 320kbps. Requires ffmpeg. |
| `--dry-run` | bool | `false` | Print what would be downloaded without actually downloading |
| `--session` | string | - | Save progress as a named resumable session. Incompatible with `--dry-run`. |
| `--no-archive` | bool | `false` | Disable all archives (URL archive + yt-dlp download archive). Re-attempt every URL. |
| `--batch` | string | - | Path to a text file with one URL per line. `#` lines are comments. |
| `--include` | string | - | Comma-separated extensions to include: `mp4,jpg,zip` |
| `--exclude` | string | - | Comma-separated extensions to exclude: `psd,zip,rar` |
| `--min-size` | string | - | Minimum known file size: `10MB`, `1GB` |
| `--max-size` | string | - | Maximum known file size: `2GB`, `500KB` |
| `--title-match` | string | - | Regex - only include items where title, filename, or URL matches |
| `--title-exclude` | string | - | Regex - exclude items where title, filename, or URL matches |
| `--limit` | int | `0` (unlimited) | Twitter/Reddit/Instagram: max number of posts to fetch. For Twitter/X hashtag search, caps discovered search posts (0 = unlimited) |
| `--layout` | string | `creator` | Output layout preset (see below) |
| `--filename-template` | string | - | Advanced yt-dlp output template. Overrides `--layout`. Relative to `--output`. |
| `--speed-limit` | string | - | Limit download speed: `5MB`, `500KB`, `1M` |
| `--include-posts` | bool | `false` | Download post text, links, and metadata as markdown. Twitter/Reddit/Instagram. |
| `--include-comments` | bool | `false` | Download post comments/replies (Reddit) |
| `--max-comments` | int | `100` | Maximum number of comments to fetch per post |

### Output layout presets

| Value | yt-dlp template | Result |
|-------|----------------|--------|
| `creator` (default) | `%(extractor)s/%(uploader)s/%(title)s.%(ext)s` | `youtube/ChannelName/Video Title.mp4` |
| `site` | `%(extractor)s/%(title)s.%(ext)s` | `youtube/Video Title.mp4` |
| `flat` | `%(title)s.%(ext)s` | `Video Title.mp4` |
| `date` | `%(upload_date>%Y-%m)s/%(title)s.%(ext)s` | `2025-07/Video Title.mp4` |

When `--filename-template` is set, it takes precedence. If the template is absolute, the `--output` directory is ignored.

### Quality behavior

| Quality | ffmpeg present | ffmpeg missing |
|---------|---------------|----------------|
| `best` | `bestvideo+bestaudio/best` (merge → mp4 + thumbnail) | `best[ext=mp4]/best` (pre-muxed only) |
| `1080` | `bestvideo[height<=1080]+bestaudio/best[height<=1080]` | `best[height<=1080][ext=mp4]/...` |
| `720` | same pattern, height ≤720 | same pattern |
| `480` | same pattern, height ≤480 | same pattern |

### Speed limit format

Accepts suffixes: `B`, `KB`/`K`, `MB`/`M`, `GB`/`G`. Case-insensitive. Examples: `5MB`, `500KB`, `1G`. Passed to yt-dlp as `--limit-rate`.

### Terminal summary

After every grab, provenance prints a structured summary to stderr:

```
discovered: 240
downloaded: 226
skipped:     10
failed:       2
output:     ./downloads
```

When `--session` is used, an additional `session:` line appears. A detailed session status follows for session-backed runs.

### Engine facade path

A plain `grab` invocation (http/https URLs or supported X/Twitter hashtag sources, with no `--dry-run`, `--filename-template`, `--batch`, or `--session`) is executed through the public `engine` facade — the same `Engine.Download` a server-side worker calls in-process. Progress is delivered as structured events and rendered to stderr by the CLI adapter; the summary above is emitted from the aggregated typed result.

The legacy internal path remains for options the public facade deliberately does not expose (`--dry-run`, `--filename-template`, `--batch`, `--session`), for non-standard `--quality` values, and for bare non-http sources. This is a documented transitional boundary; see [`ENGINE_REFACTOR.md`](ENGINE_REFACTOR.md).

---

## `provenance scan <URL...> [flags]`

Preview downloadable items without downloading.

### Flags

All `grab` flags apply, plus:

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool | `false` | Print resolved source data as formatted JSON to stdout (see `resolve.Source` format) |
| `--save` | string | - | Save scan manifest JSON to a file |

---

## `provenance doctor [flags]`

Check that provenance's prerequisites are installed and configured. The probe
is read-only (apart from creating its own cache directory and a short database
ping), so it is safe to run at any time.

| Check | Required | Meaning |
|-------|----------|---------|
| `yt-dlp` | yes | resolved from PATH or the go-ytdlp cache |
| `ffmpeg` | yes | resolved from PATH |
| `ffprobe` | yes | resolved from PATH |
| `chrome` | no | browser fallback and `import-web` |
| `cookies` | no | the `--cookies` file, if given, exists and is a regular file |
| `cache` | yes | the provenance cache directory exists and is writable |
| `database` | no | pings `PROVENANCE_DATABASE_URL` when set (host shown, never the password) |

The exit code is non-zero when a required check fails, so it can gate a
deployment or CI step. It does not trigger tool auto-install (the point is to
report state, not fix it).

```bash
$ provenance doctor
provenance doctor
  yt-dlp    ok              /home/anima/.local/bin/yt-dlp
  ffmpeg    ok              /usr/bin/ffmpeg
  ffprobe   ok              /usr/bin/ffprobe
  chrome    ok              /usr/bin/google-chrome
  cookies   not_configured  not set
  cache     ok              /home/anima/.cache/provenance
  database  not_configured  not configured (set PROVENANCE_DATABASE_URL for vault/search)
result: ok
```

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-c`, `--cookies` | string | - | Path to Netscape `cookies.txt` to verify exists |

`--chrome-path` (global) also applies.

---

## `provenance status <SESSION>`

Show progress for a saved session: total/succeeded/skipped/pending/running/failed counts, last update, and failed URL list (up to 20).

---

## `provenance resume <SESSION>`

Resume all pending and failed URLs in a session. Resets any stuck `running` entries to `pending`. If nothing remains, prints status and exits.

---

## `provenance retry-failed <SESSION>`

Retry only the failed URLs in a session. Existing pending URLs are left untouched.

---

## `provenance sessions`

| Subcommand | Description |
|------------|-------------|
| `list` | Table of all sessions: name, updated, total, OK, failed, pending. Sorted by most recent first. |
| `export <SESSION> <FILE>` | Copy the session JSON file to the given path |
| `clean <SESSION>` | Delete a session file from disk |
| `failed <SESSION> [FILE]` | Print failed URLs to stdout, or save to file |

---

## `provenance collect`

Named collection sync sources. A collection is a saved source URL with its own configuration, seen-item tracking, and incremental sync support. Each sync run scans the source, skips already-captured items, downloads new content, and records the result.

| Subcommand | Description |
|------------|-------------|
| `add <NAME> <URL> [flags]` | Create or update a collection. Accepts all `grab` flags for per-collection config. |
| `list` | Table of all collections: name, last sync, total/new/fail, URL. |
| `show <NAME>` | Full details: URL, site, output, created, last sync, seen count, last result. |
| `sync [NAME]` | Scan the source, skip seen items, download new content. `--all` to sync all collections. `--dry-run` to preview without downloading. `--record` to save capture manifests with SHA-256 hashes. `--every 24h` for repeating sync. |
| `remove <NAME>` | Delete a collection. |

### Incremental sync

Collections track seen item IDs (post IDs, video IDs) in `~/.cache/provenance/collections.json`. Each `collect sync` run:

1. Scans the source URL (discover all items without downloading)
2. Compares discovered item IDs against the seen set
3. Downloads only new items via a resumable session (`collect-<name>-<date>`)
4. Records the sync result (total, new, skipped, failed)

Repeated runs only pick up new content  -  a Reddit subreddit with 1000 wallpapers scans immediately and downloads nothing if no new posts have appeared since the last sync.

### --dry-run

```
provenance collect sync wallpapers --dry-run
```

Shows what would be synced without downloading: total items discovered, how many are new, how many skipped.

### --record

```
provenance collect sync wallpapers --record
```

Writes capture manifests to `<outputDir>/.provenance/`:

```
~/.provenance/
  collection.json                  ← collection config at sync time
  runs/
    2026-07-26T230000Z.json        ← per-sync CaptureManifest
  items/
    abc123.json                    ← per-item record with SHA-256, paths, metadata
```

Each item manifest records: external ID, URL, title, author, publication time, capture time, downloaded path, byte size, SHA-256 hash, extractor metadata, post text, and session name. Use `provenance manifest verify <dir>` to check file integrity.

### --every

```
provenance collect sync wallpapers --every 24h
```

Runs the sync in a loop at the specified interval (e.g., `24h`, `7d`, `30m`). Ctrl+C to stop. Useful as a simple daemon replacement  -  no cron required.

---

## `provenance manifest`

Show and verify capture manifests recorded via `collect sync --record`.

| Subcommand | Description |
|------------|-------------|
| `show <PATH>` | Pretty-print a capture manifest JSON file |
| `verify <DIR>` | Verify all files in a `.provenance/items/` directory against recorded SHA-256 hashes |

### verify

```
provenance manifest verify ~/Media/Reddit/Wallpapers
```

Reads every `.json` file in `<dir>/.provenance/items/`, computes SHA-256 of the referenced file on disk, and reports matches, mismatches, and missing files.

```
FAIL     ~/Media/Reddit/Wallpapers/reddit/username/abc.jpg
  expected: a1b2c3...
  actual:   d4e5f6...

42 ok, 1 failed, 0 missing
```

---

## `provenance archive`

Opt-in durable vault. Archives captured content into an immutable, content-addressed blob store with versioned revisions. Never overwrites prior captures.

There are two paths into the vault:

- **Download-then-archive** (`url`, `collection`, `session`, `import`): Copy content already on disk (downloaded via `grab` or `collect sync --record`) into the vault with SHA-256 verification. These commands do not download anything themselves.
- **Import-direct** (`import-pdf`, `import-git`, `import-docs`, `import-openapi`, `import-web`): Download AND archive in one step — no `grab` or `collect` needed.

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--collection` | string | *(required)* | Archive collection name |
| `--vault` | string | `./provenance-vault` | Vault root directory |

### Subcommands

| Command | Description |
|---------|-------------|
| `url <URL>` | Download a URL, then archive it with capture metadata |
| `collection <NAME>` | Archive all captured content from a named collection |
| `session <NAME>` | Archive all downloads from a session |
| `import <DIR>` | Import an existing directory that already has `.provenance/` capture manifests |
| `import-pdf <PATH>` | Import a PDF file  -  extracts text per page, stores as searchable documents |
| `import-git <URL>` | Import a Git repository  -  shallow clone, extract doc files, resolve tag→commit |
| `import-docs <URL>` | Import a static documentation site  -  crawl bounded by scope and max-pages |
| `import-openapi <PATH>` | Import an OpenAPI spec  -  generate endpoint documents, store original spec |
| `import-web <URL>` | Archive a web page or site via headless Chrome  -  extracts HTML, text, discovers same-scope links, bounded by `--max-pages` |

### Importer flags

| Flag | Type | Default | Applies to | Description |
|------|------|---------|-----------|-------------|
| `--ref` | string | `main` | `import-git` | Git branch or tag to clone |
| `--scope` | string | - | `import-docs` | URL path prefix for crawl scope |
| `--max-pages` | int | `10` | `import-docs`, `import-web` | Maximum pages to crawl |
| `--screenshot` | bool | `false` | `import-web` | Capture page screenshots |

### Vault layout

```
<vault-root>/
  collections.json                  ← archive collection definitions
  blobs/
    sha256/
      ab/
        abcdef123...                ← content-addressed files (deduplicated)
  revisions/
    <revision-id>/                  ← immutable, id = SHA-256 of manifest
      manifest.json                 ← revision metadata + source + tool
      entities.jsonl                ← one Entity per line
      artifacts.jsonl               ← one Artifact per line
      documents.jsonl               ← one Document per line
      relations.jsonl               ← one Relation per line
  staging/
  exports/
```

### Example

```bash
# Sync with capture manifests
provenance collect sync wallpapers --record
# Archive the captured content
provenance archive collection wallpapers --collection visual-refs --vault ~/Vault
# → verifies SHA-256, copies to blob store, creates immutable revision
# → "archived revision abc123... (226 entities)"
```

---

## `provenance torrent`

Selective BitTorrent fetch. A source is a v1 magnet link (`urn:btih`) or a local `.torrent` file. There is no "download everything" mode: `fetch` requires at least one `--select` path, and an unknown path is an error rather than a silent whole-torrent download.

Downloads are verified against the torrent's piece hashes, then written to a capture manifest in the same format `collect sync --record` produces, so `manifest verify` and `archive import` work on the result unchanged:

```bash
provenance torrent inspect "magnet:?xt=urn:btih:..."      # list selectable paths (metadata only)
provenance torrent fetch "magnet:?xt=urn:btih:..." \
  --select "release/track.flac" --select "release/cover.jpg" \
  -o ./downloads/release --max-bytes 2GB --timeout 30m
provenance manifest verify ./downloads/release            # 2 ok, 0 failed, 0 missing
provenance archive import ./downloads/release --collection music --vault ~/Vault
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-o`, `--output` | string | `./downloads` | Staging root for `fetch` and `status`; `inspect` does not use it |
| `--select` | string (repeatable) | *(required for fetch)* | Exact file path from `inspect`; commas also accepted |
| `--peer` | string (repeatable) | - | Explicit `host:port` peer, for private swarms |
| `--max-bytes` | size | `0` (no limit) | Reject a selection larger than this before downloading |
| `--timeout` | duration | `0` (no limit) | Overall deadline for the fetch |
| `--seed` | bool | `false` | Keep seeding after capture until interrupted |
| `--json` | bool | `false` | Machine-readable output for `inspect`, `fetch` and `status` |

### `torrent inspect`

Metadata only, and job-free: it takes no staging lock, creates no payload directory, piece-completion database or job record, and persists no metainfo.

- A local `.torrent` is parsed in memory — no client is constructed, nothing is dialled, no temporary directory is created.
- A magnet is resolved through a short-lived client bounded by `--timeout` (default two minutes), with payload storage deferred and both data directions disabled, so no piece is requested or served. The client and its private temporary directory are closed and removed on success, error, timeout and cancellation.
- v2-only and hybrid metainfo are refused with `unsupported torrent identity`, and `urn:btmh` magnets with `unsupported torrent source`. Only v1 torrents are accepted, because selection and capture are defined over v1 file offsets.
- `--peer` and `--timeout` apply; `-o`, `--max-bytes` and `--seed` do not.

Inspection can run while a fetch owns the same staging root.

### `torrent status`

Read-only view of the job records in a staging root. Works while another process owns the root, and after a crash. It never takes the writer lock, opens the piece database, starts a client, reads payload bytes, creates directories, or rewrites old records.

```bash
provenance torrent status -o ./downloads/release           # every recorded job
provenance torrent status -o ./downloads/release bt-<infohash>
provenance torrent status -o ./downloads/release --json
```

```
[provenance] 1 job(s) in /home/you/downloads/release
[provenance] recorded state from each job's last update; not a liveness check

bt-94158d67ef6b15e5e8c80798996ff59369721f72  payload=ready capture=committed seeding=seeding
  infohash   94158d67ef6b15e5e8c80798996ff59369721f72
  files      fixture/c.bin
  updated    2026-11-18T14:44:56Z (recorded)
  manifest   .provenance/runs/2026-11-18T144456Z.json
```

With a job id the fields are shown one per line, including `payload verified …`, `capture committed …`, `manifest`, `capture key`, and the on-disk `record format`.

**Everything is recorded state**, not a probe: `seeding` can be stale, `updated_at` is the last record change rather than a heartbeat, and a leftover lock file is ignored. Status reads one file per job and reports the capture manifest path as recorded — it does not check that the manifest still exists or is intact.

`--json` (list form; one job is emitted as a single object):

```json
{
  "staging_root": "/home/you/downloads/release",
  "complete": true,
  "jobs": [
    {
      "job_id": "bt-94158d67ef6b15e5e8c80798996ff59369721f72",
      "backend": "bittorrent",
      "info_hash": "94158d67ef6b15e5e8c80798996ff59369721f72",
      "staging_ref": "fixture",
      "selection": ["fixture/c.bin"],
      "created_at": "2026-11-18T14:40:02Z",
      "updated_at": "2026-11-18T14:44:56Z",
      "recorded_state": { "payload": "ready", "capture": "committed", "seeding": "seeding" },
      "payload": {
        "verified_at": "2026-11-18T14:44:55Z",
        "files": 1,
        "assets": [
          { "path": "fixture/c.bin", "length": 18000, "sha256": "…64 hex…" }
        ]
      },
      "capture": {
        "committed_at": "2026-11-18T14:44:56Z",
        "manifest": ".provenance/runs/2026-11-18T144456Z.json",
        "capture_key": "…64 hex…"
      },
      "record_format": "provenance/torrent-job+2"
    }
  ]
}
```

`jobs` is sorted by job id and is `[]` when the root has no jobs. A record that cannot be read appears in `"unreadable": [{"file": …, "reason": …}]` instead of being dropped; the document stays valid JSON, a warning goes to stderr, and the command exits 1 because the listing is incomplete. `"complete"` carries the same fact in-band, so a script that only parses stdout can tell a full listing from a partial one without depending on the exit code.

Records are validated on read: unsafe paths in a record, a mismatched file name, an unsupported `format`, or a record that is not a regular file (a symlink planted in the staging tree) make it unreadable rather than trusted. `staging_root` is the resolved absolute path, so two spellings of one root — including a symlinked one — produce identical output. Status output never includes the magnet source, metainfo, tracker URLs, credentials or passkeys.

`payload.assets` is the recorded attestation only (path, length, SHA-256). Status does not re-hash those files; `provenance manifest verify` does.

| Case | Exit code |
|------|-----------|
| jobs listed, or root empty, or job found | 0 |
| root missing / not a directory | 1 |
| job id missing (`torrent job not found`) or malformed (`invalid torrent job id`) | 1 |
| any record unreadable (listing incomplete) | 1 |

### Staging layout

```
<staging>/
  <torrent name>/<path>            ← downloaded files (final names only)
  .provenance/runs/<ts>.json       ← capture manifest (shared format)
  .provenance/items/<id>.json      ← per-file capture item
  .provenance/torrent/<infohash>.torrent   ← exact metainfo, persisted once
  .provenance/torrent/bt-<infohash>.job.json ← job record (immutable selection)
  .torrent.db                      ← library piece-completion database (not captured)
```

### Behaviour notes

- **Selection is not a guarantee about disk contents.** BitTorrent pieces can span file boundaries, so fetching one file may write neighbouring bytes (a shared edge piece). Only the selected, complete, hash-verified files are captured.
- **Resume.** A second run in the same staging root re-verifies retained pieces before requesting more, so verified bytes are reused and corrupted retained data is re-fetched instead of captured. Persisting the metainfo means a restart needs no peer to begin verifying.
- **Durable job state.** Each torrent gets a job record with separate payload, capture and seeding states. A capture failure can be retried from the verified staged bytes without contacting the swarm (reported as `payload=ready capture=pending`), and a restart after a capture was committed recognises the existing capture by its idempotency key instead of recording a duplicate (`existing capture reconciled`). The state line is printed after every fetch and included in `--json`.
- **One owner per staging root.** A staging root is owned exclusively by one process for as long as its `fetch` runs, including while `--seed` continues serving after the capture is committed. A second `torrent fetch` against the same root—through any spelling of the path—fails with `staging root is busy … retry after it exits` and writes nothing. `torrent inspect` and `torrent status` do not take the lock, so both work while a fetch owns the root. Different `--output` roots run concurrently. The lock is released when the command exits, and by the kernel if the process is killed.
- **Selection is immutable per job.** Re-running with a different `--select` set is refused (`ErrSelectionLocked`); use a new staging root.
- **Uploads while fetching** are enabled (normal reciprocity). Continued seeding after completion is opt-in via `--seed`.
- **No network surface.** The adapter opens no listener for other tools, exposes no RPC, and has no aria2-compatible interface.
- Paths from metainfo are treated as hostile: traversal, absolute paths, separators, reserved device names, and symlinks inside staging reject the whole torrent.
- Torrent fetches never invoke yt-dlp.

See [TORRENT.md](TORRENT.md) for the pinned library version, the security advisory review, and the dependency/licence position.

---

## `provenance vault`

Manage the durable archive vault. Requires PostgreSQL (`PROVENANCE_DATABASE_URL` env var).

| Subcommand | Description |
|------------|-------------|
| `init` | Initialize the vault and create PostgreSQL schema (tables, GIN indexes, tsvector triggers) |
| `show <ID>` | Show revision details: entities, artifacts, timestamps |
| `cite <ID>` | Generate a citation for a revision entity |
| `diff <ID_A> <ID_B>` | Diff two revisions  -  reports added, removed, and changed entities with field-level diffs |
| `gc` | Garbage-collect orphaned blobs. `--dry-run` previews without deleting |
| `retention prune` | Prune old revisions keeping the last N (`--collection`, `--keep` required) |
| `backup` | Print backup guidance for vault directory |

### cite flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--collection` | string | - | Archive collection name |
| `--entity` | string | (first entity) | Entity external ID to cite |
| `--format` | string | `plain` | Citation format: `plain`, `markdown`, `json` |

### Example

```bash
export PROVENANCE_DATABASE_URL=postgres://user:pass@localhost/provenance
provenance vault init
provenance vault show abc123def456
provenance vault cite abc123def456 --entity my-post --format markdown
```

---

## `provenance search`

Full-text search across archived content. Uses PostgreSQL `tsvector` + `tsquery` with GIN indexes. Searches entity titles, authors, post text, captions, and document content.

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--collection` | string | - | Filter by archive collection name |
| `--kind` | string | - | Filter by entity kind (video, image, post) |

### Example

```bash
provenance search "sunset mountains" --collection visual-refs
```

Results show ranked hits with title, headline snippet, source URL, collection, revision ID, and capture date.

---

## `provenance watch`

| Subcommand | Description |
|------------|-------------|
| `add <NAME> <URL> [flags]` | Create or update a watch subscription. Accepts all `grab` flags. |
| `list` | Table of watches: name, last run (or "never"), URL |
| `run [NAME] [flags]` | Run one watch (or all if NAME omitted). Each creates a `watch-<name>` session and downloads just the subscription URL. With `--every <interval>` (e.g. `24h`, `30m`), the run repeats every interval until interrupted (Ctrl-C exits cleanly; each completed tick records a history entry and updates the watch's last-run time). |
| `remove <NAME>` | Remove a watch subscription |

Watch runs use yt-dlp/native archives to skip previously-downloaded content.

### `watch run --every`

Repeat the run at a fixed interval instead of a single pass:

```bash
provenance watch run feeds --every 24h   # re-run forever every 24h
provenance watch run --every 30m         # all watches, every 30m
```

- Interval syntax: Go duration (`30m`, `24h`, `7d`…); `0`/empty = one-shot.
- The first run happens immediately, then on each tick; per-run errors are
  reported and do not stop the loop.
- Ctrl+C exits cleanly (in-flight run cancelled, watch state marked).
- Every run records a `history` entry (see below).

---

## `provenance history`

List and inspect past runs. Every run that goes through `app.Download` — CLI
`grab`, `watch run` (including `--every` repeats), and `collect sync` — records
one entry; dry runs are not recorded. Stored under the user cache dir
(`$XDG_CACHE_HOME/provenance/history.json`).

| Subcommand | Description |
|------------|-------------|
| (none) / `list` | Table of runs: ID, finished time, ok/fail/skip counts, title |
| `show <ID>` | Full details; ID may be a unique prefix |
| `delete <ID>` | Delete one run (ID or unique prefix) |
| `clear --yes` | Delete all runs; refuses without `--yes` |

```bash
provenance history                 # same as list
provenance history show 20261112-214723
provenance history clear --yes
```

`show` prints the run's title, start time, duration, ok/fail/skip counts,
total bytes, output directory, error text (if any), URLs, and per-file
results when recorded (the TUI records per-file details; plain CLI runs
record URL-level results).

---

## `provenance install`

Download yt-dlp and ffmpeg into the local cache. Normally runs automatically on first `grab`/`scan`. Useful for pre-warming a deployment image.

Note: ffmpeg is downloaded by go-ytdlp into the cache but may not be on PATH. The yt-dlp wrapper passes the cache path explicitly via `--ffmpeg-location`.

---

## `provenance tui`

Launch the interactive terminal UI. No flags. Full reference in [`docs/TUI.md`](TUI.md).

---

## `provenance completion <shell>`

Generate shell completion for one of: `bash`, `zsh`, `fish`, `powershell`.

```bash
provenance completion bash | sudo tee /etc/bash_completion.d/provenance
provenance completion zsh > "${fpath[1]}/_provenance"
provenance completion fish > ~/.config/fish/completions/provenance.fish
provenance completion powershell | Out-String | Invoke-Expression
```

---

## `provenance encode [flags]`

Batch-convert local video files to hardware-accelerated AV1 in an `.mkv` container. Inputs are scanned from a directory (or a single `--file`), transcoded through a GPU backend (Intel QuickSync, AMD AMF, or NVIDIA NVENC), and written as AV1 — audio/subtitle streams and container metadata are copied. Outputs are written in the same directory as the source files unless `--out` is set, and no log files are written. Work is fanned out across one worker pool per requested device, so multiple GPUs can encode in parallel.

Encoder selection probes your installed `ffmpeg`: `auto` picks the first available hardware AV1 codec (QSV preferred); pass `--encoder qsv|amf|nvenc` to pin one. An explicit encoder not present in the build is rejected with a `not supported by this ffmpeg build` error. Requires `ffmpeg` and `ffprobe` on `PATH` (the latter for per-file frame-rate detection).

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dir` | string | - | Source directory to scan for videos (alternative to `--file`) |
| `--file` | string | - | Single input video file (alternative to `--dir`) |
| `--recursive` | bool | `false` | Recurse into subdirectories |
| `--ext` | string | `mp4,mkv,mov,avi,ts,webm` | Comma-separated input extensions to match (no leading dot) |
| `--out` | string | source directory | Output directory (default: the source directory itself) |
| `--suffix` | string | `av1-qN` | Suffix inserted before `.mkv` in output names |
| `--overwrite` | bool | `false` | Re-encode even if the output already exists |
| `--remove-original` | bool | `false` | Delete each source file after its encode succeeds (never with `--dry-run`) |
| `--encoder` | string | `auto` | AV1 backend: `qsv` (Intel), `amf` (AMD), `nvenc` (NVIDIA), or `auto` |
| `--devices` | string | auto-detected | Comma-separated render nodes (QSV/AMF) or GPU indices (NVENC). When unset, QSV pins the Intel render nodes found in `/dev/dri` (by PCI vendor), AMF pins the AMD nodes, and NVENC uses GPU index 0 |
| `--quality` | int | `30` | Quality on the canonical QSV `global_quality` scale 1..51 (20–22 near-transparent, 24–26 very good, 28–30 aggressive, 32+ archival/animation only); mapped to AMF `cq_quality` / NVENC `crf` |
| `--preset` | string | `medium` | Speed-vs-efficiency hint, mapped per backend (QSV `fast`/`slow`, NVENC `hp`/`hq`/`slow_hq`, AMF `ultrafast`/`balanced`/`maximum`) |
| `--gop-seconds` | float | `10` | Keyframe interval in seconds; converted to a frame count per file using its ffprobe-detected fps |
| `--lookahead-depth` | int | `100` | Look-ahead frames (QSV max 100; `<= 0` disables the flag) |
| `--bframes` | int | `7` | B-frames per GOP (`<= 0` disables; skipped on AMF, which lacks the concept) |
| `--dry-run` | bool | `false` | Print the constructed ffmpeg commands without encoding |

### Behavior notes

- **Quality scale** — `--quality` is expressed once on the QSV `global_quality` scale (1–51, lower = better) and translated per backend: AMF receives `-cq_quality` on the same range, NVENC receives `-crf` mapped onto 1–33.
- **Output naming** — each output is `<base>.<suffix>.mkv` (e.g. `clip.av1-q30.mkv`), written in the same directory as the source file unless `--out` is set. No log files are written. The scan skips any directory named `av1_q*` or equal to the output dir, and skips its own in-place output files (`<base>.<suffix>.mkv` inside the output dir), so re-runs do not re-encode their own output.
- **`--remove-original`** — deletes each source file only after its encode succeeds. A failed encode leaves its source in place, and `--dry-run` never removes anything.
- **Device auto-detection** — with no `--devices`, QSV pins the Intel render nodes (PCI vendor `8086`) and AMF the AMD nodes (`1002`/`1022`), read from `/sys/class/drm` and sorted by node number, so a box with multiple GPU vendors does not default to a node that cannot encode. If no node matches the vendor, every node is pinned so the failure is loud; if no render nodes exist at all, nothing is pinned and the encoder library chooses.
- **Concurrency** — one `worker.Pool` is created per device; each job is submitted with permanent-error semantics so a failed encode never overwrites another's in-progress output.
- **`--dry-run`** — prints each device's full `ffmpeg` command (shell-escaped, no shell invoked) and touches no files.
- **Event sink** — when the operation carries an `event.Sink` (`encode.Options.Events`), narration (start, per-file outcomes, summary, dry-run echo) is emitted as structured events and nothing is written to the terminal. The CLI passes no sink, so terminal output is the legacy `[provenance]` lines.

---

## Auto-install behavior

The root command's `PersistentPreRunE` calls `extractor.EnsureInstalled()` (installs yt-dlp + ffmpeg) before every subcommand **except**:

- `help`
- `install`
- `status`
- `sessions` and all subcommands
- `completion`
- `tui`
- `history`
- `watch` (for `list`, `add`, `remove` - but **not** `watch run`)

---

## Signal handling

`SIGINT` (Ctrl+C) and `SIGTERM` cancel the root context, which propagates to all workers. Running yt-dlp subprocesses are killed. Sessions saved to disk survive.

---

## Global flags

These flags are available on all commands (registered on the root command):

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--chrome-path` | string | (auto-detect) | Path to Chrome/Chromium executable. If unset, auto-detects from: `CHROME_PATH` env var, then searches PATH for `google-chrome`, `chromium`, `brave-browser`, `brave`, `brave-origin`, `chrome`, `microsoft-edge`. |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (all URLs downloaded or nothing to do) |
| 1 | Error - error message printed to stderr, plus diagnostic hint if applicable |
