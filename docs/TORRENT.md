# Selective BitTorrent

Status: first slice. Magnet / `.torrent` input, explicit file selection, verified
capture into the existing manifest path. No UI, no RPC, no aria2 compatibility,
no queueing, no Soulseek.

## Why a native adapter

`aria2` and `transmission` demonstrate the behaviour worth having — selective
download and resumable state — but the engine already owns manifest writing,
SHA-256 verification and the archive handoff. Shelling out would duplicate those
paths and add a second source of truth for "what was downloaded". The adapter in
`internal/transfer/bittorrent` therefore implements the *behaviour* in original
Go code on top of a library that provides the peer protocol.

It is not a compatibility layer: no RPC surface, no `.aria2` control files, no
session import/export, no CLI flags copied from aria2.

## Pinned dependency

| Item | Value |
|------|-------|
| Module | `github.com/anacrolix/torrent` |
| Version | `v1.62.0` (tagged 2026-11-10) |
| Licence | MPL-2.0 |
| Pin site | `go.mod` require block (direct) |

`v1.63.0` was tagged one day before this slice was written and was not chosen: a
pin should be a version that has seen some use. `v1.61.0` and earlier are **not
acceptable** — see the advisory below.

### Security advisory review

[GHSA-2wrx-84qj-4pcg](https://github.com/anacrolix/torrent/security/advisories/GHSA-2wrx-84qj-4pcg)
/ GO-2026-6894: the bencode decoder had no nesting-depth limit, so a crafted
`.torrent` or peer-supplied metadata could overflow the stack (unrecoverable
`fatal error`, not a panic) or burn CPU quadratically in the v2 file-tree
parser.

- Affected: all versions before `v1.61.1-0.20260911233437-76452a2c8a2f`.
- Fixed by commit `76452a2c8a2f` (`bencode` depth guard).
- Verified in the pinned source: `bencode.DefaultMaxDepth = 64` and the
  `d.depth > maxDepth` check in `bencode/decode.go`.

Consequence for this slice: metainfo is only ever parsed by the library's
limited decoder, and we additionally cap local `.torrent` files at 16 MiB before
parsing. No user-supplied bencode reaches an unbounded decoder.

The advisory only covers what was reported. It is not a statement that the
library is free of other defects; the adapter therefore treats every byte from
metainfo as hostile regardless (see Safety model).

### Transitive dependency and licence census

The compile graph of `internal/transfer/bittorrent` adds 78 modules (measured
with `go list -deps`). Licence classes found by reading each module's
`LICENSE`/`COPYING` file:

| Licence | Modules |
|---------|---------|
| MIT | 40 |
| BSD (2/3-clause) | 16 |
| Apache-2.0 | 10 |
| MPL-2.0 | 8 (all `anacrolix/*`, including `torrent`, `dht/v2`, `log`, `sync`, `upnp`) |
| ISC | 3 |
| **No licence file in the module** | 1 (`github.com/go-llsqlite/adapter`) |

No GPL, LGPL or AGPL module is in the graph, so the engine stays MIT.

MPL-2.0 is file-level copyleft: it applies to modified MPL files, not to the
surrounding work. This slice does not modify or vendor any MPL file. Shipping a
binary that statically links MPL code does, however, require including the
licence text and notices with the distribution. The repository has no
`THIRD_PARTY_NOTICES` file today; adding one is a release follow-up, not part of
this slice.

`github.com/go-llsqlite/adapter` ships no licence file at all. It is a thin
adapter used by the library's cgo sqlite piece-completion backend
(`storage/sqlite-piece-completion.go`, build tag `cgo && !nosqlite`). Its author's
other modules are ISC, but that is an inference, not a licence. Either add a
notices file that records the omission, or build with `-tags nosqlite` (bolt
completion, pure Go) before distributing binaries.

### Measured cost

| Build | Binary size |
|-------|-------------|
| Before this slice | ~28 MB |
| Default (cgo, sqlite completion) | ~53 MB |
| `CGO_ENABLED=0` (bolt completion) | ~50 MB |
| `-tags nosqlite` | ~49 MB |

The growth is the torrent, DHT, uTP, WebRTC and observability dependency trees.
It is accepted for this slice because selective fetch is a distinct capability,
not a replacement for `grab`; if binary size becomes a constraint, that is a
packaging decision (separate build tag or a plugin boundary), not something the
adapter can remove.

## What the slice does

1. `torrent inspect SOURCE` resolves metadata and lists files.
2. `torrent fetch SOURCE --select PATH...` downloads only those files.
3. Selected files are captured through `internal/manifest`, so
   `manifest verify` and `archive import` work unchanged.

Sources: a v1 magnet (`urn:btih`) or a local `.torrent` file. v2-only magnets
(`urn:btmh`) are rejected: selection and capture here are defined over v1 file
offsets.

## Safety model

- **Hostile paths.** Torrent name and every file path component are validated
  against traversal (`.`, `..`), absolute paths, path separators, NUL bytes,
  invalid UTF-8, Windows drive letters, reserved device names (`CON`, `LPT1`),
  trailing dot/space, over-long components, and duplicate/colliding paths. A
  single bad component rejects the whole torrent — never a silent rename.
- **Validated before storage opens.** A `.torrent` file is parsed and validated
  before it is handed to the library, so no directory is created first.
- **Symlinks.** Existing symlinks inside staging are rejected on the write path,
  and the final read for hashing re-resolves the path and requires it to stay
  inside staging.
- **Selection is explicit.** No flag downloads a whole torrent. An unknown path
  is an error, and the CLI refuses to fetch without `--select`.
- **Only verified selected files are captured.** Every piece covering a selected
  file must be in the library's completed-and-idle state, the file must exist
  under its final name with the metainfo length, and the bytes are hashed again
  by Provenance before the capture item is written.
- **No secrets in records.** The stored source reference is a magnet carrying
  only the infohash; trackers (which can contain passkeys) and local paths do not
  reach job records, capture items, or terminal output.
- **No remote surface.** No listener for control, no API, no RPC. The client
  only speaks the peer protocol.

## Pieces, files and selection

BitTorrent pieces are fixed-size and do not respect file boundaries, so a
selected file can share a piece with a neighbour. Fetching the selection can
therefore write bytes of unselected files. That is expected; what matters is
that only selected, complete, verified files enter the capture and the vault.
The integration test asserts exactly that: a 3-file fixture with 16 KiB pieces
where piece 0 spans `a.bin` and part of `b.bin`; selecting `b.bin` writes
`a.bin` too, and the capture contains one item.

## Staging ownership

One process owns one staging root at a time. The lock is
`<root>/.provenance/torrent/staging.lock`, taken with `internal/filelock`
(`flock` on unix, `LockFileEx` on Windows) and held for the whole life of a
`bittorrent.Session` — from `New` until `Close`. Callers cannot skip it: it is
enforced inside the backend, not in the CLI.

### Why the root and not the infohash

Everything the backend mutates is root-scoped or can collide across torrents in
one root:

| State | Path | Shared by |
|---|---|---|
| piece completion | `<root>/.torrent.db` | every torrent in the root (one library instance per data dir) |
| payload | `<root>/<torrent name>/…` | two torrents whose names match resolve to the same directory |
| capture run manifest | `<root>/.provenance/runs/<ts>.json` | one-second granularity, so two captures in the same second overwrite each other |
| capture items | `<root>/.provenance/items/<id>.json` | unique per infohash, but published by the same writer |
| job / metainfo | `<root>/.provenance/torrent/…` | per infohash, but read and rewritten by reconciliation |

A per-infohash lock would leave the first three races open, so ownership is
taken on the root.

### Lock identity and spellings

The lock is a file inside the root, and `filepath.Abs` normalises lexical
spellings (`/a/b/`, `/a/./b`, `/a/x/../b`). Symlinked spellings are *not*
normalised but resolve to the same inode, so they contend on the same lock. Case
aliases on case-insensitive filesystems behave the same way. The sidecar is
never deleted: removing it would race a process that is about to lock it, and
an existing file is never treated as evidence of a live lock.

### Acquire and release points

| Point | Action |
|---|---|
| `New` | `TryAcquire` after the root exists, before the torrent client (which opens `.torrent.db`) is created |
| `New` failure | released if client construction fails; never acquired if the root is busy |
| `List` / `Fetch` | no re-acquisition; they run under the lock taken in `New`, serialized on the session |
| `Close` | client stopped first, then the lock released, so no background writer outlives ownership |
| process death | released by the kernel when the descriptor closes (no stale-lock recovery needed) |

A competing `New` fails with `ErrStagingBusy` before reading or writing anything:
no job record is created, no torrent is added, no swarm connection is attempted.
Nothing is retried automatically — the caller decides when to try again.

Ownership is not dropped at "capture committed". While `--seed` keeps the client
up, it still writes part files and completion state, so a repeat `fetch` against
that staging scope stays busy until the owner exits. The capture itself is
already published and readable at that point.

### Readers are not locked

Ownership excludes other writers, not readers. `archive import` and
`manifest verify` do not take the lock, so they can run while a job is active.
The visibility point is unchanged (see the next section): items are written
before the run manifest, so a reader that runs mid-capture can see item files
that are not part of a published capture yet. That is pre-existing behavior;
this slice only prevents two writers from interleaving.

## Read-only status

`provenance torrent status` answers "what does the local record say about this
job?" without joining anything. It is safe to run against a root that another
process owns, from a script, or after a crash.

### What it reads

Only the job records in `<root>/.provenance/torrent/*.job.json`, with the same
decoder and validation the writer uses (`readJob` + `validateJobRecord`). It
does **not**:

- construct a `Session` or take the staging lock (so it never waits for, or
  joins, a running job);
- open `.torrent.db`, start a client, or contact peers;
- hash or read payload bytes;
- create directories, repair records, or rewrite a v1 record.

A v1 record is decoded through the in-memory upgrade path and reported with the
format it actually has on disk (`provenance/torrent-job+1`), so status does not
pretend the file was migrated.

### Recorded is not live

Every value is what the last writer of that record stored, as of the record's
`updated_at`. Nothing here is observed:

- a job can record `seeding` long after the process that was seeding died; the
  record stays that way until some run writes again (a later fetch that does not
  seed records `stopped`);
- a lock file on disk means nothing to status — the lock is an OS lock, not the
  presence of a file, and status does not look for one;
- `updated_at` is the last time the record changed, not a heartbeat.

The human output says so on every job, and the JSON uses a `recorded_state`
object rather than a bare state, so a consumer cannot mistake it for a probe.

### Consistency

Each record is published by atomic rename (unique temp file in the same
directory, then `rename`), so a reader that opens one record sees either the
complete old version or the complete new one. `TestReadDuringAtomicReplacement`
replaces a record 300 times while a reader projects it and asserts every
observation is one of the two complete versions.

Across records there is **no** snapshot, and the exposure is bounded rather than
zero: ownership admits one writer per root, and that writer touches its own
record only, so a listing can mix at most "the record of the job in flight" with
"every other record, which is quiescent". With no job in flight a listing is
internally consistent in practice. Status does not claim more than that, and it
does not retry: retrying would add cost for a window it cannot close without
the writer lock it is designed not to take. `complete` reports whether every
record was readable, not whether the set was globally consistent.

Status also does not stat or re-verify the capture manifest: the record and the
manifest are separate files that a writer may replace at different times, so
"record plus manifest" is deliberately not presented as one atomic fact.
Re-establishing that truth is `manifest verify` (per capture) and `archive
verify` (per vault), not status.

`staging_root` is resolved through symlinks, so `./staging`, an absolute path,
and a symlinked spelling all report the same root. Record files are opened with
`Lstat` and must be regular files: a symlink planted in the staging tree is an
unreadable record, not a window into something outside the root.

### Failure modes

| Situation | Behavior |
|-----------|----------|
| `--output` does not exist | error, exit 1; status never creates the root |
| root exists, no jobs | `no torrent jobs recorded`, exit 0 |
| requested id missing | `torrent job not found`, exit 1 |
| requested id is not an identity | `invalid torrent job id`, exit 1 (validated as `bt-<40 hex>` or the bare infohash, never used as a path) |
| a record fails to decode, has an unsupported format, is filed under the wrong name, or is not a regular file (symlink) | listed under `unreadable` with a reason; JSON stays valid and `complete` is `false`; a stderr warning names the file; the command exits 1 because the listing is incomplete |

### What a record can and cannot prove

| Proved by the record | Not proved by the record |
|----------------------|--------------------------|
| identity: job id, infohash, backend | that the process is alive |
| the immutable selection | that the payload files still match their hashes |
| the last recorded payload, capture and seeding states | that the capture manifest still exists or is intact |
| the recorded per-file digests (`payload.assets`) | that the payload bytes still match them |
| the capture idempotency key and manifest path the writer recorded | that a peer, tracker or swarm is reachable |

Anything beyond that is `archive verify` (vault integrity) or a fresh fetch.

## Job state and crash recovery

A job is a durable local record at
`.provenance/torrent/bt-<infohash>.job.json` (`provenance/torrent-job+2`). It
holds a stable job ID derived from the infohash, the backend, the immutable
selection, a pointer to the backend's own resume state (persisted metainfo path,
data directory, part-file scheme), the three states below, and the idempotency
key of the committed capture.

The record deliberately does **not** keep a second piece checkpoint: piece
completion and part files are owned by the torrent backend, and the record only
names where they live.

### Three independent states

| Dimension | Values | Meaning |
|-----------|--------|---------|
| `payload` | `pending` → `ready` | `ready` only after every selected file is complete, hash-verified and the SHA-256 digests are recorded |
| `capture` | `pending` → `committed` | `committed` once the run manifest is written; a job can sit at payload-ready/capture-pending indefinitely and be retried |
| `seeding` | `stopped` ↔ `seeding` | only ever `seeding` with `--seed`; never blocks capture |

A job can therefore be *payload ready, capture pending* (a capture failure —
retry from staging) or *capture committed, seeding* (still serving peers after
the capture is visible).

### The commit point

`archive import` reads run manifests (`<staging>/.provenance/runs/*.json`), and
`manifest verify` reads per-item records. The operation that makes a capture
visible is the atomic rename of the run manifest — so items are written first
and the run manifest last:

```
verify bytes → hash → write job.payload (ready)
            → write items/*.json  (not yet a capture)
            → write runs/<ts>.json   ← COMMIT
            → write job.capture (committed)
```

An item file without a run manifest is invisible to archive import, and a retry
overwrites it under the same external ID.

### Idempotency key

`capture_key` (on the run manifest) is `sha256("provenance/torrent-capture+1",
job_id, and each selected file's path, length and SHA-256)`. It contains no
timestamp, run id, or random value, so the same job and the same staged bytes
always produce the same key.

On every run, before doing work, the adapter scans existing run manifests for
that key:

- found, staged bytes still verify → the capture is **adopted** (`reconciled:
  true`), nothing is written;
- found, staged bytes no longer verify → the staleness is reported as a warning,
  the payload attestation is dropped, and the normal resume path repairs the
  bytes and adopts the same capture;
- not found → a new capture is committed.

### Crash windows

| Crash point | On restart |
|---|---|
| mid-transfer | payload `pending`; retained `.part` data is re-verified and the transfer resumes |
| after verification, before commit | payload `ready`; capture is written **without opening the swarm** (`BytesRead == 0`) |
| after commit, before job update | the key is found in the existing manifest and the job is reconciled to it; no duplicate manifest is written |
| during the capture write (items only) | orphan items are overwritten by the retry under the same external ID; nothing is visible to archive import |

Residual windows this slice does **not** close:

- Two writers can no longer interleave: the staging root is owned for the
  lifetime of a Session (see above). What remains is reader visibility — a
  `manifest verify` that runs mid-capture can see an item file whose run
  manifest has not been published yet, and `archive import` simply does not
  see it. That ordering is unchanged by this slice.
- `capture: committed` is written after the manifest, so the crash window above
  is real — it is reconciled, not prevented.
- Reconciliation assumes the capture manifest still exists. A job whose
  committed manifest was deleted is re-captured from staging (same key, new run
  file), not reported as an integrity failure.
- Staged bytes are re-hashed before adopting a capture, so a repeat `fetch` costs
  a local re-read of the selection. That is deliberate: a correct capture record
  does not prove the working copy is still correct.
- The lock is advisory. A process that deletes `<root>/.provenance/torrent/`
  or replaces the root mid-run is outside the contract; nothing can prevent
  that from inside the directory being locked.

## Resume and restart

- The exact metainfo is persisted once at
  `.provenance/torrent/<infohash>.torrent` and reused on later runs, so a restart
  needs no peer to begin verifying (asserted by a test that shuts the seeder
  down).
- Retained pieces are re-hashed (`VerifyData`) before more data is requested:
  verified bytes are reused, corrupted bytes are re-fetched rather than captured.
- Library part files (`.part`) are left alone on interruption; the next run
  completes and finalizes them.
- The job record holds the immutable selection; re-running with a different
  selection is refused (`ErrSelectionLocked`).

## Limits

| Knob | Default | Where |
|------|---------|-------|
| Selected bytes | unlimited | `--max-bytes` |
| Overall time | unlimited | `--timeout` |
| Metadata wait | 2 minutes | fixed default (`Config.MetadataTimeout`) |
| Peers per torrent | library default (50) | CLI sets 40 (`MaxPeers`) |
| Metainfo file size | 16 MiB | fixed cap |

Disk usage follows the selection plus shared edge pieces; there is no quota
enforcement yet.

## Out of scope (deliberately)

- Durable multi-job queue, backend resume references, crash-boundary
  reconciliation (`PR 2` in the plan).
- Seeding policy beyond opt-in `--seed`.
- Web seeds (disabled), WebTorrent (disabled), v2-only torrents.
- HTTP API, event stream, TUI integration.
- aria2 RPC, session import/export, `.aria2` checkpoints.
- Soulseek / slskd.
- Archive schema changes: the capture manifest and vault types are untouched.

## Tests

`internal/transfer/bittorrent` runs a local in-process seeder over loopback with
DHT, PEX, trackers, webseeds and uTP disabled — CI never contacts a public
tracker. Covered:

| Test | Proves |
|------|--------|
| `TestFetchCapturesOnlySelectedFiles` | magnet join, metadata listing, exact selection, one capture item, `manifest verify`, `archive ingest` |
| `TestListReturnsMetadataWithoutCapturing` | metadata-only listing persists metainfo and writes no capture |
| `TestInterruptedFetchResumesAndReusesVerifiedBytes` | interruption leaves `.part`, resume reads less than the whole file, then a restart with **no peer** captures with `BytesRead == 0` |
| `TestCorruptedRetainedDataIsRepaired` | local corruption is detected by re-hash, re-fetched, and never captured |
| `TestFetchFromLocalTorrentFile` | `.torrent` input path |
| `TestSelectionIsRequiredAndExact` | no whole-torrent download, unknown path is an error |
| `TestSelectionSizeLimitRejectsBeforeDownload` | `--max-bytes` before payload |
| `TestJobSelectionCannotChangeMidJob` | immutable selection, order-insensitive retry |
| `TestHostileMetainfoPathsRejected` | traversal, empty component, drive letter, hostile torrent name |
| `TestStagingSymlinkRejected` | symlinked staging directory |
| `TestCaptureRetryAfterPayloadVerifyDoesNotRefetch` | crash after verification: retry commits the capture from staging with the swarm closed and `BytesRead == 0` |
| `TestCaptureCommitCrashReconcilesWithoutDuplicate` | crash after the manifest rename: restart adopts the existing capture, exactly one manifest, job reconciled |
| `TestSeedingDoesNotBlockCaptureCompletion` | capture is committed and readable while `--seed` keeps the client up; cancelling stops seeding without touching the capture |
| `TestStaleStagedPayloadIsReportedAndRepaired` | a corrupted staged file is reported as a warning, dropped to unverified, repaired and re-adopted under the same key |
| `TestJobArtifactsDoNotLeakTrackerSecrets` | job record, manifests and items never contain tracker/passkey strings |
| `job_test.go` | key stability, v1→v2 upgrade, immutable selection, record path confinement, source sanitisation |
| `ownership_test.go` | second session on the same root is busy, busy sessions write nothing, symlink/`.` spellings are the same owner, independent roots run concurrently, concurrent Fetch on one Session is serialized, closed sessions refuse work, ownership survives `--seed` |
| `ownership_process_test.go` (real processes) | barrier: a competing process gets the busy error and writes nothing, then exactly one manifest after the owner exits; SIGKILL of the owner releases the lock and the job reconciles; a seeding owner stays exclusive and releases on cancel; different torrents in one root are excluded; independent roots proceed concurrently |
| `status_test.go` | deterministic list order, projection of a recorded job, empty root creates nothing, unreadable records reported with a reason (corrupt JSON, unsupported format, mismatched file name), lookup by id/bare infohash/case-insensitive, path-like ids rejected before any filesystem access, missing root not created, v1 record readable and byte-identical afterwards, secrets never projected or echoed, unsafe record paths rejected, 300 atomic replacements observed only as complete old/new records |
| `status_process_test.go` (real CLI, real processes) | `torrent status` against a root owned by a seeding process returns promptly with the recorded capture, leaves the tree unchanged, and does not disturb ownership (a competing fetch is still busy); after `SIGKILL` of that owner, status reports the last recorded state as recorded while a lock file still exists, and a later fetch acquires the root and reconciles |
| path unit tests | component validation, containment, peer parsing, kind mapping |

## Follow-ups

1. Add `THIRD_PARTY_NOTICES` (MPL texts, the missing `go-llsqlite/adapter`
   licence) or ship with `-tags nosqlite`; decide before the first binary
   release that includes this slice.
2. A multi-job queue: jobs are now durable and mutually exclusive per staging
   root, but nothing enumerates or schedules them, and there is no
   `torrent status` that reports state without running `fetch`.
3. Optional per-job bandwidth cap, needed before fetching large public swarms.
4. Verify behaviour against a real swarm with a private tracker, outside CI.