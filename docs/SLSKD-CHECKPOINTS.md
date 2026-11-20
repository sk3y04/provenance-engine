# Soulseek integration checkpoints

One entry per shipped slice, written after the code commit so it carries the real
SHAs. Conventions match `docs/BT-CHECKPOINTS.md`: commands are the ones actually
run, `verified` means observed in this session, `inferred` means reasoned from the
code or upstream source without an observation behind it, and every limit is
stated rather than smoothed over.

Branch: `feat/slskd-integration`. Base: `61f1cb3` (`master`, "docs(release):
prepare v0.9.0"). Merged: fast-forward to `master` at `d34e326`; release prep
`9598103` (v0.10.0). Pushed: master and the `v0.10.0` tag verified on origin
at `17b667f`; the tag's Release run failed on a pre-existing torrent defect
(BT-15 in `docs/BT-CHECKPOINTS.md`, fixed at `832c3ee`), so v0.10.0 has no
published GitHub release. The integration shipped as **v0.10.1**: prep
`7c869eb`, tag verified on origin, Release run green, all seven assets
published.

| Slice | Commit | Scope |
| --- | --- | --- |
| M0 | `9013c3f` | repo audit, live 0.26.0/0.26.2 contract spike, ADR, Swagger fixtures |
| M1 | `2415c7a` | typed client, settings store, four-state health, CLI `settings/health/test` |
| M2 | `f8c762e` | search coordinator, durable jobs, reconciler, ingestor, e2e test |
| M3 | `f41cdc5` | queue controls, webhook receiver, boot recovery |
| M4 | `89448c3` | curated shares, uploads, share-safety guards |
| docs | `eea0aba` | `docs/SOULSEEK.md`, CLI reference, README, CHANGELOG |
| review | `06a9e48` | ten defects found reviewing M1–M4, each with a regression |
| review fix | `9e0ff4b` | an unknown job/share id reports itself instead of leaking the record path |
| release | `9598103` | merge verified; v0.10.0 release prep (version, CHANGELOG, RELEASING, TREEVIEW, ARCHITECTURE) |

---

## M0 — audit and live contract

**Why the audit mattered.** The execution spec assumed an HTTP server, a browser
frontend, an ORM with migrations and a user model. This repository is a
single-binary CLI/TUI with JSON record stores and no local database, so every
proposed path and table was re-mapped in `docs/adr/slskd-integration.md` before any
product code was written.

**What the spike proved** (`docs/findings-slskd-0.26-contract.md` has the commands
and raw payloads):

- `searchTimeout` is **milliseconds**. Both the 0.26.0 Swagger and
  `SearchRequest.cs` say seconds. `searchTimeout: 5` ended a search **53 ms** after
  it started (`state: "Completed, TimedOut"`). Sending seconds would have been a
  4 h 10 min search per query.
- `slskd`'s share scanner **follows symlinks out of the share root**, even on a
  `:ro` mount: `evil.txt -> /etc/hostname` and `shadow.txt -> ../../../../etc/shadow`
  were both scanned and advertised (`Sharing 1 directories and 7 files`). Hardlinks
  are indexed as ordinary files. This is the whole justification for the
  hardlink-only materialiser.
- `#1905` reproduced: `GET /api/v0/options` is `200` on 0.26.0 with a `readwrite`
  key and **`403` on 0.26.2** with the same key (administrator: `200`).
- `GET /api/v0/events` is a paginated event log, **not** SSE. `GET /api/v0/shares`
  returns a map keyed `local`, not by alias. Composite states serialise as
  `"Completed, TimedOut"` (comma + space). Batch enqueue answers `201`/`207`/`200`
  for all/partial/none, `404` for an offline peer, `500` for a message-connection
  failure.

Not verified in M0, and still not: `#1891`'s 180-second crash, `FileService.MoveFile`
permissions under `mode: 755` vs `775`, `_{ticks}` collisions on a real download,
`#1818`, `#1819`, `#1609`, `#1663`, and `SLSKD_SHARED_DIR` vs YAML array merging.

---

## Review pass (`06a9e48`)

Ten defects, each found by reading the code against the contract rather than by a
failing gate, and each fixed with a regression that was **watched fail first**:

| # | Defect | Regression | Evidence before the fix |
| --- | --- | --- | --- |
| 1 | `CheckSettingsHealth` used the nil client to redact its own error, panicking on an invalid profile | `TestCheckSettingsHealthSurvivesAnInvalidProfile` | the package's test binary died with a nil dereference at `health.go:238` |
| 2 | A `207` partial batch left refused files stateless: the job polled for a transfer that would never exist | `TestEnqueuePartialBatchSettlesInsteadOfStalling` | `rejected markers = false/false`, job stuck `IN_PROGRESS` |
| 3 | A wall-clock stop returned the poll that preceded it, discarding the final response window | `TestSearchStopKeepsFinalResponses` | `responses from the final window were lost: []` |
| 4 | `locate()` keyed staged files by basename: two albums, one track name → one file, one capture id | `TestIngestKeepsFilesWithTheSameBasenameApart` | both items resolved to `AlbumB/track.flac` |
| 5 | A partly-present job was imported as complete | `TestIngestRefusesAPartialImport` | `partial import reported as success` |
| 6 | `cleanup` ran `RemoveAll` on the whole destination, deleting other jobs' bytes in a shared destination | `TestIngestCleanupTouchesOnlyThisJobsFiles` | `cleanup deleted a file it did not import` |
| 7 | `manifest.RunPath` is second-granular: two jobs finishing in one second overwrote a capture | `TestIngestTwoJobsInOneSecondKeepBothCaptures` | `1 run manifests for two jobs … want 2` |
| 8 | Re-adding a shrunk folder kept links for deleted files, so slskd kept advertising them | `TestShareAddPrunesLinksForRemovedFiles` | `a link for a deleted source file survived the re-add` |
| 9 | `QueuePosition` decoded a `position` field; the endpoint returns the updated Transfer, so the place was always `0` | `TestQueuePositionUsesTheTransferShape` | `an absent placeInQueue reported as known: &{Position:0 Known:true …}` |
| 10 | An unknown job or share id surfaced the record path (`open …/jobs/prov-….json: no such file or directory`) and triggered the media "deleted or unavailable" hint | `TestStoreMissesReadAsNotFound` | `error: open /…/.provenance/slskd/jobs/prov-0000000000000000.json: no such file or directory` |

Two assertions were added on code paths that were already correct, so they were
proven live by mutating the code and watching them fail:

- `TestClientBaseURLPathIsNotDoubled`: doubling the base-URL path produced
  `path = "/slskd/slskd/api/v0/server", want /slskd/api/v0/server`.
- The `Known` half of `TestQueuePositionUsesTheTransferShape`: forcing the flag
  produced `an absent placeInQueue reported as known`.

Both mutations were reverted, confirmed with `grep -n` (`prefix = basePath`,
`p.Known = true`), and the tests re-run green.

Smaller corrections in the same commit: the `#1760` permission check was a dead
condition that missed `644` (no execute bit) and never looked at world-write;
webhook secrets are now compared as SHA-256 digests so length is not leaked and an
oversized body is `413` instead of a truncated-JSON `400`; masking only masks a
secret that exists; `settings set --enable` validates the effective (env-layered)
profile; `ingest` honours the stored cleanup preference and clears the slskd record
like `watch`; `checkPaths` compares `io.EOF` as an error, not a string. Dead
exported helpers (`SortedNames`, `PeerUsernames`, `Item.Succeeded`) were removed and
`ValidateForTest` became `ValidateConnection`.

---

## Verification

**Gates actually run** (output kept, not paraphrased):

| Command | Result |
| --- | --- |
| `go test -race ./...` at `eea0aba` | `EXIT=0` (whole repo; run twice, both green) |
| `go test -race ./...` at `06a9e48` | `EXIT=0` |
| `go test -race ./...` at `9e0ff4b` | `EXIT=0` |
| `go test -race ./internal/slskd/ ./cmd/provenance/` | ok / ok |
| `go vet ./...` | clean |
| `gofmt -l .` | empty |
| `golangci-lint run ./...` | `0 issues` |
| `go mod tidy -diff` | empty (the CI gate that `make test` does not cover) |
| `make build` | `./provenance` produced |

**Test count:** 132 tests in `internal/slskd`, 13 in the `provenance soulseek` CLI
files. The M2 exit gate is `TestEndToEndSearchEnqueuePollIngest`: search → enqueue →
poll → file on disk → verified capture, all through the public API.

**Live smoke against the real binary** (`slskd/slskd:0.26.0`, throwaway profile,
`XDG_CONFIG_HOME` isolated):

| Command | Observed |
| --- | --- |
| `soulseek health` (no Soulseek credentials) | `SOULSEEK_DISCONNECTED`, version `0.26.0.0`, update to `0.26.2` reported |
| `soulseek health` (logged in) | `HEALTHY`, exit 0 |
| `soulseek search "beethoven symphony flac" --limit 5` | `1418 files, 66 locked, 5 folders (3.1s)`, ranked with slot/speed/queue |
| `soulseek shares status` | `Provenance  /shared  5 file(s)  remote=Provenance` (the `local` map key is not mistaken for an alias) |
| `soulseek shares files` | 5 paginated entries, Windows separators normalised |
| `soulseek shares preview <dir>` | `files: 4 … excluded: 1 symlinks: 1` — the planted symlink was counted, not offered |
| `soulseek uploads list` / `jobs` | `active=0 queued=0 …` / `no jobs` |
| `soulseek search` again after the review (rebuilt binary) | `1194 files, 31 locked, 3 folders (1.0s)`, ranked |
| `soulseek position prov-0000…` | `unknown job: prov-0000000000000000`, exit 1, no path and no unrelated hint |

**Not rerun as part of this record:** hosted CI (nothing is pushed), and the
`render`/TUI suites beyond the repo-wide `-race` run above.

---

## Verified vs inferred

| Claim | Status |
| --- | --- |
| `X-API-Key` is the only credential header; `Authorization: Bearer` is rejected | verified (spike + `TestClientSendsAPIKeyOnly`) |
| `searchTimeout` is milliseconds | **verified** (`5` → 53 ms) |
| Composite state strings use `", "` | verified for search states; **inferred** for transfer states (no real peer transfer was completed) |
| Batch status codes 201/207/200/404/500 | verified for 404 and 500; 201/207/200 exercised against a fake server only |
| slskd indexes hardlinks and follows symlinks out of a `:ro` share root | verified (7 files, including `/etc/hostname` and `/etc/shadow`) |
| `_ticks` rename on collision | **inferred** from upstream source; matched by `ticksMatch`, never observed on disk |
| `MoveFile` permissions under `mode: 755`/`775` | **not verified** (no real download) |
| `#1891`, `#1818`, `#1819`, `#1609`, `#1663` handling | defensive, unit-tested against the described symptom, **not reproduced** |

---

## Limits and non-claims

- **No real peer download was completed.** The transfer path is covered hermetically;
  every claim about a live transfer's on-disk result stays unproven.
- **No web UI, no HTTP API, no TUI view.** This repository has no server and no
  browser surface; the private product repository owns those per `AGENTS.md`. The
  integration is reachable through `provenance soulseek …` only. The ADR records the
  divergence; the TUI view set is planned and **not shipped**.
- **No database.** Jobs and share rules are versioned JSON records (`0600`, atomic
  rename, `filelock`) under `<root>/.provenance/slskd/`, following `torrentqueue`.
  One process is the writer; two concurrent processes serialise rather than merge.
- **Shares are hardlinks only.** Cross-device materialisation fails fast by design;
  there is no copy-export fallback.
- **`shares files` fetches the whole index per invocation** and paginates in-process;
  a 100k-file share costs one large `GET /shares/contents` per command. There is no
  cross-process cache.
- **M5/M6 not started**: peer browse and recursive enqueue, the Compose/Gluetun
  template, wishlist, saved peers, download-side blocklist, telemetry and chat.

## Merge and v0.10.0 release prep (`9598103`)

The branch fast-forwarded `master` from `61f1cb3` to `d34e326`. Verified before
editing: `git rev-parse HEAD feat/slskd-integration` both
`d34e32671d2d09180c8f26edc523d3a31ec6f8e2`, `git merge-base --is-ancestor
feat/slskd-integration HEAD` true, worktree clean. The merge itself was
performed by the maintainer; this session verified it and prepared the release
on top.

`v0.9.0` is already tagged and published (the local and `origin` tags both peel
to `61f1cb3`), so the integration ships as **v0.10.0** — an additive feature,
minor bump, `package engine` untouched.

Release prep (`9598103`): version default 0.9.0 → 0.10.0; CHANGELOG Unreleased
→ 0.10.0 (2026-11-20), rewritten to the post-review behaviour (the block had
been written at `eea0aba`, before the review pass); RELEASING.md default and
tag command; TREEVIEW.md regenerated (251 → 283 Go files — verified by diffing
`git ls-files '*.go'` against the files named in the treeview, and the 26-row
`internal/slskd` section against the directory listing); ARCHITECTURE.md gained
the Soulseek integration path, the on-disk Soulseek state and the `slskd`
package entry.

**Gates actually run on the prep tree:**

| Command | Result |
| --- | --- |
| `gofmt -l .` | empty |
| `go vet ./...` | clean |
| `golangci-lint run ./...` | `0 issues` |
| `go mod tidy -diff` | empty |
| `make test` (`go test -race ./...`) | EXIT=0; `internal/slskd` 3.5s, `cmd/provenance` 3.3s, `torrentqueue` 105.9s, `bittorrent` 77.0s |
| unstamped `go build` + `--version` | `provenance version 0.10.0` |
| `make build` | produced; stamps `git describe` (`0.9.0-11-gd34e326-dirty` until the tag exists) |
| `make engine-example` | ok |
| `make release-dry-run` | six platforms + `dist/checksums.txt` |

**Outcome after this record:** the maintainer approved the push; master and
`v0.10.0` were pushed and verified on origin at `17b667f`. CI on master
passed; the Security workflow's pre-existing failure and the tag's Release
run are recorded in BT-15 (`docs/BT-CHECKPOINTS.md`) — the Release run failed
on a pre-existing torrent test defect, fixed at `832c3ee`, and no GitHub
release was published for v0.10.0. The merge does not upgrade the evidence:
every "not verified" row above stays a non-claim, including the absent real
peer download.

## Next commands for a maintainer

```bash
git switch master
make all                      # vet + lint + test + build + engine-example
go test -race -count=1 ./internal/slskd/ ./cmd/provenance/
provenance soulseek settings set --base-url http://127.0.0.1:5030 --api-key <k> \
  --slskd-downloads-root /downloads --downloads-mount /srv/media/downloads --enable
provenance soulseek health     # expect HEALTHY, or a state with a diagnostic
```

To close the biggest evidence gap: run one real download against a peer that offers
a small, freely licensed file, then confirm `jobs` reaches `IMPORTED`,
`manifest.VerifyCaptureDir` passes, and record the observed `_{ticks}`/permission
behaviour in this file with the real numbers.