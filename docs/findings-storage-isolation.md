# Storage isolation: baseline, shipped layout and evidence (BT-05)

The first half is the read-only survey taken before any code (at `95fc83e`): what
the legacy layout is keyed by, what is already refused, and what it cannot do.
The second half is what shipped: the isolated workspace, its ownership and lock
order, how a capture is located, the legacy-conflict policy, and what the tests
demonstrate.

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

## The shipped workspace

Implemented in `internal/transfer/bittorrent/workspace.go`. The baseline above is
unchanged: legacy jobs keep it exactly.

```
<root>/.provenance/torrent/jobs/<jobID>/          session root == library DataDir
    <torrent name>/<path>                          payload and .part files
    .provenance/completion/.torrent.db             piece completion, job-scoped
    .provenance/torrent/<hash>.torrent             exact metainfo, 0600
    .provenance/torrent/<jobID>.job.json           job record
    .provenance/torrent/staging.lock               workspace ownership
    .provenance/items/<identity>_<path>.json       capture items
    .provenance/runs/<ts>.json                     published run (commit point)
```

A real one, from `TestIsolatedCaptureIsReadableByExistingReaders`:

```
.provenance/
  completion/.torrent.db                                 12288 bytes
  items/9af1d6c1..._fixture_a.bin.json                     723 bytes
  runs/2026-11-19T014306Z.json                            1558 bytes
  torrent/9af1d6c1....torrent                              288 bytes
  torrent/bt-9af1d6c1....job.json                         1150 bytes
  torrent/staging.lock                                       0 bytes
fixture/a.bin                                           30000 bytes
```

Two properties keep this small instead of introducing a second path scheme:

- The session's root **is** the workspace, so every path the adapter already
  derives - `statePath`, `jobPath`, `lockPath`, the capture namespace,
  `joinWithin` containment, `finalizedPath` - becomes job-scoped without change.
  Capture paths stay `<torrent name>/<path>`, so items and manifests keep their
  existing shape and the capture key format does not move.
- The payload base and the completion database are settable independently of
  `DataDir`: `storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir,
  PieceCompletion})`, with the completion database built by
  `NewDefaultPieceCompletionForDir` on `.provenance/completion`. `DataDir` stays
  the workspace so the library writes `<ws>/<name>/...` exactly as it writes
  `<root>/<name>/...` for a legacy job, and the database stays out of the payload
  namespace. The session closes that storage itself, after the client stops,
  because the library does not close a caller-provided `DefaultStorage`.

## Ownership and lock order

| What | Legacy | Isolated |
|------|--------|----------|
| Lock | `<root>/.provenance/torrent/staging.lock` | `<ws>/.provenance/torrent/staging.lock` |
| Writers admitted | one per root | one per workspace |
| Root-level writes | all of them | none; the root's record directory is only read |

An isolated session takes exactly one lock, its workspace lock. It does not take
the root lock, so there is no lock order to define: nothing takes both, and a busy
workspace cannot block an unrelated workspace. The one root read is
`checkLegacyConflict`, which reads a record published by atomic rename, so it sees
a complete old or new record.

## How a capture is found, and what readers see

- `Capture.StagingDir` is the workspace; `Capture.ManifestPath` is
  **workspace-relative** (`.provenance/runs/<ts>.json`). No raw path string
  crosses a workspace boundary.
- Existing readers work when pointed at the workspace, because a workspace is a
  complete provenance root: `torrent status -o <ws>` lists its one job,
  `manifest verify <ws>` and `archive import <ws>` read its runs. Nothing is
  mirrored into the root, and no shared mutable index exists.
- Pointed at the **root**, `torrent status` and `archive import` see only legacy
  jobs. Isolated captures are not enumerable from the root in this PR: the queue
  and the index that would list them are BT-07, deliberately out of scope.
- `archive import` stays opt-in: a workspace is imported only when a caller names
  it.

## Legacy conflict policy

`ErrLegacyJobConflict` when the root already holds a legacy record for the same
canonical job id. No adoption, no migration, no relocation, no second job over the
same capture intent, and the legacy record is not modified (asserted
byte-for-byte). An isolated workspace whose own record describes a different
identity is also a hard error (`validateJobRecord`), never a rename.

## Identity and metadata ordering

The workspace path comes from the canonical job id only - `bt-<40 hex>` or
`bt2-<64 hex>`, validated by `validateJobID` - so a bare hash, a display name or a
truncated storage key cannot become a path. A magnet is resolved metadata-only
first (`resolveMetainfo`: no payload storage, no workspace, an in-memory
completion), and only the resolved identity picks the workspace. A btmh-only
magnet naming a hybrid therefore lands in its canonical `bt-<v1>` workspace
instead of creating a provisional `bt2-` job. The discovery profile used for that
resolution is the profile the job will run with; a private torrent is refused
before any workspace exists.

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

## Decisions taken

1. **One client per active job**, with its own payload base and completion
   database. No path-rewriting storage wrapper.
2. **Foreground `fetch` keeps the legacy layout.** Only the isolated entry point
   (`OpenIsolated`, `FetchIsolated`) uses workspaces: no migration, no fallback,
   no relocation.
3. **Run manifests keep their name and format** (`<ts>.json` in the workspace's
   `.provenance/runs/`), so nothing that reads them changes.

## Evidence

Acceptance runs in `isolated_workspace_test.go`. Every case uses real sessions
over loopback, and synchronization is a channel or a condition poll, never a
sleep.

| Test | Demonstrates |
|------|--------------|
| `TestIsolatedTwoConcurrentJobsSameName` | two sessions under one root on distinct torrents that share display name and relative paths: distinct workspaces, payloads, completion databases, job records and capture keys; both digests correct; the root grows nothing |
| `TestIsolatedWorkspaceBusyAndRecovery` | a held workspace refuses a second writer (`ErrStagingBusy`); an unrelated workspace transfers while it is held; a cancelled transfer publishes nothing; reopening recovers on retained bytes with exactly one run |
| `TestIsolatedWorkspacesKeyedByFullIdentity` | two identities colliding on the 20-byte storage key get distinct workspaces and distinct databases; a legacy record for one does not block the other; nothing is adopted by the truncated key |
| `TestIsolatedHybridResolvesToOneWorkspace` | `.torrent`, `btih`, `btih+btmh` and btmh-only all resolve to the same canonical `bt-<v1>` workspace and reconcile to one capture |
| `TestIsolatedLegacyConflictIsRefused` | conflict refused, no workspace created, legacy record byte-identical |
| `TestIsolatedCaptureIsReadableByExistingReaders` | workspace-relative manifest path; status, items and runs readable from the workspace; the root has none |

Not demonstrated, and not claimed: process-kill recovery for a workspace (the
cancel path above is in-process; the same `filelock` primitive is covered for
legacy roots by `ownership_process_test.go`), and resistance to another process
changing the workspace between the symlink checks and the library's writes. The
adapter rejects existing symlinks on the workspace path, re-resolves containment
for its own reads, and refuses a payload name that would merge with the job's
records, but library-owned I/O is not re-validated per write.
