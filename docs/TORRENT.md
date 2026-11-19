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

1. `torrent inspect SOURCE` resolves metadata and lists files, without a job:
   no staging lock, no payload storage, no writes.
2. `torrent fetch SOURCE --select PATH...` downloads only those files, and only
   after the layout and the selection have passed admission.
3. Selected files are captured through `internal/manifest`, so
   `manifest verify` and `archive import` work unchanged.

Sources: a v1 magnet (`urn:btih`), a v2-only magnet (`urn:btmh`), a dual-identity
magnet, or a local `.torrent` file of any of the three forms. v1, v2-only (BEP
52) and hybrid metainfo are all accepted; only a v2-only **magnet** is refused at
fetch time, because a magnet arrives without the piece layers a v2-only torrent
needs to verify anything (see "Piece layers" below and
`docs/findings-v2-magnet.md`).

## Discovery profiles

`--discovery` decides which network branches a run may use. `public` is the
default and preserves the behaviour of earlier builds; `direct` is the
explicit-peers-only profile. The value is validated before anything else
happens, and an unknown value is an error — there is no fallback.

| Branch | `public` | `tracker` | `direct` |
|--------|----------|-----------|----------|
| Explicit `--peer` addresses | dialled | dialled | dialled (the only source) |
| Tracker announce / scrape (magnet `tr=`, `.torrent` announce/announce-list) | yes | **yes** (the only source) | **no** |
| DHT (and its bootstrap lookup) | yes | **no** | **no** — no DHT server is created |
| PEX (advertising and accepting) | yes | **no** | **no** |
| Webseeds (magnet `ws=`, `.torrent` url-list) | no (pinned off since the first slice) | no, and the URLs are stripped from the spec | no, and the URLs are stripped from the spec |
| Metainfo sources (magnet `xs=`/`as=`, HTTP fetch) | library default | **no** — stripped from the spec | **no** — stripped from the spec |
| Peer addresses inside a magnet (`x.pe`) | dialled | **no** — stripped from the spec | **no** — stripped from the spec |
| Local service discovery (BEP 14 multicast) | off (opt-in, never enabled) | off | off |
| UPnP / NAT-PMP port mapping | off | off | off |
| Listener | all interfaces | all interfaces | `127.0.0.1` only; IPv6 and uTP off |
| Inbound connections | accepted | accepted (tracker peers must reach us) | loopback only |

`tracker` is the shape a BEP 27 private swarm needs: the torrent's own trackers
are the only peer source. It is a peer-discovery policy, not a statement about
the torrent — allowed for a public torrent as a stricter choice, and required
for a private one.

### Private torrents (BEP 27)

The pinned library already refuses the three discovery channels for a torrent
whose info dictionary sets `private=1`:

| Path | Where the pin gates it |
|------|------------------------|
| DHT announce and peer lookup | `torrent.go` `dhtAnnouncer` re-checks `isPrivate()` every loop, including after a magnet's metadata arrives |
| PEX, both directions | `pexconn.go` `pexConnState.Init` returns before enabling the extension |
| Local service discovery, both directions | `client.go` `TorrentInfohashesAndPort` and `OnLPDAnnouncement` skip private torrents |

It does **not** gate: webseeds (`url-list`), metainfo sources (`xs=`/`as=` on a
magnet), or inbound connections. Provenance closes the first two by policy in
both `tracker` and `direct`, and disables webseeds in every profile.

Because that library behaviour is not our contract, Provenance decides it
explicitly instead of inheriting it:

- A local `.torrent` that sets `private=1` is **refused** under `public`
  (`ErrPrivateTorrentPolicy`) before a job record, a lock or a dial exists. The
  error names `tracker` and `direct` as the profiles that may handle it.
- A **magnet** cannot be classified before its metadata arrives, so a private
  magnet fetched under `public` fails once the metadata shows `private=1`, with
  a message saying discovery already ran while the metadata was being fetched
  and cannot be undone. Such a swarm has to be fetched with `--discovery
  tracker` (or `direct` with explicit peers) from the start.
- `torrent inspect` reports `private` in its file listing, so the profile can be
  chosen before anything is dialled.

### Tracker credentials and resume state

A private swarm's announce URLs usually carry a passkey. The rules the adapter
keeps:

- The **job record** stores the source as a bare identity (`magnet:?xt=urn:btih:…`),
  never a tracker URL. The same is true of capture manifests and capture items.
- The **persisted metainfo** at `.provenance/torrent/<hash>.torrent` is the
  permission-restricted original/resume copy: it is written `0600` and it does
  carry the announce list, because a resume has to announce again on the same
  terms. It is not a public artifact.
- A `direct` run persists a metainfo with no announce list at all, so resume
  state from a direct run carries no credentials (see "Discovery profiles").

The audit is a test (`TestTrackerCredentialsStayOutOfJobAndCapture`), not a
claim: it plants a synthetic passkey in the announce URL, proves the URL was
really used by the announce the local tracker receives, and then walks every
file the fetch produced. Only the 0600 metainfo copy contains it.

### How it is enforced

Two layers, both applied before the library can act on the value:

1. **Client switches, before construction.** `clientConfig` builds the library
   config and applies `applyClientDiscovery` last, so the client never exists in
   a state where a forbidden discoverer could be switched off later. In the
   pinned `v1.62.0`: `NoDHT` is what stops the DHT server (and its bootstrap
   lookup) from being created at all; `DisableTrackers` stops pending announce
   updates and scrapes; `DisablePEX` keeps the `ut_pex` extension from being
   advertised *and* drops inbound PEX messages; `ListenHost` + `DisableIPv6`
   make the listener `127.0.0.1`, and `DisableUTP` + `NoDHT` mean no UDP socket
   is opened.
2. **Spec sanitisation, before `AddTorrentSpec`.** `admitSpec` is the single
   entry point for a torrent into a session, and it strips trackers, webseeds,
   metainfo sources, DHT node hints and magnet-supplied peer addresses in direct
   mode. It is used by all three sources — a fresh magnet, a local `.torrent`,
   and **the resume path** that rebuilds a spec from the persisted metainfo — so
   a restart cannot reintroduce a tracker the run is not allowed to use.

Only additive spec fields are touched. `InfoBytes`, the v1/v2 hashes and the
piece layers are identity and content, and no discovery policy edits them.

A consequence worth knowing: because the spec is sanitised before the client
builds its view of the torrent, a direct run **persists** a metainfo with no
announce list. Resume state from a direct run therefore carries no tracker URL
(and no passkey a tracker URL might contain). The reverse is not true — a public
run persists the announce list it was given — so mixing profiles over one job
keeps whatever the first run wrote.

### What `direct` does not claim

- **Not anonymity.** Peers still see this host's address; the profile is about
  which endpoints this client is willing to contact first, not about hiding
  where it is.
- **Name resolution still happens for `--peer`.** Those addresses are the
  caller's instruction, so the system resolver may be used for them. Nothing
  else in a direct run resolves a name, because no tracker, webseed, metainfo
  source or bootstrap node exists to resolve. Pass literal addresses if even that
  lookup matters.
- **IPv4 only.** With IPv6 and uTP off there is no socket to dial an IPv6 peer
  on, so a direct run cannot use one.
- **Not a private-tracker mode.** BEP 27 asks for a tracker to be the *only*
  discovery mechanism, which is the opposite of direct. A tracker-only profile,
  and the redaction that goes with it, is BT-03 in the roadmap; until then the
  two settings must not be conflated.
- **Per run, not sticky.** The job record format is unchanged, so the profile is
  a property of the invocation: `fetch --discovery direct` followed by a plain
  `fetch` is a public run. Nothing about a job's identity, selection or capture
  key depends on which profile fetched it.

### What is verified, and how

| Claim | Evidence |
|-------|----------|
| direct opens no UDP socket and binds loopback only | `TestDirectPolicyAdmitsNoUDPAndBindsLoopback`, with a public client as the control (it must show a UDP socket and a non-loopback listener, or the test refuses to conclude anything) |
| direct never announces to a tracker | `TestDirectMagnetFailsBoundedWithoutContactingTracker`, `TestDirectInspectMagnetDoesNotContactTracker`, `TestDirectFetchAndRepairStayOffTracker` — each counts requests against a live local tracker, and `TestTrackerTripwireIsArmed` proves the tripwire answers an enabled client |
| direct never fetches from a webseed | `TestDirectFetchNeverUsesWebseed`; `TestWebseedTripwireIsArmed` proves the tripwire serves a real download (60 KB over HTTP ranges) when webseeds are enabled |
| magnet `x.pe` is not dialled | `TestDirectIgnoresMagnetPeerAddresses` (times out; the same magnet resolves when the peer is passed with `--peer`) |
| resume cannot reintroduce a tracker | `TestOpenAppliesDiscoveryPolicyToResumeState` (client view and persisted metainfo, both profiles), `TestDirectFetchAndRepairStayOffTracker` (fetch → corrupt → bounded failure → repair) |
| a bad profile is refused before any work | `TestNewRejectsInvalidDiscoveryBeforeTouchingTheFilesystem`, `TestTorrentFetchRejectsUnknownDiscoveryBeforeWork` |

Not verified at the packet level: DHT, PEX and local-discovery traffic are
covered by the config mapping plus the runtime absence of the sockets those
branches need, not by a packet capture. PEX in particular has no wire-level test
here, because a conforming peer would have to speak `ut_pex` to exercise it; the
claim rests on the extension not being advertised and inbound messages being
dropped in the pinned library.

## Identities: v1, v2 and hybrid

A local `.torrent` file may be v1 (SHA-1 infohash), v2 (BEP 52, SHA-256) or
hybrid (both). `Identity` keeps the two hashes **separately and in full**:

| Kind | `info_hash` | `info_hash_v2` | Job id | Persisted metainfo |
|------|-------------|----------------|--------|--------------------|
| v1 | 40 hex | — | `bt-<40 hex>` | `<v1 hex>.torrent` |
| v2-only | — (never the SHA-1 of the info bytes) | 64 hex | `bt2-<64 hex>` | `<v2 hex>.torrent` |
| hybrid | 40 hex | 64 hex | `bt-<40 hex>` (canonical) | `<v1 hex>.torrent` |

Consequences worth stating:

- **v1 jobs, capture keys and status lookups are unchanged.** The job id, the
  idempotency key and the metainfo file name for a v1 torrent are byte-identical
  to what earlier builds wrote, so existing records resume and reconcile.
- **A hybrid is one job.** It validates both hashes and is recorded under its v1
  id, so it captures once. A lookup by the full v2 hash resolves that job
  (read-only): `bt2-<64 hex>` and the bare 64-hex form are accepted by
  `torrent status`, and neither creates, renames nor rewrites anything. There is
  no index to fall out of sync; resolution is a scan of the job records.
- **The library's short hash is never an identity.** `Torrent.InfoHash()` returns
  a 20-byte value even for a v2 torrent (a truncation of the v2 hash). The
  adapter uses it only where the library requires a 20-byte key — its
  piece-completion database and payload namespace — and derives identity from the
  exact metainfo bytes instead.
- **Piece layers are part of resume state.** A v2 torrent needs its top-level
  `piece layers` table to verify any piece. Persisted metainfo is written from
  `Torrent.Metainfo()`, which includes them; a metainfo without layers is refused
  (`ErrUnsupportedIdentity`, "carries no piece layers") rather than resumed into
  an unverifiable state, and a piece the library reports as
  `MissingPieceLayerHash` fails the transfer instead of waiting forever.
- **Conflicts are refused.** The library stores payload under the torrent's name
  and keys completion by the short hash, so a second, different identity in the
  same staging root would share bytes. That is `ErrIdentityConflict`, not silent
  reuse.

### Magnets: btih, btmh, hybrid

A magnet may name a v1 hash (`urn:btih`), a v2 hash (`urn:btmh`, multihash
`1220…`) or both. The library's parser validates the multihash (SHA2-256,
32 bytes) and rejects a second hash of either kind; the adapter rejects a magnet
with neither.

| Magnet | Resolves to | Fetch |
|--------|-------------|-------|
| btih only | whatever the metadata is: v1, or a hybrid | supported |
| btmh only on a v2-only torrent | v2-only (`bt2-<v2>`) | **refused**: piece layers exist only in `.torrent` files, so no piece can be verified |
| btmh only on a hybrid | hybrid, canonical `bt-<v1>` | supported (v1 piece hashes verify) |
| btih + btmh | hybrid; both hashes must match the metadata | supported |

Metadata is resolved **before** the durable job id is chosen, so a hybrid named
by a btmh-only magnet lands in its canonical `bt-<v1>` job instead of creating a
`bt2-` record that would later have to be renamed. A record is enriched with the
v2 identity that metadata revealed; a recorded hash is never replaced.

Piece layers: BEP 9 metadata exchange transfers the `info` dictionary only, so a
magnet-resolved torrent arrives without the top-level `piece layers` table. BEP 52
does define peer-wire `hash request` / `hashes` / `hash reject` messages for
piece-layer retrieval, but the pinned library does not implement a client for
them on this path (`setInfoBytesLocked(…, nil)`). The limitation is therefore in
the current integration, not in the protocol: Provenance does not obtain the v2
piece hashes it would need for a v2-only magnet fetch, and that case stays
inspection-only until proven otherwise.

That last claim was tested and partly overturned in a bounded spike: v1.62.0 does
implement BEP 52 hash request/receive/validation, and `Torrent.Metainfo()`
serialises the acquired hashes into top-level piece layers, so a v2-only magnet
can be verified and restarted. What blocks the case today is Provenance's own
missing-layer admission rule, not the library. See `docs/findings-v2-magnet.md`
for refs, measurements and the GO conditions. No production behaviour changed. A magnet-originated hybrid therefore has **no** piece layers,
which is fine because its info dictionary carries v1 piece hashes and the library
prefers those for verification (`Piece.Storage` takes `v1HashBytes` first). A
v2-only magnet has no v1 fallback, so `Fetch` fails with
`ErrUnsupportedIdentity` ("v2-only torrent has no piece layers … use the
.torrent file") before any job record or payload storage is created.
Inspection still works for it: identity and file list come from the metadata and
nothing is implied about readiness.

BEP 47 padding entries (`attr` containing `p`) are layout, not content: they are
excluded from the file listing and cannot be selected or captured, while the
underlying piece layout still includes them. `isPaddingFile` keys on the
attribute, never on a `_padding/`-style name.

Independent metainfo interoperability is covered by
`testdata/hybrid-torrenttools.torrent`, generated with torrenttools v0.6.2 (see
`testdata/RECIPE.md`). It confirms identities, structure and offline inspection
against a foreign encoder. It also documents a real limit: torrenttools emits a
hybrid whose v1 `pieces` cover the continuous layout with no padding entries,
and the pinned library lays files out piece-aligned for any v2 torrent, so its
own verification rejects that fixture. That is a generator/interoperability
limitation, not an adapter path; fetch-level independent interoperability and
padded hybrids remain unproven until a generator emits pad-aligned hybrid
metainfo.

A contradictory dual-identity magnet is never accepted. Both hashes are checked
against the metadata; the library rejects metadata that fails either, so the
observable failure is a metadata timeout naming the contradictory pair rather
than a generic deadline, and no job is adopted and nothing is published.

## Metadata first: inspection and admission

The order below is the contract this slice implements, and the tests assert each
step rather than the end state alone.

### Inspection

| Step | Local `.torrent` | Magnet |
|------|------------------|--------|
| Source parse | `metainfo.LoadFromFile` + size cap | `metainfo.ParseMagnetUri`, v1 only |
| Client | **none** — no listener, no temp dir | temp client under a private temp dir |
| Storage | never opened | deferred (`AddTorrentOpts.DeferStorageOpen`) |
| Transfer | n/a | `DisallowDataDownload` + `DisallowDataUpload` from creation |
| Identity | `requireV1Identity` before any path use | infohash must match, then `requireV1Identity` |
| Layout | `validateInfoPaths` on the parsed info | `validateInfoPaths` on the received info |
| Cleanup | n/a | client closed, temp dir removed on success, error, timeout and cancellation |

An inspection client uses an in-memory piece completion and a private data
directory, so nothing is created in a staging root — or anywhere else that
survives the call.

### Fetch admission

1. **Metadata arrives** with storage deferred and both data directions
   disallowed (`AddTorrentOpts` at creation, plus explicit `Disallow*` calls for
   a torrent the client already knew).
2. **Identity**: source infohash matches the metadata infohash; `requireV1Identity`
   rejects v2-only and hybrid info.
3. **Layout**: every file path component is validated, duplicate paths are
   rejected, and for a `.torrent` file this happened before the library saw it.
4. **Selection**: every requested path must exist exactly; the size limit is
   checked before any transfer.
5. **Staging paths**: every path this job could write through (all files, not
   only selected ones, because a piece can span files) is checked for an
   existing symlink.
6. **Storage opens** (`Torrent.OpenStorage`) — the first point at which a payload
   directory, part file or verified piece can exist.
7. **Selection is applied** (selected files `Download`, everything else
   `PiecePriorityNone`).
8. **Transfer is enabled** (`AllowDataDownload`, `AllowDataUpload`).
9. **Verification**: retained pieces are re-hashed, then completion requires
   every covering piece to be complete and idle and the file to exist finalized
   with the metainfo length.
10. **Capture**: bytes are re-hashed into the capture items and the run manifest.

A rejected layout or selection therefore never opens payload storage; the tests
assert that with a storage spy (`spyStorage`) rather than by looking for files
afterwards.

### What the library does and does not guarantee

Verified against the pinned `v1.62.0` source, and reproduced by
`TestClientConstructionCreatesDataDir` and the inspection tests:

- `storage.NewFile(dir)` — the default storage a client builds — calls
  `pieceCompletionForDir`, which does `MkdirAll(dir)` and opens
  `<dir>/.torrent.db` **inside `at.NewClient`**. Client construction therefore
  touches the data directory independently of `DeferStorageOpen` and of
  `OpenStorage`. That is why inspection never points a client at a staging root,
  and what `torrent fetch` inherits (the DB is created when the session starts,
  not when storage opens).
- `Torrent.setInfo` opens payload storage the moment metainfo arrives *unless*
  `deferStorageOpen` is set. `DeferStorageOpen` lives on `AddTorrentOpts`, not on
  `ClientConfig`, so the adapter builds a `TorrentSpec` to set it; `AddMagnet`
  alone cannot.
- New files start at `PiecePriorityNone`, but that is not relied on as the gate:
  priorities can be raised by other paths, so both `DisallowDataDownload` and
  `DisallowDataUpload` are set from creation.
- The library's default file backend joins metainfo components with
  `filepath.Join`, rejects a result that escapes the torrent directory
  (`isSubFilepath`, checked at storage open), but does not sanitise names and
  does not look through symlinks; `storage.ToSafeFilePath` is used only by the
  mmap backend. Confinement is therefore ours: validation runs before storage
  exists, every write path is symlink-checked at admission, and the library's
  own escape check is a second line rather than the first.

**Limitations, stated rather than papered over:** the symlink check is a check,
not an atomic guarantee — a process that can write inside the staging root while
a fetch runs could swap a component between the check and the library's write.
The staging lock is what keeps a second *provenance* process out; it cannot stop
an unrelated process. Payload writes are also not individually re-validated by
the library, so a `.part`/final rename performed by the library is trusted to
the extent that the paths were validated beforehand.

## Safety model

- **Hostile paths.** Torrent name and every file path component are validated
  against traversal (`.`, `..`), absolute paths, path separators, NUL bytes,
  invalid UTF-8, Windows drive letters, reserved device names (`CON`, `LPT1`),
  trailing dot/space, over-long components, and duplicate/colliding paths. A
  single bad component rejects the whole torrent — never a silent rename.
- **Validated before storage opens.** A `.torrent` file is parsed and validated
  before it is handed to the library, and every payload storage open is deferred
  until the layout, the selection and the staging paths have passed admission, so
  a rejected torrent creates no payload directory, part file or completed piece.
  The staging root itself and the client's piece-completion database are created
  when a fetch session starts, before the source is even parsed, because the
  session owns them; only payload storage is gated.
- **Symlinks.** Existing symlinks anywhere on a path the job could write through
  are rejected before storage opens (including unselected neighbours of a shared
  piece), and the final read for hashing re-resolves the path and requires it to
  stay inside staging.
- **Selection is explicit.** No flag downloads a whole torrent. An unknown path
  is an error, and the CLI refuses to fetch without `--select`.
- **Only verified selected files are captured.** Every piece covering a selected
  file must be in the library's completed-and-idle state, the file must exist
  under its final name with the metainfo length, and the bytes are hashed again
  by Provenance before the capture item is written.
- **No secrets in records.** The stored source reference is a magnet carrying
  only the infohash; trackers (which can contain passkeys) and local paths do not
  reach job records, capture items, or terminal output.
- **Discovery is a policy, not a hint.** `--discovery direct` is applied to the
  client config before the client exists and to every spec before the library
  sees it, so no discoverer can be constructed and switched off later, and the
  resume path is covered by the same code. An unknown profile is refused before
  the staging root is created. See "Discovery profiles".
- **No remote surface.** No listener for control, no API, no RPC. The client
  only speaks the peer protocol. In `direct` the peer listener is bound to
  loopback, so even that surface is local.

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

Ownership excludes other writers, not readers. `archive import`,
`manifest verify`, `torrent status` and `torrent inspect` do not take the lock,
so they can run while a job is active. Inspection builds no `Session` at all
(see "Metadata first" above), so it cannot contend, queue or leave a lock file.
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

### Corrupt retained data

Corruption is detected at the piece, not at the file. A selected file that
shares a piece with an unselected neighbour is the interesting case: damaging
bytes anywhere in that piece invalidates it, so the file's length and the cached
piece-completion record both still look fine while the bytes are not. The next
run re-hashes every retained piece, marks the damaged piece incomplete, and
re-fetches exactly that piece; the intact pieces are reused. Capture hashes the
finalized file again, so damaged bytes never reach a capture item even if some
earlier check were wrong.

Measured behaviour (`TestSharedPieceCorruptionIsRepairedNotCaptured`): a 48 KiB
selected file plus a 16 KiB neighbour at 32 KiB pieces, with four bytes damaged
in the shared piece, repairs by reading at most two pieces from the peer —
strictly less than the 64 KiB payload — and then reconciles to the same capture
key, so the historical run manifest is neither duplicated nor rewritten.

With no peer the attempt is bounded by the caller's context: it fails with
`ErrTransferTimeout`, publishes no run manifest, leaves the payload state
`pending` rather than `ready`, and leaves the historical capture record
byte-identical (`TestUnrepairableCorruptionPublishesNothing`).

### Storage failures are not silent

Two library behaviours shape what the adapter can report:

- A failed **chunk write** goes to `Torrent.onWriteChunkError`. The library's
  default handler logs it and disables data download, so a caller sees a
  transfer that simply never finishes. The adapter installs its own handler
  (`SetOnWriteChunkError` after admission, before any write can happen),
  records the first error, stops the transfer and fails with
  `ErrStorageFailure` wrapping the cause.
- A failed **part-file promotion** (`MarkComplete` → rename) is only logged
  (`error marking piece complete`), and the piece is still marked complete in
  memory. The observable state is: every covering piece complete and idle, but
  the final file never appears. The adapter distinguishes this from "still
  downloading" and reports `ErrNotFinalized` — "pieces are complete but the file
  was never finalized" — after `finalizeGrace` (2 s), instead of waiting for the
  transfer deadline.

Both are tested with a storage double that fails writes / refuses to promote,
which is a deterministic stand-in for a full disk or a read-only mount — not an
OS error test. Removing the fault and retrying publishes exactly one capture.

## Limits

| Knob | Default | Where |
|------|---------|-------|
| Selected bytes | unlimited | `--max-bytes` |
| Overall time | unlimited | `--timeout` |
| Metadata wait | 2 minutes | fixed default (`Config.MetadataTimeout`) |
| Peers per torrent | library default (50) | CLI sets 40 (`MaxPeers`) |
| Metainfo file size | 16 MiB | fixed cap |
| Discovery profile | `public` | `--discovery public|direct` (per run) |

Disk usage follows the selection plus shared edge pieces; there is no quota
enforcement yet.

## Out of scope (deliberately)

- Durable multi-job queue, backend resume references, crash-boundary
  reconciliation (`PR 2` in the plan).
- Seeding policy beyond opt-in `--seed`.
- Web seeds (disabled in both discovery profiles), WebTorrent (disabled).
- A tracker-only / private-swarm profile (BEP 27) and original-metainfo
  redaction: `--discovery direct` is the opposite of a tracker-only mode, and
  the two are not interchangeable.
- HTTP API, event stream, TUI integration.
- aria2 RPC, session import/export, `.aria2` checkpoints.
- Soulseek / slskd.
- Archive schema changes: the capture manifest and vault types are untouched.

## Tests

`internal/transfer/bittorrent` runs a local in-process seeder over loopback, and
the discovery tests use local tracker and webseed servers, so no test contacts
anything outside this host — CI never reaches a public tracker or swarm. The
public-profile controls deliberately leave discovery enabled, but only against
loopback endpoints. Covered:

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
| `magnet_v2_test.go` | v2-only magnet inspection (identity + files, zero payload bytes from the seeder, staging untouched) and a fetch refused with the piece-layer blocker before a job exists; hybrid reached by btih, btmh, dual and local `.torrent` all reconciling to one canonical job, one capture key and one manifest, including a no-peer reopen; contradictory dual magnets (wrong v1 and wrong v2) adopting nothing and publishing nothing; synthetic identities with equal StorageKey refused by the conflict check with no storage side effects |
| `v2_test.go` | v2-only `.torrent` inspected offline (no v1 hash invented, no magnet source); v2 fetch from a loopback seeder interrupted and restarted from the persisted metainfo with its piece layers, resume measured by bytes read, identity surviving status; hybrid identity checked against independently computed SHA-1 and SHA-256 of the info bytes, canonical v1 job, `bt2-` and bare-v2 alias resolution without mutation, one capture after restart, mismatched identity refused; layer-less metainfo fails closed with no capture; two same-named v2 torrents in one root rejected as a conflict |
| `corruption_test.go` | shared-piece corruption with real persisted state (same length, completion DB untouched): repaired by re-fetching at most the damaged piece (measured bytes), captured hash matches the fixture, one run manifest, reconciled; no-peer attempt is bounded, publishes nothing and leaves the historical capture byte-identical; injected chunk-write failure → `ErrStorageFailure`; injected failed promotion → `ErrNotFinalized` after the grace, nothing published; cancellation during verification and during finalization publishes nothing, closes cleanly and hands ownership to the next run |
| `inspect_test.go` | local `.torrent` inspected with a client-construction counter stuck at 0; magnet inspection resolves metadata while the seeder reports 0 payload bytes and 0 chunks written; no temp dir survives; inspection leaves an existing staging root byte-identical; rejection before storage open for unknown selection, path collision and symlinked payload path (storage spy count 0), and exactly one open on the admitted path; metadata timeout and cancellation clean up; v2/hybrid refused for inspect and fetch |
| `ownership_process_test.go` → `TestInspectDoesNotNeedStagingOwnership` | a separate process inspects a magnet while another process owns the staging root, writes nothing there, and does not disturb ownership |
| `status_process_test.go` (real CLI, real processes) | `torrent status` against a root owned by a seeding process returns promptly with the recorded capture, leaves the tree unchanged, and does not disturb ownership (a competing fetch is still busy); after `SIGKILL` of that owner, status reports the last recorded state as recorded while a lock file still exists, and a later fetch acquires the root and reconciles |
| `private_tracker_test.go` | BEP 27: the tracker profile's client config and spec (trackers kept, everything else stripped, identity untouched); a private `.torrent` refused under `public` with no job record, no payload and no tracker contact; `inspect` reporting `private`; a private torrent fetched under the tracker profile through a peer announced to a local tracker; announce-list tier failover (dead tier, then the live one); a synthetic passkey audit that walks every file a fetch produces |
| `discovery_test.go` | profile parsing and validation; the client-config mapping for both profiles; the spec sanitisation (and that it leaves identity bytes untouched); a direct client opens no UDP socket and binds loopback only, against a public client as control; a live tracker tripwire that announces when trackers are enabled and is never touched in direct mode (fetch, inspect, resume and repair); a webseed tripwire that serves a real ranged download when enabled and is never touched in direct mode; magnet `x.pe` ignored in direct mode; resume state stripped of trackers and webseed URLs in direct mode but not in public; invalid profiles refused before the staging root or a temp client exists |
| `cmd/provenance` torrent tests | `--discovery` is inherited by `inspect`/`fetch`, an unknown value is rejected naming both valid values, and `fetch` refuses it before creating the staging root |
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
5. Private-swarm policy (BEP 27): a tracker-only profile, redaction of the
   persisted metainfo, and separation of restricted originals from sanitised job
   metadata. `--discovery direct` is not that profile and must not be presented
   as one.
6. Cross-client interoperability (BT-02): this slice proves self-to-self loopback
   transfers plus a loopback *webseed*; real peer-wire interoperability with
   Transmission/qBittorrent and padded hybrids is still unproven.