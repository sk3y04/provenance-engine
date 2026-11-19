# Soulseek (slskd) integration

Provenance talks to an **external, unmodified `slskd >= 0.26.0`** over its HTTP
API. Everything that makes a Soulseek client work — the login, listen port,
upload quotas, user blacklist, the share list itself — stays in `slskd`. This
integration owns four things only: a connection profile, durable download jobs,
the import of completed files into provenance captures, and the materialisation
of curated shares.

Provenance never sends Soulseek credentials anywhere, never writes `slskd.yml`,
and never proxies a credential to a browser: there is no browser here.

## Requirements

- `slskd` **0.26.0 or newer** on a reachable host. `0.26.0` is the floor because
  durable jobs need transfer batches (`POST /api/v0/transfers/downloads/batches`).
- An API key with the `readwrite` role (an `administrator` key additionally
  enables option diagnostics).
- The `slskd` **downloads** directory and the **curated share root** shared with
  this process, on the same filesystem as the media you intend to import and
  share.

`0.26.1` and `0.26.2` work but carry two known upstream problems, reported as
amber warnings rather than blocks:

| Version | Warning | Effect |
| --- | --- | --- |
| `0.26.1`, `0.26.2` | `#1891` | A 180-second enqueue timeout can crash slskd (exit 134). Provenance reports `SLSKD_UNREACHABLE` and recovers; pinning `0.26.0` avoids it. |
| `0.26.2` | `#1905` | `GET /api/v0/options` returns `403` for a `readwrite` key. Provenance degrades and keeps working; use an `administrator` key for option diagnostics. |

## Configuration

Everything lives in one profile, written `0600`:

```
$XDG_CONFIG_HOME/provenance/slskd-settings.json   (fallback: ~/.config/provenance/)
```

```bash
provenance soulseek settings set \
  --base-url http://127.0.0.1:5030 \
  --api-key <key> \
  --slskd-downloads-root /downloads \
  --downloads-mount /srv/media/downloads \
  --curated-share-root /srv/media/soulseek-shares \
  --enable
```

`--url-base /slskd` adds a reverse-proxy prefix. `--insecure` disables TLS
verification for a self-signed proxy — off by default, deliberately.

Secrets can instead come from the environment, which is the recommended shape for
a headless deployment because nothing is written to disk:

```
PROVENANCE_SLSKD_BASE_URL, PROVENANCE_SLSKD_URL_BASE, PROVENANCE_SLSKD_API_KEY,
PROVENANCE_SLSKD_WEBHOOK_SECRET, PROVENANCE_SLSKD_DOWNLOADS_ROOT,
PROVENANCE_SLSKD_MOUNT, PROVENANCE_SLSKD_SHARE_ROOT, PROVENANCE_SLSKD_ENABLED,
PROVENANCE_SLSKD_INSECURE, PROVENANCE_SLSKD_CLEANUP
```

Read paths never reveal a secret: `provenance soulseek settings show` prints the
API key and webhook secret as `********`, and feeding that masked value back
keeps the stored secret instead of erasing it.

### Path mapping

`--slskd-downloads-root` is the download root **as slskd sees it** (`/downloads`
inside its container). `--downloads-mount` is that same directory **as this
process sees it** (`/srv/media/downloads` on the host). Downloads are pinned to
`<downloads>/provenance/<jobID>/`, so the mapping is the only thing that has to
be right.

## Health

```bash
provenance soulseek health
```

One of four states, plus non-blocking warnings:

| State | Meaning | What still works |
| --- | --- | --- |
| `HEALTHY` | slskd answers, version supported, logged in to Soulseek | everything |
| `SLSKD_UNREACHABLE` | stopped, crashed (`#1891`), timed out or network-severed | imported library, job history |
| `AUTH_OR_CONFIG_ERROR` | bad/revoked key, unsupported version, unreadable mount | job history |
| `SOULSEEK_DISCONNECTED` | slskd is up, its Soulseek session is not logged in | share views, local history, files |

`provenance soulseek test` is the same probe with a terse pass/fail, for a
monitoring check.

## Search

```bash
provenance soulseek search "beethoven symphony flac" --limit 10
```

Results are grouped per `(peer, folder)` and ranked by whether the peer has a
free upload slot, then upload speed, then queue depth, then breadth. Files a peer
has locked are counted but never offered: they cannot be downloaded without a
grant.

Searches are serialised inside one process on purpose. `slskd >= 0.24` permits one
concurrent search per instance and answers `429` to a second, so overlapping
requests would surface as a throttle error; Provenance queues them instead and
retries a transient `429` with backoff.

Two timing facts worth knowing, both verified against the running binary rather
than its documentation:

- `searchTimeout` is in **milliseconds**. The upstream Swagger and source comment
  both say seconds; `5` ends a search 53 ms after it starts.
- `slskd` resets its own timeout on every peer response, so a popular query can
  run far past `--timeout-ms`. Provenance enforces a wall-clock cap (35 s) and
  stops the search explicitly.

The search is deleted from slskd after a settle grace (`--settle`, default 5 s),
because deleting immediately after a poll can leave `search.db` in a concurrency
error that returns empty results (`#1819`).

## Download

```bash
provenance soulseek download "beethoven symphony" --peer someuser
provenance soulseek download "beethoven symphony" --peer someuser --dir "music/Beethoven"
```

The search runs again for the command, so what is enqueued is what that peer
offers right now — results are never cached between processes.

Each grab creates a durable job:

```
<job root>/.provenance/slskd/jobs/<jobID>.json   0600
```

The record is written **before** the batch is posted, so a crash in between
leaves a `QUEUED` job that `recover` can act on rather than downloads running
with no history. The batch pins both `externalId` and the destination
`provenance/<jobID>/`, which is what makes a restarted slskd correlate back.

Guardrails applied before the call, not after:

- A remote path segment over **255 UTF-8 bytes** is refused (`#1818`: such a path
  retries forever inside slskd's `incomplete/` directory).
- `..` traversal in any segment is refused.
- A file already covered by an active job is not enqueued twice; the existing job
  is returned.
- Downloads are never placed anywhere but the pinned destination.

### Job states

| State | Meaning | Next action |
| --- | --- | --- |
| `QUEUED` | recorded, batch accepted or in flight | wait |
| `IN_PROGRESS` | slskd reports a queued or moving transfer | `position`, `cancel` |
| `COMPLETED_PENDING_IMPORT` | bytes on disk, not ingested | `ingest` (or `watch`) |
| `IMPORTED` | verified capture written | — |
| `FAILED` | every transfer failed | `retry` |
| `IMPORT_FAILED` | bytes arrived, import failed; staging kept | fix, then `ingest` |
| `INTERRUPTED` | slskd forgot the job and no bytes are on disk | `retry` |
| `CANCELLED` | cancelled by the user | `retry` |

Every completed-but-not-succeeded state (`Completed, Rejected`, `Completed,
TimedOut`, `Completed, Errored`, `Completed, Aborted`, `Completed, Cancelled`) and
a bare `Cancelled` terminates the job immediately with the exact upstream reason
preserved. Nothing waits out a 180-second timeout.

### Monitoring and control

```bash
provenance soulseek jobs                  # durable history
provenance soulseek watch --interval 5s   # poll + import until interrupted
provenance soulseek position <jobID>      # per-transfer queue position (rate-limited)
provenance soulseek cancel <jobID> [--remove]
provenance soulseek retry <jobID> [--another-source]
provenance soulseek clear-completed       # slskd's records only; job history stays
```

`retry --another-source` picks the next ranked alternate from the candidates
stored with the job, so a dead peer does not require a new search.

### Restart recovery

```bash
provenance soulseek recover
```

Cross-checks every pending job against three sources: the durable record,
slskd's transfer list, and the bytes on disk.

- Transfers slskd still knows about are folded in as usual.
- A job whose transfer records expired (retention, or a `flags.volatile: true`
  restart) but whose files arrived becomes `COMPLETED_PENDING_IMPORT`.
- A job present in neither place becomes `INTERRUPTED` with a reason. Jobs
  younger than two minutes are left alone: an enqueued batch has no transfer
  records yet, and calling that interrupted would be a lie.
- **Nothing is changed when slskd is unreachable.** A network failure is not
  evidence that a download is gone.

## Import

```bash
provenance soulseek ingest [--vault <path>] [--collection soulseek] [--cleanup]
```

A completed job is imported into the existing capture pipeline, not a second
format: one `manifest.CaptureItem` per file plus one run manifest per job, with
Soulseek provenance in `raw_metadata` (`source`, `peer_username`,
`remote_filename`, `search_query`, `search_id`, `slskd_external_id`,
`slskd_transfer_id`, `job_batch_id`). With `--vault`, the capture is also ingested
into the vault and the job records the revision id.

Verification before `IMPORTED`:

- every file is inside the pinned destination (containment, not string prefix),
- symlinks in the staging tree are ignored, never followed,
- the on-disk size matches what slskd reported,
- the file is non-empty and re-hashed,
- the run manifest is written, and `manifest.VerifyCaptureDir` accepts it.

`IMPORT_FAILED` leaves the staging copy exactly as it was. `--cleanup` deletes it
**only after** a verified import, and only then clears the slskd transfer record.

A file slskd renamed on a collision (`FileService.MoveFile` appends `_{ticks}`
before the extension) is found and imported.

## Curated shares

```bash
provenance soulseek shares preview /srv/media/music/Album   # nothing linked yet
provenance soulseek shares add /srv/media/music/Album --alias Music
provenance soulseek shares list
provenance soulseek shares status
provenance soulseek shares files --page 2 --page-size 100 --filter flac
provenance soulseek shares rescan [--cancel]
provenance soulseek shares remove music
```

Sharing is explicit and previewed: the preview reports file count, bytes, an
extension histogram and what the denylist excluded. Nothing is auto-shared, and
nothing is shared because it was downloaded.

**Hardlinks only, never symlinks.** This is not cosmetic. The 0.26.x share
scanner follows symlinks and indexes their targets: in the contract spike,
symlinks to `/etc/hostname` and `/etc/shadow` placed inside a `:ro` mounted share
root were both scanned and advertised to the network. Same-filesystem hardlinks
are indexed as ordinary files and copy no bytes, so the curated root is
materialised as hardlinks and the resulting tree is audited with `Lstat` before
slskd is asked to rescan.

Defaults that follow from that:

- The curated share root must be on the same filesystem as the source; a
  cross-device link fails fast with an error naming both paths rather than
  leaving a partial share.
- The share root and the source may not contain each other.
- The denylist excludes dotfiles, `.ini`, `Thumbs.db`, `.DS_Store`, and
  `.db/.sqlite/.sqlite3/.env/.key/.pem/.yml/.yaml/.json/.pdf/.docx/.xlsx`, plus
  SSH keys and credential files.
- Mount the share root `:ro` into slskd, and never mount `/` or a home directory
  into it.

`shares status` warns when rules are active but slskd reports zero files, which is
what `flags.no_share_scan: true` (`#1663`) or a failed scan looks like from here.

## Uploads

```bash
provenance soulseek uploads list
provenance soulseek uploads cancel <username> <uploadID> [--remove]
provenance soulseek uploads clear-completed
```

Duplicate `Queued, Locally` records (`#1609`) are collapsed in the listing, keeping
the earliest request; completed uploads are never merged, because two finished
uploads of the same file are two real events.

## Optional webhooks

Webhooks are a latency optimisation only. REST polling stays authoritative:
slskd emits no search, progress or failure webhooks, and a `volatile` restart
drops them entirely.

```bash
provenance soulseek webhook-listen --addr :8791
```

The command prints the `slskd.yml` snippet to paste (with a placeholder for the
secret, so it does not end up in shell history or scrollback). The receiver:

- requires the shared secret in `X-Provenance-Webhook-Secret`, compared in
  constant time, and refuses to run at all when no secret is configured,
- deduplicates deliveries by event `id` in a bounded TTL cache,
- ignores a delivery whose timestamp is older than five minutes,
- wakes the reconciler on `DownloadFileComplete` / `DownloadDirectoryComplete`
  and the health probe on `SoulseekClientConnected` /
  `SoulseekClientDisconnected`.

## What Provenance deliberately does not do

- It does not store Soulseek credentials, set up the listen port, or configure
  upload quotas. That is `slskd`'s job.
- It does not write `slskd.yml` over HTTP. `POST /api/v0/options/yaml` would
  require `remote_configuration`, expose plaintext secrets via `GET`, and strip
  the operator's YAML comments.
- It does not speak `slskd`'s SignalR hub. There is no SSE surface either; the
  hub is undocumented and `/api/v0/events` is a paginated event log, not a stream.
- It does not modify or redistribute `slskd`. The interaction is HTTP only, which
  keeps the AGPL additional-terms question out of scope.
- It does not fall back to the legacy `POST /api/v0/transfers/downloads/{username}`
  endpoint: without a batch there is no `externalId` to pin, so a restart could
  not be reconciled.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `AUTH_OR_CONFIG_ERROR`, key is right | version below `0.26.0` | upgrade slskd |
| `AUTH_OR_CONFIG_ERROR`, mount named | mount missing/unreadable in this process | fix `--downloads-mount` (container path vs host path) |
| Warning `#1905` | `0.26.2` + `readwrite` key | expected; option diagnostics are unavailable |
| Warning `#1891` | `0.26.1`/`0.26.2` | pin `0.26.0` until a release carries `#1921` |
| `SLSKD_UNREACHABLE` mid-download | slskd crashed or restarted | `recover`, then `jobs` — nothing is lost |
| Job `INTERRUPTED` | slskd's volatile restart dropped the transfers | `retry` (or `retry --another-source`) |
| Import fails | size mismatch or missing file | inspect `jobs`, the reason is exact; staging is kept |
| Share warns `#1663` | `no_share_scan: true` | set it `false`, then `shares rescan` |
| Cross-device error on share | share root on another filesystem | put both on the same volume |