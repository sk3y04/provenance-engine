# Storage isolation: baseline survey (BT-05, step 1)

Read-only survey before any code. Nothing here is a proposal yet; it is what the
current layout is keyed by, which collisions are already refused, and what
"isolated per-job workspace" would have to change. Written from the code at
`95fc83e`.

## What is keyed by what

| State | Path today | Keyed by | Shared by |
|-------|------------|----------|-----------|
| Payload | `<root>/<info.Name>/…` | the torrent's display name | any two torrents with the same name in one root |
| Piece completion DB | `<root>/.torrent.db` | one bolt/sqlite DB per root | every torrent the root's client has ever added |
| Part files | `<root>/<info.Name>/<path>.part` | library internal | same as payload |
| Persisted metainfo | `<root>/.provenance/torrent/<hash>.torrent` | full v1 (or v2) hash | one job |
| Job record | `<root>/.provenance/torrent/<jobID>.job.json` | job id (`bt-<40hex>`, `bt2-<64hex>`) | one job |
| Staging lock | `<root>/.provenance/torrent/staging.lock` | the root | every writer, by design |
| Capture items | `<root>/.provenance/items/<id>.json` | `<infohash>:<staging-relative path>` | one file |
| Run manifest | `<root>/.provenance/runs/<ts>.json` | **second granularity timestamp** | every capture in that second |

Two facts fall out of the table: everything that is *not* identity-keyed is
root-scoped (payload directory, piece DB, run manifest name), and the only thing
that keeps two jobs apart today is that a root admits one writer at a time.

## What is already refused

- **A second identity that would share the library's 20-byte storage key or the
  payload directory** is `ErrIdentityConflict`, checked before storage opens
  (`checkPayloadDirConflict`, `checkIdentityConflict`). Covered by
  `v2_test.go` (two same-named v2 torrents in one root) and `magnet_v2_test.go`
  (synthetic identities with an equal `StorageKey`, no storage side effects).
- **A second writer on one root** is `ErrStagingBusy`, taken in `New` and held
  until `Close` (`ownership_test.go`, `ownership_process_test.go`).
- **Hostile paths** are validated before storage exists, including symlinks on
  every path a piece could touch (`path_test.go`, `inspect_test.go`).

## What the current layout cannot do

1. **Two jobs downloading at once in one root.** The library's file backend
   writes `<DataDir>/<name>`, one client owns a root, and the completion DB is
   one file for the whole root. concurrency would need either one client per job
   with its own `DataDir`, or a storage implementation that rewrites paths.
2. **Two same-named torrents, even sequentially.** Refused today, which is the
   right behaviour for one root but means the *job* is not the unit of
   isolation: the root is.
3. **Case-insensitive collision safety.** Not tested. Two identities whose names
   differ only by case are distinct directories on Linux and one on a
   case-insensitive filesystem.
4. **A capture that names its own payload.** Run manifests are named by second,
   so two captures in one second overwrite each other; that is why
   `captureKey` is content/job-derived and not timestamp-derived, but the file
   name still is.

## What "isolated workspace" would mean

The shape that fits the constraints above, for the new runner only (BT-07), not
for the current `fetch`:

```
<root>/.provenance/torrent/jobs/<jobID>/
    job.json            job record
    metainfo.torrent    exact metainfo (0600)
    payload/            this job's DataDir: <payload>/<info.Name>/…
    .torrent.db         this job's piece completion
```

- One library client per active job, `DataDir = <payload>`, so the payload
  directory and completion DB become job-scoped and two same-named torrents
  cannot meet.
- The job id is the full identity (`bt-<40hex>` / `bt2-<64hex>`), never a
  truncated hash, and the path is derived from it, so the namespace is
  collision-resistant by construction rather than by the conflict check.
- A capture item id would also carry the job id, so two jobs cannot overwrite
  each other's items; run manifests keep their format (archive import depends on
  it) but are written under the job's own directory.

## What must not change

- Full v1/v2 identities, the canonical hybrid mapping, v1 capture keys, the
  capture item and run manifest formats, and the archive handoff. `archive
  import` reads `<root>/.provenance/runs/*.json`; a new writer that moves those
  files breaks the vault path.
- Legacy roots stay readable: payload at `<root>/<name>`, DB at `<root>/.torrent.db`,
  records at `.provenance/torrent/*.job.json`. Any migration is versioned,
  dry-runnable, crash-safe and reversible, and moves no live seed data.

## Test plan for the implementation ticket

Already covered: same-name conflict, equal-`StorageKey` conflict, symlink
rejection, hostile components, one-writer-per-root.

Still to write: two same-name jobs whose captures are both unique; stopping or
cleaning one job leaving the other untouched; a case-collision pair on a
case-insensitive filesystem (or an explicit refusal with a reason); a
file/directory conflict (`a/b` as file vs directory); a migration dry run, a
crash between move and record update, and a rollback.

## Open decisions (need a call before code)

1. Per-job client and `DataDir` — accepted cost is one client per active job
   (goroutines, sockets) against losing the root-scoped completion DB. The
   alternative, a path-rewriting storage wrapper, is more code and more risk.
2. Whether `fetch` itself moves to the new layout, or only runner-managed jobs
   do. The roadmap says the new runner only, until a finer-grained ownership
   model exists.
3. Whether the run-manifest file name changes (it is format-visible) or stays
   timestamp-based inside the job directory.