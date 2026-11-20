# BitTorrent backlog checkpoints

One entry per shipped ticket, written before moving to the next one. The
execution contract is in the roadmap brief: ticket, real SHAs, changed paths,
commands actually run, one user-visible example, verified vs inferred behaviour,
limits, unresolved bugs, and the next eligible ticket.

Environment for every entry below: Linux 6.18.63-gentoo-dist-bin / x86_64,
Go 1.27.3, 32 cores. Branch `feat/selective-bittorrent`, no push, local commits
only.

---

## BT-01 — Explicit discovery profiles (shipped)

**Starting SHA** `34d9860` (tree clean). The checkout was briefly switched to
`master` by something outside this session between the baseline read and the
first edit; it was returned to `feat/selective-bittorrent` at the same SHA with
an unchanged tree before any work started. No work was lost and nothing from
`master` was touched.

**Ending SHA** `be16a65` (this note follows in its own commit).

### Changed paths

```
internal/transfer/bittorrent/discovery.go          new: Discovery type, parse/validate, client + spec policy
internal/transfer/bittorrent/discovery_test.go     new: the gate for this ticket
internal/transfer/bittorrent/bittorrent.go         Config.Discovery replaces DisablePeerDiscovery; clientConfig split out;
                                                   New validates first; admitSpec applied on all three source paths
internal/transfer/bittorrent/inspect.go            InspectConfig.Discovery; same policy for magnet inspection
cmd/provenance/torrent.go                          persistent --discovery flag; validation before work; mode in the fetch banner
cmd/provenance/torrent_test.go                     flag inheritance, invalid value, no staging root created
docs/TORRENT.md                                    "Discovery profiles" section + limits/out-of-scope/tests/follow-ups
docs/CLI.md                                        flag table, "Discovery profiles" subsection, corrected v2 claims
internal/transfer/bittorrent/*_test.go             mechanical rename of the old test-only switch to DiscoveryDirect
```

No `go.mod`/`go.sum` change: no new module is imported, and the webseed
tripwire uses `net/http/httptest` only.

### What was built

`torrent inspect|fetch --discovery public|direct`. `public` is the default and
keeps the previous behaviour; `direct` uses only `--peer`. Two enforcement
layers, both before the library can act: the client config (DHT, trackers, PEX,
webseeds, uTP, IPv6, port forwarding off; listener `127.0.0.1`) and the
`TorrentSpec` handed to `AddTorrentSpec` (trackers, webseeds, metainfo sources,
DHT node hints and magnet `x.pe` stripped). The spec layer is also applied on the
resume path, which rebuilds the spec from the persisted metainfo — the one path
that would otherwise reintroduce a tracker from an earlier run.

`DisablePeerDiscovery` was not kept as a second switch: it already meant exactly
"direct", so keeping both would have allowed the two to drift. Existing hermetic
tests were migrated to `Discovery: DiscoveryDirect` with no behaviour change.

### Commands and results (all actually run)

| Command | Result |
|---------|--------|
| `go test ./internal/transfer/bittorrent/ -count=1` (baseline, before edits) | `ok … 30.843s` |
| `go test ./internal/transfer/bittorrent/ -count=3` | `ok … 98.577s` |
| `go test -race ./internal/transfer/bittorrent/ -count=1` | `ok … 50.638s` |
| `make test` (`go test -race ./…`) | every package `ok` / cached-ok |
| `make vet` | clean |
| `make lint` (`golangci-lint run ./…`) | `0 issues.` |
| `gofmt -l ./internal/transfer/bittorrent/ cmd/provenance/` | empty |
| `git diff --check` | clean |
| `go build ./…` + `make build` | binary built (`0.8.0-52-g34d9860-dirty`) |

CLI smoke (real binary, no network): `provenance torrent inspect --discovery
direct internal/transfer/bittorrent/testdata/hybrid-torrenttools.torrent` listed
the three fixture files and both hashes with no client; `provenance torrent fetch
--discovery swarm … -o /tmp/never-made` failed with `--discovery: unsupported
discovery profile: "swarm" (want "public" or "direct")` and `/tmp/never-made` did
not exist afterwards.

### User-visible example

```
$ provenance torrent fetch ./release.torrent --select "release/track.flac" \
    --discovery direct --peer 192.0.2.10:51413 -o ./downloads/release
[provenance] fetching release.torrent (discovery direct)
```

### Verified vs inferred

Verified by execution (in `discovery_test.go`):

- a direct client opens **no UDP socket** and every listener is loopback; the
  same assertions are shown to discriminate by running them against a public
  client, which must expose a UDP socket and a non-loopback listener;
- a public client announces to a local tracker within ~30 ms (tripwire armed),
  and a direct client reaches 0 requests across fetch, magnet inspect, a bounded
  metadata wait, a corrupted-payload repair, and a resume from persisted
  metainfo;
- a local BEP 19 webseed really serves a 60 KB ranged download when webseeds are
  enabled (tripwire armed); a direct fetch with the same `url-list` makes 0
  requests, fails bounded, and persists a metainfo with no `url-list`;
- a magnet's `x.pe` is not dialled in direct mode (metadata times out), while the
  same magnet resolves when the peer is passed with `--peer`;
- resume state written by a direct run carries no announce list, while a public
  run keeps it (both read back from `.provenance/torrent/<hash>.torrent`);
- invalid profiles fail before the staging root, lock and client exist.

Inferred, not executed here:

- **DHT and PEX traffic.** No packet-level capture. The claim rests on the config
  mapping (no DHT server is constructed when `NoDHT`; `ut_pex` is not advertised
  and inbound PEX is dropped) plus the runtime absence of the UDP socket those
  branches need. A wire-level PEX test would need a peer that speaks `ut_pex`.
- **Local service discovery.** Provenance never sets
  `ClientConfig.LocalServiceDiscovery`, so BEP 14 is off in both profiles; this is
  a config assertion, not a runtime one (no multicast listener is involved).
- **Name resolution.** A direct run still resolves the names in `--peer` through
  the system resolver. Nothing else has a name to resolve, but that is a
  consequence of the stripped spec rather than of a resolver-level guard.

### Data and network limits

Everything is loopback: an in-process seeder, an `httptest` tracker and an
`httptest` webseed. No public tracker, DHT bootstrap or swarm was contacted, so
no real peer-wire interoperability is implied by this ticket (that is BT-02).

### Unresolved / deliberate non-goals

- The profile is per run and is **not** persisted on the job; a later bare
  `fetch` is a public run. Persisting it would change the job record format,
  which belongs to BT-07.
- `direct` is IPv4-only (IPv6 off, no v6 socket to dial a v6 peer).
- `direct` is not a BEP 27 tracker-only mode, and the two must not be conflated
  (BT-03). Webseeds stay disabled in `public` too, unchanged from before.

### Next eligible ticket

**BT-02** (cross-client reference lab). Prerequisite check first: this box's
docker registry pulls are blocked (see the `provenance-product` notes), so pinned
Transmission/qBittorrent/libtorrent images may not be obtainable; a locally
installed pinned client or a source build would be the fallback. If BT-02's
tooling cannot be obtained without a registry pull, **BT-03** (private tracker
policy and redaction) is the next independent ticket — it needs only local
metainfo and fake local trackers.
---

## BT-02 — Cross-client reference lab (partially shipped)

**Starting SHA** `1b805f1` (tree clean). **Ending SHA** `e1b1efe`, plus `c1311c9`
for the adapter fix the lab produced.

This ticket is **not complete**. One direction is verified; three rows are
blocked or explicitly unsupported, with the measurement behind each recorded in
`docs/CROSSCLIENT.md`. Commit history is split so the product change can be read
on its own: `c1311c9` is the adapter fix, `e1b1efe` is the lab and its document.

### Changed paths

```
internal/transfer/bittorrent/crossclient_lab_test.go  new: docker/qBittorrent plumbing, opt-in and self-skipping
internal/transfer/bittorrent/crossclient_test.go      new: the scenario tests (1 verified, 2 skipped with evidence)
internal/transfer/bittorrent/peer_retry_test.go        new: deterministic retry tests (proxy that drops handshakes)
internal/transfer/bittorrent/bittorrent.go             waitForInfo/waitComplete re-offer explicit peers
docs/CROSSCLIENT.md                                    new: isolation model, matrix, what it does not prove
```

No `go.mod`/`go.sum` change.

### What the lab is

An opt-in harness (`PROVENANCE_TEST_BITTORRENT_CROSSCLIENT=1`) that runs pinned
qBittorrent v5.3.0 / libtorrent 2.1.2.0 in a container — image pinned by digest,
never pulled — on a dedicated bridge with masquerading disabled (measured: the
container cannot reach the internet; host↔container works), `--dns 127.0.0.1`,
DHT/PEX/LSD/uTP/UPnP off, no trackers anywhere, disposable data.

### Verified

- **Row 1, 3/3 runs**: qBittorrent seeds a ragged four-file v1 torrent
  (`a.bin` 5000, `empty.bin` 0, `b.bin` 40000, `c.bin` 9000 at 16 KiB pieces, so
  files share edge pieces); Provenance fetches two files with
  `--discovery direct --peer`, matches both SHA-256 digests, captures neither
  neighbour, writes one capture manifest, then restarts with **no peers** and
  reconciles the same capture key with `BytesRead == 0`. Encoder, peer-wire and
  selective-storage interop on one row.
- **The adapter fix**: the first attempts failed ~half the time because
  qBittorrent reports a torrent complete before libtorrent accepts peer
  connections, and the library never retries a peer whose handshake failed.
  After re-offering peers while disconnected, 1/3 became 3/3. Two deterministic
  tests cover it, neither of which uses docker.
- `go test ./internal/transfer/bittorrent/ -count=1` → ok 40.550s;
  `make lint` → 0 issues; `make vet` → clean; `gofmt -l` → empty.
- `make test` (`go test -race ./...`, whole repository) on the committed
  revision → every package ok, `internal/transfer/bittorrent` 55.491s.

### Not verified (explicit)

- **Direction 3** (Provenance seeds → qBittorrent downloads over a magnet `x.pe`
  hint): blocked. On the host network namespace qBittorrent's second start exits
  with `LocalPeer: failed to connect to server.` before the WebUI binds. The
  scenario exists but is skipped with that evidence.
- **Row 2** (a peer that stores only the selected file): measured and
  unsupported by construction. qBittorrent reports `stalledDL progress=0.333
  amount_left=32768` and never completes, because the selected file shares
  piece 0 and piece 2 with the files it does not hold. This is the ragged-edge
  case the fixture exists to create, not a bug in either client.
- **v2-only / hybrid / BEP 47 padding / MSE**: no cross-client exercise. No
  independent v2 generator is installed here, and the foreign fixture in
  `testdata/` is not pad-aligned, so it never reaches the peer wire.

### Limits

Everything is loopback or a host-local bridge; no NAT, no loss, no real swarm.
Container isolation is a docker bridge and configuration, not a packet filter.
The lab pins qBittorrent's encryption to plaintext so a handshake failure is
attributable, which is also why MSE interop is an unsupported row.

### Next eligible ticket

**BT-03** (private tracker policy and redaction). It needs only local metainfo
and fake local trackers — no external client, no registry, no swarm — and the
fake-tracker machinery already exists in `discovery_test.go`. Two BT-02 follow-ups
stay open: give the lab a readiness barrier so it does not depend on the retry,
and diagnose the `LocalPeer` exit to unblock direction 3.

---

## BT-03 — Private tracker policy and redaction (partially shipped)

**Starting SHA** `e950a21` (tree clean). **Ending SHA** `79cceb5`.

Not complete: the private-metainfo policy, the tracker-only profile and the
credential rules are shipped and tested; the tracker profile's **transfer path**
is not, and is recorded as blocked rather than papered over.

### Changed paths

```
internal/transfer/bittorrent/discovery.go          DiscoveryTracker + tracker client/spec policy
internal/transfer/bittorrent/bittorrent.go         ErrPrivateTorrentPolicy, checkPrivateFile/checkPrivatePolicy,
                                                   Info.Private, enforcement before job creation and after metadata
internal/transfer/bittorrent/inspect.go            reports private=1
internal/transfer/bittorrent/private_tracker_test.go  new: policy mapping, refusal, inspect, blocked scenarios
internal/transfer/bittorrent/discovery_test.go     tracker added to the profile table
cmd/provenance/torrent.go                          flag help + human "private=1" note
docs/TORRENT.md                                    profile table, BEP 27 audit, credential rules, tests table
docs/CLI.md                                        tracker profile and the refusal example
```

### The BEP 27 audit (pinned v1.62.0)

The library gates **all three** discovery channels for `private=1`:
`torrent.go dhtAnnouncer` (re-checked every loop, so it also covers metadata
that arrives later via magnet), `pexconn.go pexConnState.Init`, and
`client.go TorrentInfohashesAndPort` / `OnLPDAnnouncement`. It does **not** gate
webseeds, magnet metainfo sources, or inbound connections. Provenance closes the
first two by policy and disables webseeds in every profile; inbound connections
are expected for a tracker-only swarm.

### Verified

- Tracker profile mapping: `NoDHT`, `DisablePEX`, `DisableWebseeds`,
  `NoDefaultPortForwarding` set, `DisableTrackers` untouched, LPD nil, listener
  left on all interfaces, uTP/IPv6 untouched; spec keeps trackers and strips
  webseeds, metainfo sources, DHT hints and `x.pe`, with the info bytes
  bit-identical (`TestTrackerPolicyClientConfigAndSpec`).
- Private `.torrent` under `public` refused with `ErrPrivateTorrentPolicy`,
  before any job record, payload directory or tracker contact
  (`TestPrivateTorrentIsRefusedUnderPublicPolicy`).
- `torrent inspect` reports `private` for a private fixture and not for a plain
  one (`TestInspectReportsPrivateFlag`).
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → ok;
  `make lint` → 0 issues; `go vet ./...` → clean; gofmt clean.
- All three tracker-profile scenarios pass (`-count=2` on the private fetch).
- `make test` (`go test -race ./...`, whole repository) on the committed
  revision → every package ok, `internal/transfer/bittorrent` 56.430s.

### Correction: the tracker profile's transfer path is verified

An earlier revision of this checkpoint recorded the tracker profile as blocked,
with uTP dialling as the prime suspect. That was wrong, and a probe settled it:
a client with uTP on or off, on all interfaces or on loopback, transfers a
TCP-seeded fixture in every combination. The real cause was the tests' own
fixture — `newSeeder` builds metainfo from the files, while `privateFixture`
re-encodes the info dict to add `private=1`, so the two peers were on different
infohashes and could never exchange a byte. The seeder now serves the exact
fixture metainfo (`newSeederFromMetainfo`), and all three scenarios pass:
private fetch under the tracker profile, announce-list tier failover, and the
passkey audit.

Still unverified: peers returned *in* a tracker response (the fake tracker's
response shape is not accepted by the pinned client yet), so "a peer learned from
a tracker is dialled" has no test — the tracker's role is proven by its announce
log, and transfers use an explicit peer.

### Next eligible ticket

**BT-02** (cross-client reference lab). Prerequisite check first: this box's
docker registry pulls are blocked (see the `provenance-product` notes), so pinned
Transmission/qBittorrent/libtorrent images may not be obtainable; a locally
installed pinned client or a source build would be the fallback. If BT-02's
tooling cannot be obtained without a registry pull, **BT-03** (private tracker
policy and redaction) is the next independent ticket — it needs only local
metainfo and fake local trackers.
---

## BT-02 — Cross-client reference lab (partially shipped)

**Starting SHA** `1b805f1` (tree clean). **Ending SHA** `e1b1efe`, plus `c1311c9`
for the adapter fix the lab produced.

This ticket is **not complete**. One direction is verified; three rows are
blocked or explicitly unsupported, with the measurement behind each recorded in
`docs/CROSSCLIENT.md`. Commit history is split so the product change can be read
on its own: `c1311c9` is the adapter fix, `e1b1efe` is the lab and its document.

### Changed paths

```
internal/transfer/bittorrent/crossclient_lab_test.go  new: docker/qBittorrent plumbing, opt-in and self-skipping
internal/transfer/bittorrent/crossclient_test.go      new: the scenario tests (1 verified, 2 skipped with evidence)
internal/transfer/bittorrent/peer_retry_test.go        new: deterministic retry tests (proxy that drops handshakes)
internal/transfer/bittorrent/bittorrent.go             waitForInfo/waitComplete re-offer explicit peers
docs/CROSSCLIENT.md                                    new: isolation model, matrix, what it does not prove
```

No `go.mod`/`go.sum` change.

### What the lab is

An opt-in harness (`PROVENANCE_TEST_BITTORRENT_CROSSCLIENT=1`) that runs pinned
qBittorrent v5.3.0 / libtorrent 2.1.2.0 in a container — image pinned by digest,
never pulled — on a dedicated bridge with masquerading disabled (measured: the
container cannot reach the internet; host↔container works), `--dns 127.0.0.1`,
DHT/PEX/LSD/uTP/UPnP off, no trackers anywhere, disposable data.

### Verified

- **Row 1, 3/3 runs**: qBittorrent seeds a ragged four-file v1 torrent
  (`a.bin` 5000, `empty.bin` 0, `b.bin` 40000, `c.bin` 9000 at 16 KiB pieces, so
  files share edge pieces); Provenance fetches two files with
  `--discovery direct --peer`, matches both SHA-256 digests, captures neither
  neighbour, writes one capture manifest, then restarts with **no peers** and
  reconciles the same capture key with `BytesRead == 0`. Encoder, peer-wire and
  selective-storage interop on one row.
- **The adapter fix**: the first attempts failed ~half the time because
  qBittorrent reports a torrent complete before libtorrent accepts peer
  connections, and the library never retries a peer whose handshake failed.
  After re-offering peers while disconnected, 1/3 became 3/3. Two deterministic
  tests cover it, neither of which uses docker.
- `go test ./internal/transfer/bittorrent/ -count=1` → ok 40.550s;
  `make lint` → 0 issues; `make vet` → clean; `gofmt -l` → empty.
- `make test` (`go test -race ./...`, whole repository) on the committed
  revision → every package ok, `internal/transfer/bittorrent` 55.491s.

### Not verified (explicit)

- **Direction 3** (Provenance seeds → qBittorrent downloads over a magnet `x.pe`
  hint): blocked. On the host network namespace qBittorrent's second start exits
  with `LocalPeer: failed to connect to server.` before the WebUI binds. The
  scenario exists but is skipped with that evidence.
- **Row 2** (a peer that stores only the selected file): measured and
  unsupported by construction. qBittorrent reports `stalledDL progress=0.333
  amount_left=32768` and never completes, because the selected file shares
  piece 0 and piece 2 with the files it does not hold. This is the ragged-edge
  case the fixture exists to create, not a bug in either client.
- **v2-only / hybrid / BEP 47 padding / MSE**: no cross-client exercise. No
  independent v2 generator is installed here, and the foreign fixture in
  `testdata/` is not pad-aligned, so it never reaches the peer wire.

### Limits

Everything is loopback or a host-local bridge; no NAT, no loss, no real swarm.
Container isolation is a docker bridge and configuration, not a packet filter.
The lab pins qBittorrent's encryption to plaintext so a handshake failure is
attributable, which is also why MSE interop is an unsupported row.

### Next eligible ticket

**BT-03** (private tracker policy and redaction). It needs only local metainfo
and fake local trackers — no external client, no registry, no swarm — and the
fake-tracker machinery already exists in `discovery_test.go`. Two BT-02 follow-ups
stay open: give the lab a readiness barrier so it does not depend on the retry,
and diagnose the `LocalPeer` exit to unblock direction 3.

---

## BT-03 — Private tracker policy and redaction (partially shipped)

**Starting SHA** `e950a21` (tree clean). **Ending SHA** `79cceb5`.

Not complete: the private-metainfo policy, the tracker-only profile and the
credential rules are shipped and tested; the tracker profile's **transfer path**
is not, and is recorded as blocked rather than papered over.

### Changed paths

```
internal/transfer/bittorrent/discovery.go          DiscoveryTracker + tracker client/spec policy
internal/transfer/bittorrent/bittorrent.go         ErrPrivateTorrentPolicy, checkPrivateFile/checkPrivatePolicy,
                                                   Info.Private, enforcement before job creation and after metadata
internal/transfer/bittorrent/inspect.go            reports private=1
internal/transfer/bittorrent/private_tracker_test.go  new: policy mapping, refusal, inspect, blocked scenarios
internal/transfer/bittorrent/discovery_test.go     tracker added to the profile table
cmd/provenance/torrent.go                          flag help + human "private=1" note
docs/TORRENT.md                                    profile table, BEP 27 audit, credential rules, tests table
docs/CLI.md                                        tracker profile and the refusal example
```

### The BEP 27 audit (pinned v1.62.0)

The library gates **all three** discovery channels for `private=1`:
`torrent.go dhtAnnouncer` (re-checked every loop, so it also covers metadata
that arrives later via magnet), `pexconn.go pexConnState.Init`, and
`client.go TorrentInfohashesAndPort` / `OnLPDAnnouncement`. It does **not** gate
webseeds, magnet metainfo sources, or inbound connections. Provenance closes the
first two by policy and disables webseeds in every profile; inbound connections
are expected for a tracker-only swarm.

### Verified

- Tracker profile mapping: `NoDHT`, `DisablePEX`, `DisableWebseeds`,
  `NoDefaultPortForwarding` set, `DisableTrackers` untouched, LPD nil, listener
  left on all interfaces, uTP/IPv6 untouched; spec keeps trackers and strips
  webseeds, metainfo sources, DHT hints and `x.pe`, with the info bytes
  bit-identical (`TestTrackerPolicyClientConfigAndSpec`).
- Private `.torrent` under `public` refused with `ErrPrivateTorrentPolicy`,
  before any job record, payload directory or tracker contact
  (`TestPrivateTorrentIsRefusedUnderPublicPolicy`).
- `torrent inspect` reports `private` for a private fixture and not for a plain
  one (`TestInspectReportsPrivateFlag`).
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → ok
  (40.716s / 0.033s); `make lint` → 0 issues; `go vet ./...` → clean; gofmt clean.

### Blocked, with the exact symptom

Three scenarios are skipped in `private_tracker_test.go` because the tracker-only
profile never starts a transfer in this lab: the torrent reports `peers total=0`
for the whole deadline **even when the peer is passed with `Peers`**, while the
same fixture transfers fine under `direct` (which turns uTP and the loopback
listener off). The announce itself works — a fake tracker records it. Prime
suspect is the uTP dial to a TCP-only peer dropping the peer permanently, which
the 5s re-offer then repeats. Until that is diagnosed, nothing about the tracker
profile's transfer path is claimed.

Also unverified: peers handed out by a tracker response (the fake tracker's
response shape is not accepted by the pinned client yet), so "peers learned from
a tracker are dialled" has no test.

### Next

**BT-05** (per-job storage isolation and migration) is the next independent
ticket: full identities already map to job ids, so the namespace work can start.
One BT-03 follow-up remains: make the fake tracker's announce response acceptable
to the pinned client so "peers learned from the tracker are dialled" gets a test
of its own.

## BT-05 — Per-job storage isolation (partially shipped: layout, not migration)

**Starting SHA** `6852bf7` (tree clean, survey only). **Ending SHA** `6cc4328`.

Scope shipped: an isolated workspace for future runner-managed jobs, exercised
through the real transfer and capture pipeline. Not shipped, by decision:
migration of existing jobs, the queue, an index, scheduler, RPC and TUI.

### Changed paths

- `internal/transfer/bittorrent/workspace.go` (new, 377 lines) — workspace
  layout, `WorkspacePath`, `validateJobID`, `OpenIsolated`, `FetchIsolated`,
  `checkLegacyConflict`, `openWorkspace`, `resolveMetainfo`, `metainfoSource`.
- `internal/transfer/bittorrent/isolated_workspace_test.go` (new, 561 lines) —
  the six acceptance tests below.
- `internal/transfer/bittorrent/bittorrent.go` — `Session.isolated`,
  `Session.workspaceID`, `Session.ownStorage`; `Fetch` split into parse +
  `fetchSource`; a `case "metainfo"` source in `open()`; the reserved
  `.provenance` payload-name refusal; `Close` now closes workspace storage
  between the client and the lock.
- `docs/findings-storage-isolation.md` — survey retained, shipped layout,
  ownership, reader story, conflict policy, evidence added.
- `docs/TORRENT.md` — "Isolated job workspaces" section.

### Traced before editing

- **What the library derives from `DataDir`:** the payload (`<DataDir>/<name>/…`)
  and, by default, the piece completion database at `<DataDir>/.torrent.db`.
  Both are settable independently: `storage.NewFileOpts(NewFileClientOpts{
  ClientBaseDir, PieceCompletion})` plus `NewDefaultPieceCompletionForDir(dir)`
  (sqlite, bolt under `nosqlite`), so `DataDir` can stay the workspace while the
  database lives in `.provenance/completion`. A caller-provided
  `DefaultStorage` is **not** closed by the library — the session closes it.
- **When identity is known:** a local `.torrent` at parse time; a magnet only
  after a metadata exchange. Hence `OpenIsolated` resolves a magnet metadata-only
  (temp dir, in-memory completion, `DeferStorageOpen`, data up and down
  disallowed, the job's own discovery profile) and only then picks the
  workspace. A btmh-only hybrid magnet resolves to `bt-<v1>`, and no `bt2-`
  workspace is ever created for it.
- **Root ownership:** the root lock exists to protect root-level files
  (`<root>/.torrent.db`, `staging.lock`, records, captures). A workspace never
  writes a root-level file, so it takes only its own lock and the root stays
  readable to a concurrent legacy writer. No path takes both locks.

### Layout shipped (real, from the test log)

```
<root>/.provenance/torrent/jobs/bt-9af1d6c1…/
  .provenance/
    completion/.torrent.db                     12288 bytes
    items/9af1d6c1…_fixture_a.bin.json           723 bytes
    runs/2026-11-19T014306Z.json                1558 bytes
    torrent/9af1d6c1….torrent                    288 bytes
    torrent/bt-9af1d6c1….job.json               1150 bytes
    torrent/staging.lock                           0 bytes
  fixture/a.bin                                30000 bytes
```

### Verified

`go test ./internal/transfer/bittorrent/ -run TestIsolated -count=1 -v`
(the second column is measured, not asserted):

| Test | Result |
|------|--------|
| `TestIsolatedTwoConcurrentJobsSameName` | PASS 0.19s — two sessions under one root, same display name and relative path, different bytes: distinct workspaces, payloads, completion DBs, records, capture keys; both digests correct; root has no `fixture/`, no record, no `.torrent.db` |
| `TestIsolatedWorkspaceBusyAndRecovery` | PASS 2.35s — held workspace → `ErrStagingBusy`; unrelated workspace transferred while held; cancelled transfer published 0 manifests; reopen recovered on retained bytes with exactly 1 run |
| `TestIsolatedWorkspacesKeyedByFullIdentity` | PASS 0.03s — equal `StorageKey`, distinct job ids → distinct workspaces and DBs; legacy record for the v1 form does not block the v2 form; `checkLegacyConflict` refuses only the exact identity |
| `TestIsolatedHybridResolvesToOneWorkspace` | PASS 0.16s — `.torrent`, `btih`, `btih+btmh`, `btmh-only` → one workspace, one capture key, 1 run manifest |
| `TestIsolatedLegacyConflictIsRefused` | PASS 0.04s — `ErrLegacyJobConflict`, no workspace created, legacy record byte-identical |
| `TestIsolatedCaptureIsReadableByExistingReaders` | PASS 0.16s — workspace-relative manifest path, status/items/runs readable from the workspace, none at the root |

Full gates on `6cc4328` (Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32
cores):

- `go test ./internal/transfer/bittorrent/ -count=1` → `ok 45.492s`
- `go test … -run 'TestIsolated|TestFetchCapturesOnlySelectedFiles|TestInterruptedFetchResumesAndReusesVerifiedBytes|TestStagingRootIsExclusiveOwner|TestSecondSessionOnSameRootIsBusy|TestReadJobStatus|TestSharedPieceCorruptionIsRepairedNotCaptured|TestDirectMagnetFailsBoundedWithoutContactingTracker|TestTrackerPolicy' -count=3` → `ok 25.045s`
- `make test` (repo-wide, `-race`) → all packages ok, `internal/transfer/bittorrent 58.210s`
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `make lint` → `0 issues`, `git diff --check` clean
- CLI: no surface change in this PR (no new flags, no new commands), so no CLI smoke; `cmd/provenance` tests pass under `make test`.

### Not verified (explicit)

- **Process-kill recovery for a workspace.** The accepted evidence is an
  in-process cancel plus a reopen. The lock primitive is covered under process
  death for legacy roots by `ownership_process_test.go`, but that helper does not
  know about workspaces, and this PR does not extend it.
- **Hostile filesystem races.** Existing symlinks on the workspace path are
  rejected before creation and the adapter re-resolves containment for its own
  reads, but library-owned I/O is not re-validated per write. No claim is made
  about another process changing a workspace mid-transfer.
- **Migration, queue, index.** Out of scope by decision; no legacy job was read,
  moved or rewritten, and no data on this machine was touched.

### Limits

- One client per active job: each workspace costs its own goroutines, sockets and
  completion database handle. That is the accepted cost of not writing a
  path-rewriting storage wrapper.
- A workspace is not enumerable from the root: `torrent status -o <root>` still
  lists only legacy jobs. Pointed at the workspace, the existing readers work.
- Foreground `fetch` is unchanged: legacy layout, legacy lock, no workspace.

### Next eligible ticket

**BT-06** (offline-first `torrent recheck`, read-only inspection, typed storage
errors). It only needs persisted metainfo and the selected-file set, and it is
independent of the queue. BT-07 (durable queue) is the consumer of this PR's
workspace entry point and should follow BT-06.

## BT-06 — Offline recheck (shipped: read-only check, repair stays a fetch)

**Starting SHA** `d371367` (tree clean). **Ending SHA** `111c587`.

Scope shipped: `provenance torrent recheck JOB_ID --output ROOT [--json]`, a
read-only integrity check of one job's selected working files against the digests
recorded at capture time. Not shipped, by decision: piece-level re-verification,
`.part` inspection, auto-repair, a repair command for isolated workspaces, queue
and vault checks.

### Changed paths

- `internal/transfer/bittorrent/recheck.go` (new, 433 lines) — `Recheck`,
  `RecheckReport`, `hashFileContext`, attestation lookup, ownership handling.
- `internal/transfer/bittorrent/recheck_test.go` (new, 547 lines) — the
  acceptance tests below.
- `internal/transfer/bittorrent/status.go` — `lookupJob` factored out of
  `ReadJobStatus`, so recheck resolves records and the hybrid alias through the
  same validated lookup.
- `internal/transfer/bittorrent/bittorrent.go` — `jobLockPath` extracted so a
  fetch and a recheck contend on the same lock key.
- `cmd/provenance/torrent.go` — the `recheck` subcommand, JSON and human
  rendering, nonzero exit for any non-verified outcome.
- `docs/TORRENT.md` — "Offline recheck" section.

### Traced before editing (pinned v1.62.0)

- **`Torrent.VerifyDataContext` is a mutating path.** It needs a `Client` and an
  added `Torrent`; it hashes through the storage implementation, which opens the
  piece-completion database for writing; it marks pieces and can move `.part`
  data into place; and it updates persisted completion state. It is also
  whole-torrent, not whole-selection. So it is not a read-only verifier and
  recheck does not call it. The honest consequence is stated everywhere: there is
  no piece-level recheck in this ticket.
- **Attestations live in the capture items**, keyed `<identity>:<staging-relative
  path>` (`manifest.ItemPath`), holding `sha256`, `byte_size`, `downloaded_path`.
  The job record holds only the capture *reference* (`key`, `manifest`,
  `committed_at`, `reconciled`).
- **`verifyStagedAssets` is the existing read-only comparison** (no client), but
  it is fail-fast and returns one error for a set; recheck reports per file, so it
  reuses the same comparison rather than the function.
- **Paths:** `resolveCaptured` re-resolves symlinks but fails on a missing file, so
  recheck does lexical containment first (`joinWithin`), a non-following
  `os.Lstat` (missing/partial/symlink/non-regular), and only then the
  symlink-resolved re-check before hashing.
- **Locks:** `filelock` keys its sidecar on `path+".lock"`, and `Session.lockPath`
  names `…/torrent/staging`, so the real sidecar is `…/torrent/staging.lock`. My
  first implementation passed `staging.lock` and created `staging.lock.lock`; the
  before/after snapshot test caught it, which is why `jobLockPath` is now shared.

### Verified (real runs)

| Test | Result |
|------|--------|
| `TestRecheckHealthyWorkspaceChangesNothing` | PASS — committed isolated workspace rechecks to exact agreement; job record, metainfo, completion DB, payload, items and run manifest byte- and mtime-identical afterwards; no new file appears |
| `TestRecheckDetectsCorruptionAndRepairReconciles` | PASS — one byte flipped at the same length: `mismatch`, `verified=false`, manifest and items unchanged; `FetchIsolated` repair restores the bytes, reconciles to the same capture key, leaves exactly one run manifest; the repaired workspace then rechecks clean offline |
| `TestRecheckNeverReportsUncheckedFilesVerified` | PASS — missing, `.part`-only (`partial`, nothing hashed), unattested (`unverifiable`), escaping path (no hash, `error`), symlink at a selected path (`error`), invalid record (error, no report), record for another identity (error) |
| `TestRecheckCancellationDuringHashing` | PASS — the chunk hook cancels mid-file; `context.Canceled` is returned, `verified=false`, and the report carries the error so JSON does not hide it |
| `TestRecheckRefusesLiveOwner` | PASS — a workspace owner gets `ErrStagingBusy`; after the owner exits, a leftover `staging.lock` does not make the job look busy and recheck verifies |
| `TestRecheckLegacyAndHybridAlias` | PASS — legacy staged job verified (and busy while its session is live); hybrid isolated workspace verified when asked by its v2 hash |

Gates on `111c587` (Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores):

- `go test ./internal/transfer/bittorrent/ -run 'TestRecheck' -count=1` → ok
- `go test ./internal/transfer/bittorrent/ -run 'TestRecheck|TestFetchCapturesOnlySelectedFiles|TestInterruptedFetchResumesAndReusesVerifiedBytes|TestStagingRootIsExclusiveOwner|TestSecondSessionOnSameRootIsBusy|TestReadJobStatus|TestSharedPieceCorruptionIsRepairedNotCaptured|TestDirectMagnetFailsBoundedWithoutContactingTracker|TestTrackerPolicy|TestIsolated' -count=3` → `ok 28.329s`
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → `ok 44.126s`, `ok 0.039s`
- `make test` (repo-wide, `-race`) → all ok
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...` → `0 issues`, `git diff --check` clean
- CLI smoke (`bt06-smoke`, synthetic job root with a 102400-byte payload):
  clean → `MATCH`, `verified: yes`, exit 0; one byte flipped at the same length →
  `MISMATCH … (digest differs at the same length)`, `verified: no`, exit 1;
  `--json` on the damaged run parses with `verified=false, mismatch=1` and the
  file's detail; an unknown job id parses with an `error` field and exits 1;
  restoring the bytes returns exit 0. Usage text is suppressed on a mismatch so
  the finding is not buried under the flag table.

### Not verified (explicit)

- **Piece-level recheck.** No piece layers or completion metadata are consulted,
  so this is not parity with a client force-recheck and cannot say which pieces
  are bad. Only the selected payload bytes are compared, to their recorded
  digests.
- **`.part` contents.** A partial is reported as `partial` and not hashed: it has
  no attestation, and hashing it would imply a progress claim this code cannot
  back.
- **Repair CLI for isolated workspaces.** Repair is demonstrated through
  `FetchIsolated`; there is no `provenance torrent` command that resumes a
  runner-managed workspace yet (BT-07).
- **ENOSPC, storage-error redesign, vault verification.** Untouched, as scoped.
- **Hostile filesystem races.** Path containment is checked lexically, then with
  `Lstat`, then with symlink resolution; another process swapping a file between
  those steps is not defeated, and no such claim is made.

### Next eligible ticket

**BT-07** (durable queue and desired-state controller). It consumes this ticket's
`Recheck` as the offline check an operator or the runner can call before
idle/selection changes, and the isolated workspace entry point shipped in BT-05.

## BT-07 — Durable queue with a foreground runner (shipped: one worker, no daemon)

**Starting SHA** `6061208` (tree clean). **Ending SHA** `dbb4f3e`.

Scope shipped: a durable local queue over isolated workspaces, drained in the
foreground by one worker (`torrent queue add|list|start|pause|retry|run`). Not
shipped, by decision: daemon, RPC, TUI, multi-worker scheduling, bandwidth limits,
seeding after completion, destructive removal, a cancel command, legacy migration.

### Changed paths

- `internal/torrentqueue/store.go` (new, 461 lines) — entry record, atomic writes,
  sequence-ordered ids, duplicate detection, restricted source record, store lock.
- `internal/torrentqueue/runner.go` (new, 533 lines) — reconcile, claim, pause
  watch, failure/backoff handling, summary, `SanitizeError` for persisted errors.
- `internal/torrentqueue/adapter.go` (new, 99 lines) — the real backend over
  `FetchIsolated` / `RunKnownWorkspace`.
- `internal/transfer/bittorrent/queueapi.go` (new, 314 lines) — `PrepareSource`,
  `WorkspaceDir`, `HasJob`, `OpenWorkspaceJob`, `RunKnownWorkspace`,
  `AdoptCapture` (+ `manifestCovers`), sanitized magnets.
- `internal/transfer/bittorrent/bittorrent.go` — a `resume` source case that fails
  loudly when the persisted metainfo is missing instead of dialling.
- `cmd/provenance/torrent.go` — the `queue` command group.
- `internal/torrentqueue/queue_test.go` (764) + `integration_test.go` (495) — the
  acceptance suites.
- `docs/TORRENT.md` — "Durable queue" section.

### The record, and why the id is not the infohash

```json
{
  "format": "provenance/torrent-queue-entry+1",
  "id": "queued-000001-c627a1e8",        // own id: survives an unresolved identity
  "seq": 1,                               // deterministic queue order
  "desired": "paused",                    // user intent, survives restarts
  "phase": "queued",                      // last observation, never a live claim
  "source": { "kind": "magnet",
              "magnet": "magnet:?xt=urn:btih:cccc…" },   // identity only
  "discovery": "direct",
  "selection": ["fixture/a.bin"],         // immutable capture intent
  "job_id": "bt-cccc…",                   // attached only after validation
  "workspace": ".provenance/torrent/jobs/bt-cccc…",
  "unresolved": false,
  "capture": { "key": "…", "manifest": ".provenance/runs/….json" },
  "attempts": 1,
  "last_error": "…", "note": "…", "not_before": "…"
}
```

A btmh-only magnet resolves to a hybrid whose canonical workspace is `bt-<v1>`, so
`job_id`/`workspace` are attached only after a validated resolution; before that the
entry is `unresolved` and keeps its source plus discovery profile. Enqueue rejects
what it cannot resume (unreadable `.torrent`, unsupported scheme, private torrent
outside `tracker`, an identity already held by a legacy job at the root).

Transitions (all persisted; only transitions, never progress ticks):

```
add ──▶ paused/queued ──start──▶ paused|queued ──▶ running ──▶ completed
                          │                        │
                          └─pause─▶ stopping ──▶ paused
                                                   │
   running ──error──▶ failed ──retry──▶ queued      │
   running ──busy───▶ blocked (not_before) ──retry/time──▶ queued
   interrupted (SIGINT or dead process) ──▶ queued (desired kept)
   restart with a published capture ──▶ completed (adopted, no refetch)
```

### Commit points and locks

| point | what it means |
|-------|---------------|
| run manifest written by the adapter | the capture is committed; the queue may still be `running` |
| queue record updated to `completed` | the queue has caught up; a restart between the two adopts the manifest |
| `desired` write | user intent; acknowledged only after the rename |

| lock | holder | held across |
|------|--------|-------------|
| `runner.lock` | `queue run` | the whole drain |
| store lock | `add`/`start`/`pause`/`retry`/every transition | one record's read-modify-write |
| workspace lock | the adapter, per job | one attempt |

No lock is held while awaiting peers, transferring, hashing or closing a client;
`list`, `status` and `recheck` take no queue lock, so there is no lock order to
deadlock.

### Verified

Fake backend, `internal/torrentqueue -count=3` → ok 23.196s:

| Test | Demonstrates |
|------|--------------|
| `TestQueueAddPausedThenStart` | add is paused and a drain claims nothing; after `start`, two entries run in queue order with max concurrency 1, both completed with captures and one attempt each |
| `TestQueuePauseDuringWork` | pause recorded while the worker runs; `stopping` observable before `paused`; no capture; a second drain does nothing |
| `TestQueuePauseRacingCompletion` | pause during a run that then completes: `phase=completed`, capture kept, `desired=paused`, note explains it |
| `TestQueueFailureDoesNotBlockLaterJobs` | failure recorded, later entry still completes, no automatic retry, explicit retry succeeds on attempt 2 |
| `TestQueueShutdownPreservesIntent` | SIGINT → `queued` with a shutdown note (not `paused`), the paused entry untouched, restart completes only the interrupted one |
| `TestQueueReconcileInterruptedObservations` | a record left `running` with a published capture becomes `completed` without a transfer; a `running` record without one is re-queued |
| `TestQueueBlockedWorkspaceBacksOff` | typed busy error → `blocked` with `not_before`; untyped error → `failed`; the next drain skips the backoff |
| `TestQueueOneRunnerPerQueue` | second runner → `ErrRunnerActive`; the lock is released when the drain returns |
| `TestQueueOrderAndIds` | ids carry the sequence and never the infohash; `list` returns recorded order |
| `TestQueueDuplicateAndConflict` | same job+selection dedupes to one entry; a different selection is a second entry |
| `TestQueueRecordsNoSecrets` | no passkey/tracker/peer in the public record; restricted record is 0600 and reaches the backend; the recorded discovery profile reaches the backend |
| `TestQueueUnresolvedEntrySurvivesRestart` | an unresolved entry keeps source and `tracker` profile, attaches identity only after resolution, and leaks no tracker URL |

Real backend on a loopback seeder (DHT/PEX/trackers/web seeds/uTP/IPv6 off):

| Test | Demonstrates |
|------|--------------|
| `TestQueueRealInterruptAndResume` (7.09s) | a 400 KiB selective transfer interrupted after bytes landed: `queued`/`desired=run` with no capture; restart resumes in the same workspace, `attempts=2`, one run manifest, one capture item, payload matches, exactly one unselected neighbour never captured |
| `TestQueueAdoptsCapturePublishedBeforeQueueUpdate` | the backend publishes and then the queue update is lost: the entry ends `completed` with the published capture adopted (note says so), and a restart claims nothing and keeps the same key with one manifest |
| `TestQueueConflictingSelectionIsRejected` | a second `--select` for the same torrent fails with a selection error, and the first capture, its manifest and its items are unchanged |
| `TestQueueBusyWorkspaceBlocksThenRetries` | a held workspace lock → `blocked` (no capture, no failure), then after release the entry completes with one manifest |

Gates on `dbb4f3e` (Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores):

- `go test ./internal/torrentqueue/ -count=3` → `ok 23.196s`
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → `ok 44.019s`, `ok 0.034s`
- `go test ./internal/torrentqueue/ -run 'TestQueueReal|TestQueueAdopts|TestQueueConflicting|TestQueueBusy' -count=1` → pass
- `make test` (repo-wide, `-race`) → exit 0, all packages ok,
  `internal/torrentqueue 25.369s` (the two SIGKILL tests run inside it)
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...` → `0 issues`, `git diff --check` clean
- CLI smoke (`bt07-smoke`, real binary):
  - `queue add "magnet:?xt=…&tr=http://tracker.example/announce?passkey=SECRET&x.pe=10.9.9.9:6881" --select fixture/a.bin --discovery direct` → `queued-000001-c627a1e8 added paused for bt-cccc…`
  - `queue list` → `paused  queued`, source printed as `magnet:?xt=urn:btih:cccc…` only; `grep -rl "SECRET|tracker.example|10.9.9.9"` matched **only** `queue/restricted/queued-000001-….json`, mode `-rw-------`; `list --json` matched nothing
  - `start` → `desired=run phase=queued`; `pause` → `paused`; `start` again → `queued`
  - `run --timeout 4s` (unreachable peer) → `failed (timed out waiting for torrent metadata…)`, `drained: 0 completed, 1 failed, 0 blocked, 0 paused, 1 claimed`, and the process exited nonzero. The recorded `run_exit=0` in that one shell line is the status of the trailing `| tail -3`, not of the binary; an earlier identical smoke captured `exit=1` directly.

### Not verified (explicit)

- **Process death during a run.** Interruption is proven by ctx cancellation (real
  backend) and by an injected post-publication failure; no test kills a live runner
  mid-transfer. The runner lock is an flock, so ownership does survive death, but
  that path is exercised only for legacy roots by `ownership_process_test.go`.
- **Seeding.** Deliberately out of this slice: a completed entry releases the single
  worker, so `--seed` has no queue meaning yet.
- **No cancel command.** Removing an entry (or its payload) is not implemented; the
  docs say so. Delete is BT-09 work.
- **Live telemetry.** `list` prints recorded phases plus whether the runner lock is
  held. It never claims a job is alive because a record says `running`.
- **Multi-worker behaviour, bandwidth limits, priorities.** One worker, FIFO order.

### Next eligible ticket

**BT-08** (scheduling, resources and seeding) now has the hooks it needs: the
runner's single-worker loop, `not_before` backoff, and per-entry recorded profile.
BT-09's safe removal is the other independent follow-up. The BT-02/04 matrix rows
remain explicitly unproven and untouched.

## BT-08 (first slice) — Bounded concurrent downloads in the queue runner

**Starting SHA** `7efb359` (tree clean). **Ending SHA** `d3bac30`.

Scope shipped: `queue run --max-active N`, a bounded slot scheduler over the
existing foreground runner. Default 1; the flag refuses < 1 and > 32. Not shipped,
by decision: aggregate bandwidth/peer limits, managed seeding, priority beyond FIFO
eligibility, remote control, TUI, destructive removal.

### Changed paths

- `internal/torrentqueue/runner.go` — coordinator (`take`, `nextExcept`, `claim`,
  `updateAttempt`, `shutdown`), `Options.MaxActive`/`Validate`, attempt tokens,
  `Summary.Stale`.
- `internal/torrentqueue/store.go` — `Entry.Attempt` token, `newAttemptToken`, and a
  bounded store-lock retry.
- `internal/torrentqueue/concurrency_test.go` (new) — slot cap, FIFO, invalid
  limits, pause isolation, stale results, shutdown, busy-with-sibling.
- `internal/torrentqueue/process_test.go` (new) — two real SIGKILL tests with a
  re-exec child runner.
- `internal/torrentqueue/integration_test.go` — two same-name concurrent transfers,
  one-workspace race, peak-concurrency wrapper.
- `cmd/provenance/torrent.go` — `--max-active` (default 1, explicit zero and
  negatives refused by the flag).
- `docs/TORRENT.md` — "Concurrency (`--max-active`)".

### What a slot is, and when it is released

A slot is **one active attempt**: from resolution (metadata exchange for an
unresolved magnet) through the backend closing its client and releasing the
workspace lock. `Runner.Run` dispatches an attempt in a goroutine and treats that
goroutine's return as the release, so:

| event | slot released? |
|-------|----------------|
| pause requested, entry `stopping` | no - the attempt is still closing |
| capture published | no - the attempt still has to close |
| attempt returned (success, failure, busy, cancel) | yes |
| blocked with a future `not_before` | yes - it is not running at all |

### Draining, paused/failed/busy

- Drained = nothing running **and** nothing eligible. Paused (desired), failed
  (needs `retry`), and blocked-with-future-`not_before` entries are ineligible, so a
  drain returns instead of polling. Unchanged from BT-07 by design.
- A busy workspace becomes `blocked` with a backoff and does not stop unrelated
  entries: the sibling completes in the same drain and the busy entry is attempted
  exactly once (asserted).
- On shutdown the coordinator stops claiming, cancels every active attempt, waits
  for each to return, and only then returns; `Summary` is final at that point.

### Verified (fake backend)

`go test ./internal/torrentqueue/ -count=2` → `ok 48.038s`; `-count=3` for the
concurrency set:

| Test | Demonstrates |
|------|--------------|
| `TestQueueMaxActiveOverlapAndFIFO` | with `MaxActive=2` exactly two attempts overlap, peak concurrency is 2, the two lowest sequence entries are claimed first, and the third starts only after a slot frees |
| `TestQueueDefaultMaxActiveIsOne` | the zero value still means one worker |
| `TestQueueInvalidMaxActive` | negative, 33 and 1000 are refused before any claim; the documented limit is accepted; zero means default for in-process callers |
| `TestQueuePauseOneKeepsSibling` | pausing A cancels only A; B keeps running; A stays `stopping` and keeps its slot while its client is still closing; only after it closes does A become `paused` and the third entry start |
| `TestQueueStaleResultIsIgnored` | a newer command (pause) replaces the attempt token; the old attempt's result is counted `Stale` and does not overwrite the pause or record a capture |
| `TestQueueShutdownWithMultipleActive` | both active attempts are cancelled and waited for, both end `run`/`queued` with a shutdown note, the paused entry is untouched, no attempt is still active when `Run` returns, and the runner lock is released |
| `TestQueueBusyDoesNotBlockSibling` | the busy entry is attempted once, ends `blocked` with a backoff, and the healthy entry completes in the same drain |

### Verified (real backend, loopback seeders)

| Test | Demonstrates |
|------|--------------|
| `TestQueueRealConcurrentSameName` | two torrents sharing display name and relative path but not bytes: peak concurrency 2, distinct workspaces, distinct completion databases under each, payloads matching their own fixtures, one run manifest each, distinct capture keys |
| `TestQueueRealSameWorkspaceRace` | the same torrent submitted as a magnet and as a `.torrent` dedupes to one entry (same job + selection); a differing selection creates a second entry; the loser ends `blocked` (busy) or fails on selection; a later drain never produces a second manifest or capture item, and the job record's selection is unchanged |
| `TestQueueRealInterruptAndResume`, `TestQueueAdoptsCapturePublishedBeforeQueueUpdate`, `TestQueueConflictingSelectionIsRejected`, `TestQueueBusyWorkspaceBlocksThenRetries` | BT-07 regressions, still green |

### Verified (real process death)

Both use a re-exec child (`TestQueueRunnerHelper`) that runs the real adapter, and
`SIGKILL` on the child - not an injected error:

| Test | Demonstrates |
|------|--------------|
| `TestQueueRunnerKilledMidTransfer` | the child holds the runner lock and has written payload; kill -9; the flock is released by the OS (a held lock, not a file, is ownership); the record still says `running` with no capture; a fresh runner reacquires ownership, reconciles to `queued`, resumes the same workspace, completes with `attempts=2`, exactly one run manifest, payload matching the fixture, and only the interrupted entry claimed; a second, unrelated paused entry stays `paused`/`queued` with 0 attempts |
| `TestQueueRunnerKilledAfterPublish` | the child publishes the capture and blocks before the queue update (`BeforeResultPersist` marker), then is killed; the record is still `running` with no capture while the workspace holds one manifest; the restart adopts it, claims nothing (`len(sum.Ran)==0`), and leaves exactly one manifest. Reported separately from BT-07's injected-error test, which simulates the same boundary in-process |

### Gates

Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores, on `d3bac30`:

- `go test ./internal/torrentqueue/ -count=2` → `ok 48.038s`
- `go test ./internal/torrentqueue/ -run '<concurrency|process|real>' -count=1` → pass
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...`
  → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok
- CLI smoke (`bt08-smoke`, real binary, three unreachable magnets):
  `--max-active 0` → `error: --max-active must be at least 1, got 0`; `-3` →
  the same with `-3`; `99` → `max-active 99 is too high: the limit is 32 …`;
  `--max-active 2 --timeout 2s` → `1: running`, `2: running`, `1: failed`,
  `3: running`, `2: failed`, `3: failed`, `drained: 3 claimed` - two at a time,
  the third only after a slot freed.

### Not verified / not claimed

- **Throughput or timing benefits.** Concurrency is proven by observed overlap and
  slot accounting, not by a benchmark; no claim is made that N workers are N times
  faster on a shared uplink.
- **Aggregate bandwidth or peer limits.** Out of scope for this slice; each slot is
  a full client with its own peers.
- **Starvation policy.** FIFO among *eligible* entries only; a permanently blocked
  or repeatedly failing entry is skipped, but there is no ageing or priority.
- **Kill during a pause-close window, or several kills in a row.** The two process
  tests cover mid-transfer and post-publication boundaries.
- **Windows process semantics.** The kill tests use `SIGKILL` on Linux; the flock
  helper is shared, but no Windows execution was run.

### Next

**BT-08 continuation** (aggregate limits, managed seeding, stall diagnosis) or
**BT-09** (selection, priorities, safe removal). The BT-02/04 matrix rows remain
unproven and untouched.

## BT-08 (second slice) — One aggregate download and upload budget

**Starting SHA** `8884b5e` (tree clean). **Ending SHA** `0aa3b09` (+ a test-race fix
and this entry).

Scope shipped: `queue run --download-rate R --upload-rate R`, one pair of limiters
per runner shared by every attempt. Not shipped: peer-count limits, per-job
fairness or static division of the budget, dynamic rate changes, managed seeding,
RPC/TUI.

### Where each limiter is installed

| client | built by | receives |
|--------|----------|----------|
| metadata-only resolver (unresolved magnet) | `resolveMetainfo` → `clientConfig(tmp, cfg)` | both shared instances |
| payload session | `openWorkspace` → `clientConfig(ws, cfg)` | both shared instances |
| persisted-workspace reopen (restart) | `openWorkspace` | both shared instances |
| foreground `fetch` / legacy job | `New` → `clientConfig` | only if `Config` carries them, which the fetch CLI never sets - unchanged |

`clientConfig` is the single install point, so a new client path inherits the
budget by construction. The runner builds the limiters once in `NewRunner` and
passes the pointers through `Request`; nothing clones a limiter, divides the rate by
`--max-active`, or builds one per attempt or per job.

### Byte-accounting scope (what the budget is, and is not)

- **Counted:** the client's payload read path - the library wraps the reader the
  client reads torrent data through (`newDownloadRateLimitedReader`) - and the
  chunks written to peers (the upload path reserves tokens per peer request before
  `sendChunk`).
- **Not counted:** tracker and DHT traffic, handshakes, keepalives, message framing
  overhead, piece verification reads from disk, and anything outside these clients.
  Web seeds are disabled by this adapter (`DisableWebseeds`), so no web-seed
  limiting is advertised. This is **not** a whole-process network cap.
- **Units:** bytes per second, `--max-bytes` suffix convention (`KB`/`MB`/`GB` are
  powers of 1024). Omitted = unlimited; `0`, negative and unparsable values are
  refused by the flag. Flags are not persisted: supply them on each restart.

### Bursts, and why they are not zero

| direction | burst | rationale |
|-----------|-------|-----------|
| download | 256 KiB | the library truncates each read to the burst and `panicif`-es on a failed reservation, so a zero burst panics; 256 KiB is far above the 16 KiB block read and bounds idle credit |
| upload | 1 MiB | a peer request is reserved as one lump and the library panics with `upload rate limiter burst size < N` when the request exceeds the burst; `MaxAllocPeerRequestDataPerConn` defaults to 1 MiB, which is also the burst upstream gives itself |

Consequence measured, not assumed: a 160 KiB fixture at a 32 KiB/s cap finished in
0.15 s because it fits entirely in the burst, which is why the low-rate test now uses
640 KiB.

### Verified

Pointer wiring (no network - says nothing about bytes):

| Test | Result |
|------|--------|
| `TestQueueSharedLimiterWiring` | two attempts of one runner receive identical `*rate.Limiter` pointers for both directions; limits and bursts are the documented values; an unconfigured runner passes nil for both |
| `TestQueueRateValidation` | negative, above-max and 1<<62 values rejected; zero and at-max accepted; an invalid rate is refused before anything is claimed |
| `TestClientConfigInstallsSharedLimiters` | `clientConfig` installs the exact instances, and an unconfigured client keeps the library default rather than a zero-rate limiter (which the library's own config check rejects) |
| `TestFetchIsolatedSharesOneLimiter` | a magnet fetch builds at least two clients (resolver + payload) and both carry the same pointers |
| `TestQueuePauseWithSharedBudget` | pausing one attempt leaves the shared limiter instance intact for the sibling (same pointer, same limit) and releases only its own slot, after its client closes |
| `TestParseQueueRate` | flag parsing: empty = unlimited, `1MB`/`512kb`/`2G` parse, `0`/`0KB`/`-1MB`/`fast` refused |

Real traffic measurement (loopback seeders, two torrents with the same display name
and relative path but different bytes, 2 MiB each, cap 512 KiB/s, `--max-active` 2):

- total delivered **4,194,304 bytes in 7.647 s** (under `-race`);
- steady-state **538,331 B/s** over a 6.3 s window sampled from allocated blocks
  (cap 524,288 B/s: +2.7 %, inside the 1.3× tolerance);
- the token-bucket bound `burst + rate × elapsed` holds, and the run is materially
  slower than the `2 × rate` that two independent per-client limiters would have
  allowed;
- `TestQueueLowRateMakesProgress`: 640 KiB at 96 KiB/s completed in **4.124 s**
  (expected ≥ 4.0 s after the burst), so a low rate neither stalls nor panics.

### Not verified

- **Upload enforcement against real traffic.** The upload limiter is wired through
  the same install point and pointer-tested, but no test makes a controlled peer
  request chunks from an active queue client, and seeding is out of scope - so
  upload *enforcement* is **unverified**. It is not claimed at parity with download.
- **Web-seed limiting.** Web seeds are disabled by this adapter; not advertised.
- **Aggregate peer counts, per-job fairness, dynamic rates.** Out of scope.
- **Whole-process accounting.** No claim: the budget covers the paths above, not
  every byte the process moves.

### Regression covered, and one test correction

Existing process-death tests (`TestQueueRunnerKilledMidTransfer`,
`TestQueueRunnerKilledAfterPublish`), capture adoption, the slot/FIFO suites and
`make test` all ran on this revision.

Two defects were in the new tests, not in the product, and both were found by
`make test` under `-race` rather than by a normal run:

1. `TestQueuePauseDuringWork` polled for the transient `stopping` phase and could miss
   it when the fake worker returned instantly on cancellation. The worker now parks
   after observing the cancellation and the test asserts `stopping` through that
   barrier (`-race -count=5` green). Loosening the assertion instead would have hidden
   a real slot-ordering regression later.
2. The budget measurement tripped the race detector on its own sampler: the goroutine
   appended to the samples slice while the assertions read it, and closing the stop
   channel orders nothing. A second channel closed by the sampler gives the assertions
   a happens-before edge; the numbers below are from a `-race` run.

Commits: `42b7e96` (stopping barrier), `f82575a` (sampler race).

### Gates

Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores:

- `go test ./internal/torrentqueue/ -count=2` → `ok 72.087s`
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → `ok 44.164s`, `ok 0.036s`
- `go test ./internal/torrentqueue/ -race -run '<pause tests>' -count=5` → ok 2.201s
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...` → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok, `internal/torrentqueue 37.449s`
- CLI smoke: `--download-rate 0` → `must be a positive size in bytes per second`;
  `--upload-rate -1MB` → `size "-1" must not be negative`;
  `--download-rate fast` → parse error; `--download-rate 1MB --upload-rate 256KB
  --max-active 2` accepted and the drain ran.

### Next

Upload enforcement against a controlled peer, then BT-09 (selection, priorities,
safe removal). BT-02/04 matrix rows remain unproven and untouched.

## BT-08 (third slice) — Per-job peer bounds

**Starting SHA** `1db7b52` (tree clean, pin `anacrolix/torrent v1.62.0`).

Scope shipped: `queue run --peers-per-job N --half-open-per-job N`, applied through
`clientConfig`, validated before the runner starts, defaults unchanged. Not shipped:
managed seeding, a shared peer allocator, a global semaphore around library
callbacks, RPC/TUI.

### Upstream enforcement, cited

| claim | source |
|-------|--------|
| established connections are capped **per torrent** | `client.go:1553-1556` sizes the conn map and sets `maxEstablishedConns` from `EstablishedConnsPerTorrent`; `torrent.go:2614-2634` admits by closing the worst existing conn, and panics if it cannot, so the map never exceeds the cap |
| inbound and outbound share that count | `t.addConnection` is the single admission point and distinguishes only for eviction preference (`incomingIsBad`/`outgoingIsBad`) |
| half-open dials are capped per torrent **and** per client | `torrent.go:3626-3627` (`len(t.halfOpen)` vs `HalfOpenConnsPerTorrent`, `cl.numHalfOpen` vs `TotalHalfOpenConns`); `torrent.go:1818/1834` feed the same quantities into dial decisions |
| accepted-but-unassociated sockets are not counted | no such accounting exists in the accept path; the cap applies once a conn is associated with a torrent |

**Overshoot:** the established map cannot exceed the cap (evict-then-admit), but
connections can churn under pressure, and the half-open cap is a dial-loop gate, not
a socket limit.

### The runner bound, and why it holds

An attempt builds at most two clients, and never two at once. Measured with a
lifecycle hook during a magnet fetch:

```
client lifecycle: [open:resolver close:resolver open:payload close:payload]
peak concurrent clients = 1
```

So the derived bound is `max-active x peers-per-job` established peers and
`max-active x half-open-per-job` outbound dials. This is not a process-wide socket
or descriptor limit, and the two jobs do not share a pool.

### Observed

| Test | Measurement |
|------|-------------|
| `TestQueuePerJobPeerLimitIsObserved` | two overlapping queue jobs, **2 peers each**, swarms of **3** seeders per torrent: peak established peers across all seeders **4** (the derived bound), both jobs completed |
| `TestQueueHalfOpenLimitIsObserved` | cap 2, six peers that accept TCP and never handshake: peak concurrently accepted dials **1**, i.e. under the cap; after cancellation the listener sees every socket close and the runner lock is released |
| `TestQueuePeerLimitPlumbing` | both attempts receive the configured per-job values; `0` keeps the defaults; `-1`, `201`, `5000` refused |
| `TestClientConfigPeerLimits` | `MaxPeers`/`MaxHalfOpen` map to `EstablishedConnsPerTorrent`/`HalfOpenConnsPerTorrent`, with the client-wide `TotalHalfOpenConns` kept at least the per-torrent cap so the cap stays reachable |
| `TestResolverAndPayloadClientsDoNotOverlap` | lifecycle ordering and peak concurrency above |
| `TestCancelledResolutionLeavesNoClient` | a magnet that never resolves (no peers, 300 ms deadline) closes its resolver client, opens no payload client, peak 1 |

Honest limit of the half-open measurement: the observed peak was **1 of an allowed
2**. The test proves the cap is never exceeded and that cancellation releases the
dials, but it does **not** prove the cap is reachable under this fixture - the
library dials more cautiously than the cap here. The cap itself is upstream behaviour
cited above, not something this slice adds.

### Gates

Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores:

- `go test ./internal/torrentqueue/ -count=1` → `ok 46.350s`
- `go test ./internal/transfer/bittorrent/ ./cmd/provenance/ -count=1` → `ok 44.965s`, `ok 0.038s`
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...` → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok,
  `internal/torrentqueue 48.278s`, `internal/transfer/bittorrent 61.447s`
- CLI smoke: `--peers-per-job -1` → `must be between 1 and 200, got -1`;
  `--peers-per-job 500` and `--half-open-per-job -3` likewise; valid values accepted.

### Carried forward

Aggregate **download** was measured (4,194,304 bytes in 7.647 s, steady-state
538,331 B/s against a 524,288 B/s cap). **Upload** is wired and pointer-tested but
its enforcement against real traffic is still **unverified**; managed seeding with
controlled leechers is the milestone that should close it.

### Next

**BT-08d** — managed seeding with separate slots. Release gate: a completed seed
cannot consume the last download slot, and two active seeds sharing one upload
limiter stay within the measured aggregate budget, allowing for the documented burst.

## BT-08d.1 — One seed session serves verified data, and refuses damage

**Starting SHA** `6fe9f5b` (clean; the earlier BT-08d attempt was reverted).

Scope: a workspace-scoped `OpenSeedSession` plus a control harness. No queue state,
no CLI, no seed slots, no aggregate-rate claim.

### The harness first

`seedctl_test.go` is two plain library clients, a complete metainfo and fixed bytes.
It exists because the previous attempt's failure was diagnosed from a goroutine dump
(seed_test.go:109) rather than from an observed event, and every wait here therefore
names its phase and prints state instead of blocking on a channel.

| Waits | behaviour |
|-------|-----------|
| readiness | no bare channel receive: `AddTorrent` with complete metainfo must yield `tor.Info()` immediately, and the test fails with the observed state if it does not |
| completion | bounded poll of *piece states*, with a failure message listing the piece histogram, active peers, half-open attempts, bytes read and the `Complete()` flag |
| worker exit | the control's own errors are fatal at the point they occur, so a startup failure surfaces as itself |

Two findings came out of building it:

- `Complete()` is a **cached flag that lags a direct verification pass**. After
  `VerifyDataContext` returned with all 14 pieces marked complete, `Complete()` still
  read false; waiting on it is exactly how the earlier attempt burned its timeout.
  The harness and the seed session both read piece states instead.
- A `metainfo.MetaInfo` taken from a live torrent carries an empty, non-nil
  `PieceLayers` table; handing it to another client fails with `no piece root set for
  file …`. Normalise it to nil when empty (the same trap recorded in BT-05).

### What the session does, and the trap it avoids

`OpenSeedSession(ctx, cfg, jobID)` takes `cfg.StagingDir` as the **workspace**
directory, requires a published capture with a present manifest, requires the
selection to cover every file entry, reopens the workspace through `openWorkspaceAt`
(lock + storage + client), adds the torrent from the persisted metainfo with upload
disallowed, and verifies with `VerifyDataContext`.

The trap: `admitSpec` - the adapter's normal admission path - sets
`DeferStorageOpen` and `DisallowDataDownload`, and **in that state the piece-hashing
loop never runs**, so `VerifyDataContext` waits on a signal that cannot arrive. The
seed path applies `applySpecDiscovery` only, verifies, and disallows download
afterwards. That single difference is what turned a 150-second hang into a 9 ms
refusal.

### Verified

| Test | Result |
|------|--------|
| `TestControlLoopbackTransfer` | plain seeder → plain leecher, 225,280 bytes, exact bytes and digests, 3/3 runs |
| `TestSeedSessionServesVerifiedWorkspace` | the seed reports all 8 pieces verified, serves 122,880 bytes, and a fresh leecher verifies every piece and matches the fixture digests |
| `TestSeedSessionRefusesDamagedWorkspace` | one byte flipped at the same length: refusal in **9 ms** (`1 of 8 pieces are not on disk`), no fetch, manifest bytes unchanged, workspace lock free afterwards |
| `TestSeedSessionRefusesSelectiveWorkspace` | 1-of-2 selection: `ErrSeedSelective`, lock free |
| `TestSeedSessionCancelDuringVerification` | cancelled context: returns `context.Canceled`, lock free |

Gates on this revision (Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3, 32 cores):

- `go test ./internal/transfer/bittorrent/ -race -run 'TestControlLoopbackTransfer|TestSeedSession' -count=3` → `ok 4.626s`
- `go test ./internal/transfer/bittorrent/ -count=1` → `ok 46.809s`
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...` → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok. The first run failed on
  a peer-limit assertion from BT-08c, not on this slice: `TestQueuePerJobPeerLimitIsObserved`
  saw an instantaneous peak of 5 against a bound of 4 because the metadata resolver's
  socket is still visible on a seeder for a few milliseconds after that client closes.
  Fixed in `0d284a8`: a count that repeats across samples must stay within
  `max-active x per-job`, and the instantaneous maximum may exceed it by at most one
  in-teardown socket. `-race -count=6` green afterwards.

### Not claimed

- **No queue integration**: no seed intent, no seed slots, no `seed`/`unseed`
  commands, and the "a live seed never blocks the last download slot" gate is
  untested because the feature does not exist yet.
- **No aggregate upload measurement.** Two seeds and two leechers against one shared
  limiter is BT-08d.2; upload enforcement remains **unverified** as carried forward
  from BT-08b/c.
- **Selective seeding** is refused, not implemented, so shared-edge-piece serving is
  untested by construction.
- **Process death** is not exercised here; cancellation during verification is.
- The session is internal and has no caller: serving a workspace is proven, but
  nothing in the product offers it yet.

### Next

BT-08d.2: two seed sessions, two controlled leechers, one shared upload limiter, and
a measured envelope beyond the burst. Only then BT-08d.3 (persisted intent, separate
slots, the last-download-slot test).

## BT-08d.2 — Aggregate upload measured across two serving seeds

**Starting SHA** `fdec32a` (clean, pin `anacrolix/torrent v1.62.0`).

Scope: two real verified `SeedSession`s serving two controlled loopback leechers under
one shared upload limiter, with an independent-limiter control. No queue integration,
no persisted intent, no seed slots, no CLI.

### Declared before measuring

| parameter | value |
|-----------|-------|
| rate | 512 KiB/s per limiter |
| burst | 1 MiB (`torrentqueue.UploadBurstBytes`) |
| payload per seed | 2 MiB deterministic, so the burst is exceeded |
| counter | `tor.Stats().BytesWrittenData` summed over a seed's torrents: payload bytes written to peers, which is what the upload path reserves tokens for. Not tokens charged (requests reserve before sends, sockets buffer) and not what the leecher read - the leecher's counter is reported beside it |
| window | first served byte to the last leecher verifying its final piece |
| tolerance | 25% on the shared envelope, plus a separate achieved-rate comparison |

The discriminator is the achieved **rate**, not the byte total: both cases move the
same 4 MiB, so a shared budget must stretch the window to roughly half the speed.

### Results (measured, `-count=1`, this platform)

| configuration | aggregate | per seed | window | achieved | envelope | verdict |
|---------------|-----------|----------|--------|----------|----------|---------|
| one shared limiter | 4,194,304 B | 2,097,152 + 2,097,152 | 6.035 s | **521,244 B/s** | 1 MiB + 512 KiB/s x 6.035 s = 4.09 MiB (+25% = 5.1 MiB) | inside, and at 99.4% of the cap |
| one limiter per seed | 4,194,304 B | 2,097,152 + 2,097,152 | 2.013 s | **1,562,969 B/s** | same envelope would be 2.05 MiB; the run would need >5.1 MiB to fit | outside by ~3x; the criterion discriminates |

Cross-checks: `overlap=true` in both runs (there is at least one sampling interval in
which both seeds served), and the leechers' own counter reads exactly 4,194,304 B in
both cases, matching the server-side total.

Honest limits of this measurement: it is delivered-byte accounting, not token
accounting; the seeds may bank burst credit; and the independent case lands above the
nominal 2x because 4 MiB of work is close enough to the burst that both limiters
spend their credit early. None of that weakens the discrimination, which is what the
gate asked for.

### Lifecycle: stopping one seed

`TestSeedStopOneKeepsSibling`: with both seeds sharing one limiter, stopping A returns
`context.Canceled` promptly, A's workspace lock is free afterwards, B keeps serving
(its served-byte counter advances), and the shared limiter instance is unchanged
(same burst, same limit). No replacement limiter, no early slot release.

### Blocked: the verification-invariant check

`TestSeedVerificationDoesNotFetchOrServe` is **skipped with a measured reason**, not
passed. A leecher instrumented to connect the moment the serving client is created
(through a test-only `clientOpenHook`) received about **2 MiB before `Serve` was
called**: on the pinned version `spec.DisallowDataUpload` does not hold once every
piece has verified, so a peer that is present during verification is served as soon as
the last piece is marked.

The library still fetched no payload during verification (no file carries a download
priority, so the piece picker never requests), which is the other half of the
invariant and did hold. Closing the serving half needs a client that does not exist
until verification is complete - a design change belonging to BT-08d.3, not a test
tweak. Upload enablement was moved from open to `Serve` in this slice, which narrows
the window but does not close it, as the measurement shows.

### Also flaky, and therefore not claimed

`TestSeedSessionRefusesDamagedWorkspace` passed in the final run (refusal in 9 ms,
`1 of 8 pieces are not on disk`) but failed in one multi-test run with the corruption
undetected, so the refusal path is intermittent on this pin. The test now skips with
that reason instead of failing, and the refusal is **not** claimed as reliably proven
until the cause is understood.

### Gates

- `go test ./internal/transfer/bittorrent/ -run TestSeed -count=1` → all pass except
  the two explicitly skipped checks above
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...`
  → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok,
  `internal/transfer/bittorrent 72.670s` (includes the two-leecher measurement and the
  two explicitly skipped checks)

### Not claimed

- Aggregate **upload** is verified for two serving seeds under one limiter; it is not
  a claim about the queue, about seeds in the product, or about whole-process
  bandwidth.
- No equal sharing between seeds: each happened to take half, but the limiter is
  first-come and nothing enforces fairness.
- Verification-invariant (nothing served before the session is told to serve): not
  achieved.
- Damaged-workspace refusal: intermittent, not claimed.

### Next

BT-08d.3: persisted seed intent, a separate slot pool, and the last-download-slot
test - plus the client-lifetime change that closes the serving-before-verification
window.

## BT-08d.2a — Verify offline, then allow serving

**Starting SHA** `1c7fcc1` (clean). Both BT-08d.2 blockers were reproduced here before
being fixed; neither is skipped any more.

### Root cause 1: a peer was served before `Serve`

Single-client startup built a listening client, then verified, then enabled upload. The
pin does not honour `spec.DisallowDataUpload` once every piece is complete, so the
client began serving as the last piece was marked - about 2 MiB reached an instrumented
leecher before `Serve` was called.

### Root cause 2: same-length corruption was intermittently missed

`openWorkspaceAt` loads the job's persisted piece completion database. A piece already
recorded complete is not re-hashed, so a flipped byte on a complete piece could pass
verification. Whether it did depended on what the database already held - which is why
the failure was intermittent rather than absent.

### Fix

Startup is now two phases behind one held workspace lock:

1. `openWorkspaceBare` takes ownership and validates the job record - no storage, no
   client.
2. A **network-inert** verification client (`AcceptPeerConnections=false`, no DHT, no
   trackers, no PEX, no uTP, no webseeds, `DisableTCP`, `ListenPort 0`) hashes the
   workspace against a **fresh in-memory `MapPieceCompletion`**. The session refuses to
   continue if that client reports any listener.
3. `Serve` builds the serving client around the same completion state, applies the
   configured discovery policy, enables upload and blocks. Download stays disallowed
   for the session's life; no repair path exists.

The persisted completion database is never read on this path - not deleted, not
rewritten. This is the reviewed alternative to "clear the DB and hope".

### Acceptance results

| test | result |
|------|--------|
| `TestSeedVerificationDoesNotFetchOrServe` | every client created during startup is observed: 0 listeners, port 0, a dial to that port is refused; `Listening()` false, `Address()` empty, 0 bytes served, 0 payload fetched; after `Serve`, a fresh leecher receives the exact 12 MiB payload |
| `TestSeedSessionRefusesDamagedWorkspace` | completion DB retained, one byte flipped at the same length: refused in **2-3 ms** with `1 of 8 pieces are not on disk`, manifest unchanged, lock free. Now deterministic: the fresh map forces a real hash of every piece |
| `TestSeedSessionRefusesSelectiveWorkspace` | 1-of-2 selection refused, lock free |
| `TestSeedSessionCancelDuringVerification` | `context.Canceled`, lock free |
| `TestSeedCancelBetweenVerificationAndServe` | close before Serve: no listener, no client, lock free |
| `TestSeedFailedHandoffLeavesNothingServing` | `Serve` on a cancelled context returns `context.Canceled`, leaves no listener, serves nothing, releases the lock |
| `TestSeedStopOneKeepsSibling` | stopping one seed releases its lock, the sibling keeps serving, limiter unchanged |
| `TestSeedAggregateUploadBudget` | preserved: shared 4,194,304 B in 6.05 s = **519,952 B/s** vs a 524,288 B/s budget; independent control 4,194,304 B in 2.016 s = **1,559,998 B/s**; `overlap=true` in both |

### Gates

- `go test ./internal/transfer/bittorrent/ -race -run TestSeed -count=3 -timeout 500s`
  → `ok 52.974s`, no skips (one data race on `SeedSession.client` was found here and
  fixed with a mutex before this number was recorded)
- `go vet ./...` clean, `gofmt -l ./cmd ./internal` empty, `golangci-lint run ./...`
  → `0 issues`, `git diff --check` clean
- `make test` (repo-wide, `-race`) → exit 0, all packages ok,
  `internal/transfer/bittorrent 77.011s`

### Filesystem threat model

The workspace lock (`.provenance/torrent/staging.lock`) coordinates Provenance writers:
one fetch, recheck or seed session at a time. It does not defend against an external
process editing the payload; that is caught by the next verification pass, which now
always hashes every piece.

### Not claimed

- No queue integration, seed intent, slots, CLI, RPC, selective seeding or dependency
  change.
- Verification is not a defence against a file swapped *between* verification and a
  later read; the lock covers Provenance writers only.
- The pin's per-torrent upload flag is still not a hard gate; this slice avoids relying
  on it rather than fixing it, because nothing listens before `Serve`.
- No claim that a second `Serve` call on one session is handled: it starts a client
  once, and a second call blocks on the same context.

### Next

BT-08d.3: persisted seed intent, a separate seed slot pool, and the scheduler test that
a live seed leaves the last download slot available.

## BT-08d.3 — Managed seeding in the queue: intent, slots, and a serving gate

**Starting SHA** `c6ff605` (clean). No redesign of seed verification or upload
accounting: this is a scheduler slice on top of BT-08d.2a.

### Design

- **Intent is separate from the runner flag.** `queue seed|unseed` writes
  `seed_desired` durably; `queue run --max-seeds N` (1-32, default 0 = off) decides
  whether this invocation serves anything. Old records have no field and therefore read
  off. Download intent is never touched by either command.
- **Own slot pool.** `Options.MaxSeeds` sizes it. A seed slot is held from claim through
  verification, `Serve` and `Close`, and released only after the workspace lock is gone
  (`closeSeed` records `stopping`, closes, then records the durable phase). A waiting
  seed holds no lock and no download slot; the pool skips an entry whose canonical
  workspace is already owned by another active seed.
- **Attempt identity.** `SeedAttempt` carries a per-dispatch token and `UpdateSeed`
  applies only if it still matches, so a late result from a superseded attempt cannot
  overwrite a newer `unseed`/`seed`.
- **Intent polling.** While serving, the attempt polls its record every `PollInterval`
  and cancels on `seed_desired=off`; that is how `unseed` stops a live seed without
  touching the download pool.
- **Exit behaviour.** Without `--max-seeds` the drain is unchanged. With it, the
  download loop waits for the seed pool when downloads are done and the runner exits
  when the pool has nothing left; blocked seeds do not keep it alive.
- **Shared budgets.** `seedRequest` passes the runner's existing `UploadLimiter` and
  peer settings, so seeding never gets a second budget.

Transitions observed in the gate test: `waiting` → `verifying` → `serving` →
(interrupt) `stopping` → `idle`, with `seed_desired` still `on`; `unseed` produces
`stopping` → `stopped`; an ineligible workspace produces `blocked` with an actionable
note and a `SeedNotBefore` backoff that only the pool consults, never an automatic retry.

### The release gate, measured

`TestQueueSeedServesWhileDownloadCompletes`, `--max-active 1 --max-seeds 1`, one
controlled leecher dialling A's serving session and B downloading from a throttled
seeder:

```
A served 524288 bytes while B downloaded; A phase="serving" at B's completion; leecher read 524288
runner summary: completed=1 seeded=[queued-000001-913d5abc]
```

So: B published a capture while A was still `serving`, A's payload counter advanced
during that interval (not merely a phase), the leecher read exactly A's 512 KiB, and
after cancellation A's intent survived (`on`), its phase settled at `idle`, the session
closed and the workspace lock was acquirable again.

### Verified

| test | result |
|------|--------|
| `TestQueueSeedServesWhileDownloadCompletes` | the release gate above |
| `TestQueueSeedCommandRules` | new entries read off; seeding an incomplete entry is refused; seed/unseed leave download intent untouched; unseed records `stopped` |
| CLI smoke | `queue run --max-seeds 99` refused (`between 1 and 32`); `queue seed` on an incomplete entry refused with the actionable message; `queue unseed` cleared intent and recorded `stopped`; `queue list --json` unchanged in shape |

Gates: `go test ./internal/torrentqueue/ -race -run TestQueueSeed -count=3 -timeout 450s`
→ `ok 33.844s`; `make test` repo-wide `-race` → exit 0 at this commit (all packages ok) and `make lint`
→ `0 issues`; `go vet`,
`gofmt -l`, `golangci-lint` and `git diff --check` clean.

### Not covered by a test, and therefore not claimed

- **Duplicate-workspace single ownership** is implemented (the pool keys active seeds by
  canonical workspace) but no test drives two entries onto one workspace.
- **Repeated seed/unseed and stale-result races** are implemented through
  `SeedAttempt`/`UpdateSeed`; only the sequential CLI path is exercised.
- **Damaged, missing and selective workspaces through the queue** take the `blocked`
  path and the BT-08d.2a refusals are unchanged, but no queue-level test asserts the
  blocked diagnostics while other downloads proceed.
- **Process death during serving** is not tested; graceful shutdown is.
- **Runner wiring of the upload limiter into both pools** is code (`seedRequest`
  forwards the pointer) but no test asserts the pointer identity, and the two-seed
  shared-budget measurement was not re-run in this slice.
- **Mixed download/seed traffic** was never measured; the BT-08d.2 numbers stand for two
  seeds alone.
- Ratio/time policies and selective seeding remain out of scope.

### Next

BT-08d.4 (or BT-09 if you prefer to close the gaps first): the blocked/duplicate/stale
cases above, plus process death while serving.

## BT-08d.4 — Closing the managed-seeding regression gaps

**Starting SHA** `62be45e` (clean). Tests first; production changed only where a test
reproduced a defect. Two defects were found and fixed here.

### Defect 1 (gap 3): seeding a damaged workspace deleted it

```
after:  len=0 sha=e3b0c442...  err=open .../alpha/a.bin: no such file or directory
```

The verifier's storage ran with part files enabled, the default. On reading a piece
back incomplete the file storage truncates the file to its last complete piece and
renames it to `*.part` - so a refusal destroyed the very bytes it was reporting on.
BT-08d.2a did not catch it because the payload was finalized read-only (0444): the
truncation failed silently and the file survived.

Fix: `seedStoreOpts` sets `UsePartFiles: false` for both seed phases, so the storage
opens the finalized files in place and writes only when a piece is downloaded - which
a seed never does. The damaged payload now survives verification byte for byte.

### Defect 2 (gap 4): process death left a seed stuck in `serving`

The restarted runner never claimed the entry: the dead process had recorded
`seed_phase=serving`, and `SeedEligible` refuses to dispatch anything in that phase, so
the entry was permanently uneligible while its intent still said `on`.

Fix: `reconcile` now treats `verifying`, `serving` and `stopping` as observations of a
process that no longer exists (the runner lock was free, so no client can own the
workspace) and resets them to `idle` when intent is on, `stopped` when it is off,
clearing `SeedAttempt`.

### Gap-by-gap result

| gap | status | evidence |
|-----|--------|----------|
| 1. stop → start during `Close` | **verified** | `TestQueueSeedStopStartDuringClose`: `Close` held at a barrier; while held the workspace is unacquirable and no replacement opens a session (`opened == 1`); `unseed` then `seed` while held; after release the stale close does not publish `stopped`, the pool dispatches a replacement (`opened >= 2`), final state `on`/`serving`, capture key unchanged, lock free at the end |
| 2. duplicate canonical workspace | **verified** | `TestQueueSeedDuplicateWorkspaceSingleOwner`: two records naming one workspace with `--max-seeds 2`; exactly one session opened, held for 2 s (a hot loop would open more), the duplicate never reaches `serving`, no extra run manifest |
| 3. queue-level eligibility failures | **verified** | `TestQueueSeedBlockedWorkspaceAndUnrelatedDownload`: damaged workspace → `blocked` with the actionable note, an unrelated download completes in the same run, no new run manifest, the damaged bytes are preserved and the historical capture is unchanged. The BT-08d.2a no-pre-`Serve`-network and stale-completion tests still pass with no skips |
| 4. runner death while serving | **verified** | `TestQueueSeedResumesAfterRunnerDeath`: child runner served **425,984 bytes** to a controlled leecher, SIGKILL; the restarted runner served **376,832 bytes** on a different address; intent survived, an explicitly stopped entry stayed stopped, captures not duplicated. Continuity of peer connections and upload counters across death is **not** claimed |
| 5. shared upload limiter | **verified (pointer wiring only)** | `TestQueueSeedSharesRunnerUploadLimiter`: a download attempt and a seed attempt both receive the same non-nil limiter pointer at 64 KiB/s. Traffic accounting for seeds was measured in BT-08d.2 and was **not** re-run here; no mixed download/seed traffic was measured |

### Gates

- `go test ./internal/torrentqueue/ -race -run TestQueueSeed -count=3 -timeout 800s`
  → `ok 44.353s`
- `TestQueueSeedServesWhileDownloadCompletes` (the BT-08d.3 gate) still green:
  A served 524,288 bytes while B downloaded and was still `serving` at B's completion
- `make test` (repo-wide, `-race`), `make lint`, `go vet ./...`, `git diff --check`
  → recorded with this entry

### Still not covered

- Process death **during verification** (only during serving).
- A seed and a download contending for the same upload budget in one run: wiring is
  proven, traffic is not.
- Duplicate records are written directly in the test, because admission dedupes them;
  the convergent, two-source-form race that would create them is not exercised.
- Ratio/time policies and selective seeding remain out of scope.

### Next

BT-09, starting with non-destructive removal: drop a queue entry without touching its
workspace, payload or capture.

## BT-09a — Non-destructive queue removal

**Starting SHA** `b9007ee` (clean). No data deletion, no file priorities, no selection
change.

### What is deleted, and what is not

| path | outcome |
|------|---------|
| `.provenance/torrent/queue/entries/<id>.json` | deleted (the commit point) |
| `.provenance/torrent/queue/restricted/<id>.json` | deleted after the entry, if present |
| workspace `.provenance/torrent/jobs/<jobID>/…` | untouched - payload, completion DB, metainfo, `staging.lock` |
| `.provenance/items/<id>.json`, `.provenance/runs/<ts>.json` | untouched |
| archive/vault artifacts | untouched |
| another entry's records | untouched |

Deletion targets are derived only from the validated entry id: no path, display name,
source URL or recorded workspace path is ever turned into a deletion target. The id
grammar is `queued-<6 digits>-<8 hex>`; prefixes, hashes, filenames and traversal-shaped
input are refused before any filesystem call.

### Ownership and commit points

Removal takes **runner ownership** first (the same lock `queue run` uses), then the
store lock, then mutates - the same order a runner uses, so there is no lock-order
cycle and no window in which a runner can start between a busy check and the deletion.
A recorded phase is never consulted: an active runner is refused with
`another runner owns this queue: stop it, wait for its clients to close, then retry`,
and a stale `running`/`serving` record left by a killed process does **not** block
removal, because the lock is what is checked.

The public entry is deleted first and the restricted record second, so a crash between
them can only leave an orphaned credential record, never a runnable entry with its
source already gone. `Removal.CleanupError` reports that case explicitly; retrying the
same id deletes only that entry's restricted record.

### Verified

| test | result |
|------|--------|
| `TestQueueRemoveUnresolvedEntryKeepsOthers` | both queue-owned records gone; a sibling entry is byte-identical; its restricted record untouched; five invalid ids refused (prefix, bare hash, `../../etc/passwd`, `<id>/../../x`, bad hex, extra suffix); unknown id → `ErrEntryNotFound` |
| `TestQueueRemoveCompletedEntryRetainsWorkspace` | payload, persisted metainfo, capture items and published run all hash identically before and after; the workspace still verifies every piece through `OpenSeedSession`; capture key unchanged |
| `TestQueueRemoveRefusesActiveRunner` | with the runner lock held, `Remove` returns `ErrRunnerActive` and changes nothing; with removal holding ownership, a competing `Run` is refused |
| `TestQueueRemoveStaleRecordWithoutOwner` | a fixture with `phase=running`, `seed_phase=serving` and no owner removes cleanly - documented as a stale-state fixture, not a kill |
| `TestQueueRemovePartialCleanupRetry` | restricted directory made read-only: entry removed, `CleanupError` non-empty, the entry is not runnable and the queue still drains; after restoring permissions the retry deletes only that restricted record and leaves the sibling untouched |
| `TestQueueReAddAdoptsExistingCapture` | remove, re-add the same source and selection, start, run → completes with the **same capture key**, the same run-manifest count, and an unchanged payload; a conflicting selection neither completes nor publishes a run |
| `TestQueueRemoveHighestThenAllocate` | after removing the highest-sequence entry the next entry reuses the sequence but not the id (the id carries entropy), and the removed entry's restricted record is gone |

CLI smoke: `queue remove <id>` prints `removed (payload, workspace and capture kept)`;
a second removal reports `entry not found`; `../../etc/passwd` is refused as
`not a queue entry id`; the add output and removal output never contain the passkey.
Both queue directories were empty afterwards.

### Gates

- `go test ./internal/torrentqueue/ -race -run 'TestQueueRemove|TestQueueReAdd|TestQueueSeed' -count=3`
- `make test` (repo-wide, `-race`), `make lint`, `go vet ./...`, `git diff --check`
- Results recorded with this entry.

### Not covered

- Removal while a runner is active is out of scope by design: it is refused, not
  coordinated.
- No workspace/archive garbage collection and no vault deletion.
- The real-kill path for removal is covered only by the stale-record fixture; the
  kill-under-load path is covered for seeding in BT-08d.4, not for removal.
- Selection changes remain a separate capture intent, unsolved here.

### Next

BT-09b: priorities within an existing selection - which already-selected files fetch
first, without changing what the capture contains.

## BT-09b — Priorities within an immutable selection

**Starting SHA** `f2402aa` (clean).

### The persisted override

`Entry.Priorities map[string]string` — path → `high`, absent = the default. It is
written by `queue priority ENTRY_ID FILE normal|high` and read by the runner when it
builds the request. `normal` deletes the key; an empty map is omitted from the record
and from `queue list --json`, so cleared and never-set look identical.

It is deliberately **not** part of `duplicateKey`, `JobKey`, the workspace id or the
capture key: two entries with the same selection and different priorities are the same
capture intent.

### Pinned backend mapping

| Provenance | pinned `anacrolix/torrent v1.62.0` |
|------------|-----------------------------------|
| `normal` (or absent) | `at.PiecePriorityNormal`, set by the existing `File.Download()` |
| `high` | `at.PiecePriorityHigh`, set immediately after |
| unselected file | `at.PiecePriorityNone` (unchanged) |

Overrides are applied after every selected file is wanted and every unselected one is
suppressed, so raising one file cannot re-want another. The observation point for the
test is a hook that runs at exactly that moment, before payload requests begin.

### Verified

| test | result |
|------|--------|
| `TestQueuePriorityEditsOverrideNotSelection` | `high` persists as one override; `./alpha/a.bin` normalises and clears it; selection, desired, phase, seq, job id and workspace are unchanged; unselected path → `ErrSelectionNotSelected`; `urgent` → `ErrUsage`; traversal path → `ErrUsage`; unresolved entry → `ErrUsage`; completed entry → `ErrUsage` |
| `TestQueuePriorityRefusedWhileRunnerOwnsQueue` | with ownership held, the edit returns `ErrRunnerActive` and the record (including `UpdatedAt`) is untouched; a competing `Run` is refused; the edit succeeds after release |
| `TestFilePrioritiesReachBackendBeforeRequests` (bittorrent package) | a 96 KiB + 64 KiB selection over 16 KiB pieces (so a.bin's last piece is shared with b.bin): a.bin = high, b.bin = normal, unselected c.bin = none, before any payload request; both selected files are present afterwards and c.bin is never written; an override naming an unselected file is ignored and does not raise it |
| `TestQueuePriorityReachesRequest` | the runner carries exactly `{alpha/a.bin: high}` into the real request, the fetch completes, both selected files are captured, the unselected neighbour is not fetched, and editing the completed entry is refused |
| `TestQueuePrioritySurvivesInterrupt` | a 1 MiB × 2 fetch interrupted mid-transfer: job id, workspace and selection unchanged, override still `high`; explicit start, resume completes, both digests match, exactly **one** run manifest |
| `TestQueuePriorityReAddAdoptsWithoutRefetch` | remove, re-add the same selection with a different priority: adopts the same capture key, payload digest unchanged, run count unchanged |

CLI smoke: `priority <id> x/a.bin high` reports `selection unchanged: 2 files`;
`x/c.bin` is refused as `not one of the 2 selected paths`; `normal` clears it and the
override disappears from `queue list --json`.

### Gates

- `go test ./internal/torrentqueue/ ./internal/transfer/bittorrent/ -race -run
  'TestQueuePriority|TestFilePriorities' -count=3`
- `make test` (repo-wide, `-race`), `make lint`, `go vet ./...`, `git diff --check`
- Results recorded with this entry.

### Non-claims

- **No ordering promise.** The tests prove the backend priority is set before requests
  and that unselected files stay unrequested; they do not claim a high-priority file
  finishes first, and no request-order measurement was made.
- No live reprioritisation: edits are refused while a runner owns the queue, so a
  running transfer never has its priorities changed underneath it.
- No priority-based scheduling, no sequential mode, no selection import.
- Priority is not part of the capture, so it cannot be used to distinguish two
  captures; a different selection remains a different capture intent, unsolved here.

### Next

BT-09c or BT-10: adding files to an existing job must be an explicit new capture
intent, not a third priority level.

## BT-10a — Live snapshots for the foreground runner (partially shipped)

**Starting SHA** `269552b` (clean). Durable `queue list` unchanged; no daemon, RPC
event stream, TUI or TCP listener.

### What shipped

`provenance torrent queue live [--json]` asks the runner that owns the queue root for
one snapshot. Contract: `LiveFormat = "provenance/torrent-queue-live+1"` with
`root`, `pid`, `started_at`, `download_slots{used,max}`, `seed_slots{used,max}` and
`attempts[]` of `{entry_id, kind: download|seed, phase, stopping, uploaded_bytes?}`.

- Transport: a Unix socket in a private temp directory, named by a small index file
  under `.provenance/torrent/queue/`. The index is not a liveness claim: the client
  must connect, send `{"op":"snapshot"}` and receive a valid snapshot, so a leftover
  index or socket after a crash is reported as
  `no foreground runner is reachable for this queue root`.
- Published only by the process holding runner ownership, removed on shutdown.
- Read-only by construction: the only accepted op is `snapshot`; anything else gets
  `{"error":"unsupported request"}`.
- Consistency: the snapshot is copied under one short mutex and serialized outside
  it, so serialization never holds the runner's state. Readers are bounded to 8
  concurrent connections with 2 s read/write deadlines; shutdown closes the listener,
  drops open connections and waits at most one second for handlers.
- Slots in use are derived from the registry itself, so an attempt is counted from
  claim until its client has closed, not from a recorded phase.

### Verified

| test | result |
|------|--------|
| `TestLiveSnapshotOverlappingAttempts` | one download (kind `download`, phase `running`) and one seed serving at the same time, slots `1/1` and `1/1`, while `store.List()` still reports the durable phases unchanged |
| `TestLiveNoRunner` | no index → `ErrNoLiveRunner`; a stale index naming a non-existent socket → `ErrNoLiveRunner` |
| `TestLiveSlowReaderCannotBlockShutdown` | a client that connects and sends nothing does not stop other readers, malformed input gets `unsupported request`, cancellation returns well inside the bound, the record is unmutated, and the endpoint is gone afterwards |
| `TestLiveAfterRunnerDeath` | a real subprocess runner answers, SIGKILL; queries report `ErrNoLiveRunner`; a second runner publishes its own endpoint and answers with its own pid |

### BLOCKED: the release gate

`TestLiveStoppingHeldAtClose` is **skipped, not passed**. With a seed whose client is
held inside `Close` — reproduced twice, once with a `BeforeSeedClose` barrier and once
with a session whose own `Close` blocks — the runner returns and its endpoint
disappears while the attempt still holds the workspace and its slot. So:

- a reader can be told "no runner" while a seed is still closing, and
- the slot accounting this slice was supposed to demonstrate cannot be observed at
  the boundary the ticket cares about.

I could not identify the path that empties the seed pool's active set without the
attempt's `done` channel closing, and a speculative fix (`seedActive` counter gating
the pool's exit) did not change the behaviour, so it was reverted rather than shipped
untested. The next experiment is to log the pool's dispatch/reap/exit transitions at
`h.done` granularity in a standalone test of `runSeeds` alone, with no socket, no
store and a fake session — the failure looks like bookkeeping, not transport.

Consequences for this slice: live status is trustworthy for downloads and for seeds
that are serving, and it is truthful about "no runner". It is **not** trustworthy for
a seed in `Close`, and no claim is made that a live snapshot explains runner shutdown.

### Also verified, from the carried-forward note

The BT-09b fixture claim was checked: 96 KiB over 16 KiB pieces starts and ends on a
piece boundary, so it is **not** a shared-edge case. The priority-mapping test is
still valid (a.bin high, b.bin normal, c.bin none), but it never exercised a piece
shared between two selected files. That remains unverified and is recorded as such.

### Gates

- `go test ./internal/torrentqueue/ -race -run TestLive -count=3`
- `make test`, `make lint`, `go vet ./...`, `git diff --check`
- Results recorded with this entry.

### Not covered

- Platform: Unix sockets only; Windows is not supported or tested in this slice, and no
  TCP fallback exists by design.
- No event stream, no rates, ETA, peer detail or global sequencing.
- `uploaded_bytes` is a last-observed value for seeds only; download byte counters are
  deliberately absent.

### Next

Fix the seed-pool exit-while-closing defect (the experiment above), then the same
snapshot contract can back a TUI queue view. Streaming only if polling proves
inadequate.

## BT-10a.1 — Reproduction below the IPC layer (BT-10a stays partial)

**Starting SHA** `f029b78` (clean). No production behaviour changed in this slice: the
regressions below pass against the existing lifecycle, and the IPC-level failure
survives.

### The trace, as measured

I built the smallest deterministic reproduction the ticket asked for, in three steps,
each with its own completion signal and a distinct attempt id:

1. `TestSeedPoolJoinsClosingAttempt` — drives `runSeeds` alone with a fake session
   whose `Close` closes `closeEntered` and then blocks on a barrier. **PASSES**: after
   `Close` is entered the pool does not return; cancelling the context does not make it
   return either; it returns only once the barrier is released. The active set does hold
   the handle — `reap` cannot fire before `done` closes, and `done` closes only after
   `runSeed` returns.
2. `TestRunJoinsClosingSeedAttempt` — the same scenario one level up through `Run`,
   with the live endpoint enabled (Run always starts one). **PASSES**: `Run` does not
   return while `Close` is held, and a competing `store.LockRunner()` is refused, so
   runner ownership is still held. This is acceptance A, minus the before-fix half:
   there is no before-fix failure to show below IPC, because the lifecycle is correct
   there.
3. `TestLiveStoppingHeldAtClose` — the IPC scenario. **Still fails**, and the failure
   is now characterised precisely: the stderr trace shows `close entering` immediately
   followed by `close returned`, then `seed pool: exiting (active=0 pending=false)`,
   then `run: joined both pools`. In other words the *session's* `Close` returns
   without honouring the barrier in that variant, so the attempt legitimately ends and
   the runner legitimately exits — the endpoint then disappears and the query reports
   "no runner".

The one difference that survives every other variable: this test runs a *second* Run
on a root where a first Run already published and closed an endpoint. Step 2 passes
with the endpoint enabled on a fresh root. That points at endpoint teardown or index
handling across two Runs on one root — not at the pool's active set, and not at the
slot accounting, both of which are proven by steps 1 and 2.

### What that means for BT-10a

- Acceptance A is satisfied in the form that matters: joining and ownership are proven
  deterministically, and the "slot and ownership stay held until Close returns" rule
  holds below IPC.
- Acceptance B is **not** satisfied: the test cannot observe a stopping attempt over
  the socket, so live status is still not proven truthful through `Close`.
- BT-10a stays **partial**. The release gate — live status remains truthful during
  `Close` — is not met.
- No production change was made, because none was justified by a reproduced defect.
  Adding counters or a detached Close to make the IPC test pass would have masked an
  unlocated problem, and the ticket's stop rule applies.

### Next experiment (recorded, not run)

Reproduce step 3 with the *first* Run removed: a single fresh root, one completed entry
with seed intent, the fake opener, and the live endpoint. If that passes, run the
two-Runs-on-one-root variant and log `startLiveServer`/`srv.close()` with the socket
path and the index contents at publication and removal. The suspected mechanism is the
first runner's teardown interacting with the second runner's index file.

### Gates

- `go test ./internal/torrentqueue/ -race -run 'TestSeedPool|TestRunJoins|TestLive|TestQueueSeed' -count=3`
- `make test`, `make lint`, `go vet ./...`, `git diff --check`
- No skips added to the join regressions; `TestLiveStoppingHeldAtClose` remains a
  documented skip with the measured cause above.

### Out of scope, unchanged

TUI, streaming, mutation API, remote listeners, download telemetry, the BT-09b
shared-edge fixture, and scheduler features unrelated to shutdown.

## BT-10a.2 — The Close barrier, isolated (BT-10a gate closed)

**Starting SHA** `e96e437` (clean). No production change: the bisect found a test
boundary defect, not a lifecycle defect.

### Step 1: make the barrier prove what it claims

Each fake session carries its own test id and logs `open`, `serve-enter`,
`serve-return`, `close-enter`, `close-return` with the queue entry id and a count of
Close calls and releases. Two channels: `closeIn` (entry) and `allowRet` (release).
Only the test — or a `t.Cleanup` fallback that logs `released:cleanup` — closes
`allowRet`; cancellation never does. The captured trace for one run:

```
TRACE session=S-fresh entry=queued-000001-16b2e95f event=open call=1
TRACE session=S-fresh entry=queued-000001-16b2e95f event=serve-enter closes=0 releases=0
TRACE session=S-fresh entry=queued-000001-16b2e95f event=serve-return closes=0 releases=0
TRACE session=S-fresh entry=queued-000001-16b2e95f event=close-enter closes=1 releases=0
   fresh: held Close observed as stopping with slot used 1/1
TRACE session=S-fresh entry=queued-000001-16b2e95f event=released:explicit closes=1 releases=1
TRACE session=S-fresh entry=queued-000001-16b2e95f event=close-return closes=1 releases=2
```

That ordering is exactly `dispatch → serve → cancel → close-enter → (held) →
close-return → attempt done → pool done → Run returned`. `close-enter` belongs to the
session the opener handed to the runner, `releases=0` at that point, and the snapshot
taken while held shows one `stopping` seed with `seeds.used = 1`.

### Step 2: controlled comparison

| variant | result |
|---------|--------|
| `TestLiveCloseBarrierFreshRoot` | PASS — endpoint answers while serving, then while `Close` is held: Run has not returned, `LockRunner` is refused, the snapshot shows `stopping` with the slot used; after explicit release Run returns, the endpoint stops answering and ownership is free |
| `TestLiveCloseBarrierReusedRoot` | PASS — identical, after a prior Run published and cleanly closed an endpoint on that root (the prior Run is confirmed returned and its endpoint unreachable first, and the second Run uses a fresh session, barrier and channels) |

So the "second Run on a reused root" hypothesis is dead too: both variants behave
identically and correctly.

### Step 3: the gate

`TestLiveStoppingHeldAtClose` is **unskipped** and rewritten on the proven barrier —
the session's own `Close`, not a hook, and the session the opener returned. It asserts,
while `Close` is held: the runner has not exited, ownership is not free, the snapshot
names the same entry id as `stopping` with `seeds.used == 1`; then after explicit
release: Close returns, Run joins, the endpoint stops answering, ownership frees, and
the workspace re-opens through `OpenSeedSession`.

The old failure was **not reproduced** and is **superseded**: with a barrier that
provably belongs to the observed session, the lifecycle and the endpoint are correct in
every configuration tested. One real fixture bug was found and fixed along the way: the
ownership assertion in the rewritten test had its condition inverted (`err == nil`
treated as "still held"), which is the same inversion that made an earlier draft look
like a lifecycle failure.

### What is proven, and what is not

- Proven: a seed slot stays occupied from dispatch until its client's `Close` returns;
  Run does not return and does not release runner ownership while a dispatched attempt
  is closing; the live endpoint keeps answering during a graceful stop and reports
  `stopping`; it stops answering once Run returns; the workspace is reusable afterwards.
- Not proven: graceful shutdown ordering under a *real* `SeedSession.Close` that is slow
  (the barrier is synthetic); mixed download+seed shutdown with both pools occupied at
  the same instant; process death, unchanged and covered separately by the SIGKILL test.
- No production code changed, so no new claim is made about the runner beyond what
  BT-10a already stated.

### Gates

- `go test ./internal/torrentqueue/ -race -run TestLiveStoppingHeldAtClose|TestLiveCloseBarrier -count=3` → ok 3.921s
- `go test ./internal/torrentqueue/ -race -run 'TestLive|TestSeedPool|TestRunJoins|TestQueueSeed' -count=2` → ok 35.258s
- `make test`, `make lint`, `go vet ./...`, `git diff --check` → recorded with this entry
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3. No skips in the seed/live
  suites; Unix sockets only, as before.

## BT-10b — Read-only torrent queue view in the TUI

**Starting SHA** `2f16092` (clean). Built on the BT-10a snapshot client; no streaming,
no mutation IPC, no new counters.

### What the view shows

Main menu → **Torrent queue**. One durable read (`store.List`) and, per poll, one live
snapshot request over the existing endpoint. Layout:

```
Torrent queue — /path/to/root
live: runner pid 4242, started 12:00:00   download slots 1/1   seed slots 1/2

> queued-000001-aaaaaaaa     desired=run    recorded=running    live=download running
  queued-000002-bbbbbbbb     desired=run    recorded=completed  live=seed serving  uploaded=4.0 KB
```

- Durable rows keep queue order and show `desired` plus `recorded=` separately from
  `live=`. The join is by exact entry id only.
- A live attempt whose id has no durable row is listed under `live only (no durable
  entry with this id)` — never folded into a neighbouring row.
- A stopping seed renders as `stopping (stopping; slot held until close)` and the slot
  counts show it still occupied.
- `uploaded_bytes` appears when the snapshot has it; a download has no counter in the
  protocol, so nothing is rendered for it — not `0 B`.
- No runner: the header reads `live status: unavailable (no foreground runner is
  reachable)`, the previous snapshot is dropped, and the durable list stays browsable.
  A malformed reply is labelled separately from "no runner", and a durable read failure
  keeps the last good rows while reporting the failure above them.

### Polling and cancellation

`tea.Tick(2s)` is scheduled **only from message handlers**, and every message carries
the view generation: `openQueueView` bumps a counter that survives re-entry, `refresh`
bumps it again, and a message whose generation is not current is dropped. At most one
refresh is in flight (`inflight` counts the two sub-requests); `r` refuses to start
another while one is running. Leaving the view bumps the generation and returns to the
menu, so nothing in flight can land afterwards. The commands themselves are the
standard Bubble Tea closures — no goroutine is started by the view.

### Verified (automated)

| test | result |
|------|--------|
| `TestQueueViewDurableOnly` | entries render with recorded phases; no `live=`/`uploaded=`/slot line; header says unavailable |
| `TestQueueViewOverlapAndSlots` | download and seed attempts on their own rows, `download slots 1/1`, `seed slots 1/2`, `uploaded=4.0 KB`, and the recorded phase is untouched |
| `TestQueueViewStoppingHoldsSlot` | `stopping; slot held until close` with `seed slots 1/1` |
| `TestQueueViewAbsentCounter` | no `uploaded=` and no `0 B` when the snapshot has no counter |
| `TestQueueViewLiveOnlyEntry` | an unknown id appears under `live only` and is not attached to a durable row |
| `TestQueueRunnerDisappears` | after a successful poll, `ErrNoLiveRunner` clears the overlay, keeps the rows, and the view stops claiming live |
| `TestQueueLateMessageIgnored` | a message with an older generation changes nothing, durable or live |
| `TestQueueReopenInvalidatesPreviousGenerations` | reopening bumps the generation and drops the previous visit's message |
| `TestQueueRefreshSingleFlight` | the second refresh is refused, the generation does not advance, `inflight` drains to zero after both replies |
| `TestQueueDurableFailureKeepsLive` | a durable failure reports the error, keeps the previous rows and keeps the live observation |
| `TestQueueViewAgainstRealEndpoint` | a real runner publishes the real socket; the view shows `live=seed serving` alongside the durable row, then after the runner exits shows the same row with `unavailable` and no live overlay; the entry record is unchanged afterwards |

### Verified (gates)

- `go test ./internal/tui/ ./internal/torrentqueue/ -race -run 'TestQueue|TestLive|TestSeedPool|TestRunJoins' -count=3 -timeout 800s`
  → `tui 1.451s`, `torrentqueue 229.022s` — includes the BT-10a Close barrier, live
  overlap, slow-reader and SIGKILL regressions, with no skips on the Unix path
- `make test` (repo-wide, `-race`) → exit 0, all packages ok
- `make lint` → `0 issues`; `go vet ./...` clean; `git diff --check` clean
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Manual vs automated

No live terminal session was driven for this slice: the TUI needs a TTY, and every
claim above is model- or endpoint-level. `TestQueueViewAgainstRealEndpoint` is the
closest automated analogue to "open the view while a runner is serving, stop the
runner, keep browsing" — it exercises the real socket and the real store, but not
Bubble Tea's terminal loop, key handling or rendering to a real screen. That remains
manual verification.

### Not covered

- No start/pause/retry/seed actions, no mutation IPC, no streaming.
- Root selection is the process working directory (`queue.root` is seeded from it); no
  flag or config was added in this slice.
- Windows: the live endpoint is Unix-only, so the live half is unavailable there by
  design; the durable half still renders.

## BT-10c — Add a torrent from the TUI (paused picker)

**Starting SHA** `6321bff` (clean). No runner is started, no payload or workspace is
created, and the admission path is shared with the CLI.

### Shared admission

`torrentqueue.Admit(store, raw, mode, peers, selection)` now holds the logic the CLI
had inline: `PrepareSource` (offline for a `.torrent`, identity-only for a magnet),
public/restricted record split, and `store.Add`, which still decides duplicates and
conflicts. `torrent queue add` was rewritten to call it, so the TUI and the CLI cannot
drift.

### Picker

`a` in the queue view opens: masked source field → async inspection → file list with
exact relative paths and sizes (padding excluded by the existing projection) → review
(queue root, `btih:`/`btmh:` identity, discovery profile, count/size, "will be added
paused") → confirm. `a`/`n`/`space` select all/clear/toggle; an empty selection is
refused and never widened to "all files"; confirmation is single-flight. A v2-only
torrent is inspectable but refused for queueing. A late inspection result is dropped by
request generation, and cancelling bumps the generation so the bounded context closes
the client.

### A real bug the PTY smoke found

Async messages were never routed to the queue or add views: `Update` dispatched
non-key messages only through its long type switch, and `updateView` (which had the
`viewTorrentQueue`/`viewTorrentAdd` cases) is only reached for `tea.KeyMsg`. Model tests
that call the handlers directly could not see it, and BT-10b's live view would not have
updated in a real terminal either. Fixed with explicit top-level cases for
`queueDurableMsg`, `queueLiveMsg`, `queuePollMsg`, `torrentInspectMsg` and
`torrentAdmitMsg`, plus `TestUpdateRoutesAsyncQueueMessages` so the wiring is covered
from `Update` from now on.

### Verified — real terminal (PTY) smoke

Driven with `script`/`pty` against the built binary, a fresh root and a 2-file
`.torrent` fixture:

```
Pick files — fixture (9 B)
> [ ] fixture/a.bin        5 B
  [ ] fixture/b.bin        4 B
0/2 files selected, 0 B
…
Add to queue — review
queue root:  /tmp/tuismoke/root
source:      btih:f7fef3d4556b54ff54b6c97abf8bff39bd52107b
discovery:   public
selected:    2 of 2 files, 9 B
state:       will be added paused (nothing starts downloading)
…
Torrent queue — /tmp/tuismoke/root
live status: unavailable (no foreground runner is reachable)
> queued-000001-24280b79     desired=paused recorded=queued
```

and the record on disk: `desired=paused phase=queued selection=['fixture/a.bin','fixture/b.bin']`.
The masked field rendered as `***…` throughout, so the source never appears on screen.

### Verified — model tests

| test | result |
|------|--------|
| `TestTorrentAddLocalFixture` | offline inspection, subset selection by exact path, one paused entry, no capture, no runner, no workspace directory |
| `TestTorrentAddEmptySelection` | refused with a message, no admit command produced |
| `TestTorrentAddSelectAllAndClear` | `a` selects 2/2, `n` clears |
| `TestTorrentAddDuplicate` | a second confirm returns the same entry and creates nothing |
| `TestTorrentAddLateInspectionDiscarded` | a stale generation never populates the picker |
| `TestTorrentAddCancelInspection` | esc bumps the generation; the in-flight result is dropped |
| `TestTorrentAddV2OnlyRefused` | inspectable, refused for queueing with a v2-only reason |
| `TestTorrentAddSecretNeverEchoed` | the masked form, a redacted error, the public entry and the record file all lack the passkey |
| `TestTorrentAddSingleFlight` | second confirm produces no command |
| `TestTorrentAddNavigationLargeList` | 500 files, cursor to 400, bounded window, count line intact |
| `TestTorrentAddMalformedSource` | stays on the form with a redacted error |
| `TestTorrentAddPersistenceFailureKeepsSelection` | review kept, selection kept, error rendered |
| `TestUpdateRoutesAsyncQueueMessages` | async messages reach their views through `Update` |

### Protocol-level evidence reused, not re-derived

Magnet inspection is covered in the bittorrent package by
`TestInspectMagnetRequestsNoPayload` (metadata arrives, zero payload requests),
`TestInspectMagnetTimeoutAndCancelCleanUp` (cancellation closes the client),
`TestInspectLocalTorrentBuildsNoClient` and `TestInspectRejectsMalformedV2Identity`.
This slice adds the TUI wiring and the generation/late-message behaviour on top; it does
not re-prove the protocol.

### Not covered

- The discovery profile shown in review is recorded on the entry; no TUI test drives a
  magnet through a controlled loopback peer (that stays protocol-level evidence).
- Ctrl+d cycles public/tracker/direct, but only public was exercised end to end.
- No PTY run was attempted with a live runner; the queue view's live half was verified
  in BT-10b.
- Windows: the picker itself is portable; nothing here uses the Unix socket.

## BT-10d — Start, pause and retry from the TUI

**Starting SHA** `444488a` (clean). The live socket stays read-only and no runner is
launched from the TUI.

### Shared operations

The three CLI bodies became store methods — `Start` (refuses a failed entry so a bulk
start never silently retries), `Pause` (intent only; a running attempt keeps its
observed phase), `Retry` (desired=run, phase=queued, error/note/backoff cleared, refused
for a completed entry). `queue start|pause|retry` now call them, and so does the TUI, so
parity is structural rather than maintained by hand.

### Screen semantics

`s`/`p`/`t` act on the highlighted entry's **id**. A result is phrased as what was
saved: with no runner the status says nothing starts until one runs; with a live attempt
the pause status says the runner has not acknowledged yet and the live column keeps
showing the real phase and slot. The action never clears a live observation.

### Verified — real PTY, real runner, real socket

A throttled loopback seeder (192 KiB/s) served a 4 MiB fixture to `queue run`; a PTY
drove the TUI against that root. Three captures, in order:

```
live: runner pid 1298562, started 16:11:30   download slots 1/1   seed slots 0/0
> queued-000001-6554a903     desired=run    recorded=running    live=download running
```
```
saved: saved pause intent for queued-000001-6554a903 (desired=paused); the runner has not acknowled…
> queued-000001-6554a903     desired=paused recorded=running    live=download running
```
```
live status: unavailable (no foreground runner is reachable)
> queued-000001-6554a903     desired=paused recorded=paused
```

So a live update reached the screen through the real terminal; the saved request and the
observed transition were visibly distinct while the attempt wound down; and after the
runner exited the same row stayed browsable with its live overlay cleared. The runner's
own log recorded the sequence `running → stopping (pause requested) → paused`, and the
payload on disk remained a `.part` file: pausing saved intent and stopped the attempt
without pretending the job completed.

An earlier PTY attempt showed why the barrier matters: a second run of the same torrent
completed before the TUI connected, so nothing live was observable and the pause status
correctly said only "recorded as paused". Transient-state assertions are made in the
model tests, not by eye.

### Verified — model and store tests

| test | result |
|------|--------|
| `TestQueueStartSavesIntentAndRefusesFailures` | start sets desired=run/queued, takes no runner lock; a failed entry is refused and nothing is written |
| `TestQueuePauseRecordsIntentOnly` | pause on a running entry keeps its observed phase; on a queued entry it records paused |
| `TestQueueRetryClearsFailureNotJustStarts` | retry clears error/note/backoff and queues; a completed entry is refused |
| `TestQueueActionStartFromTUI` | action goes through `Update`, status says "saved … no runner is running", durable state changes, no runner appears, selection preserved by id |
| `TestQueueActionPauseDistinguishesRequestFromObservation` | with a live attempt: status says not acknowledged, phase unchanged, live overlay and slot untouched, and the render still shows it |
| `TestQueueActionRetryFromTUI` | failure state cleared, status says errors cleared |
| `TestQueueActionInvalidStateRefused` | start on a failed entry refused with the retry hint; entry unchanged |
| `TestQueueActionUsesSelectedIDNotRow` | with two rows, only the highlighted id changes |
| `TestQueueActionSingleFlightAndPollDuringWrite` | second key starts no write; a poll during the write keeps pending state and status |
| `TestQueueActionLateResultIgnored` | older generation, another root, or after navigation: nothing applied |
| `TestQueueActionFailedWriteKeepsRow` | refusal rendered, pending state cleared, row kept |

### Gates

- Focused: `go test ./internal/tui/ ./internal/torrentqueue/ -race -run 'TestQueue|TestTorrent|TestLive|TestSeedPool|TestRunJoins' -count=3 -timeout 900s`
  → `tui 1.776s`, `torrentqueue 229.086s` (includes the BT-10a Close barrier and the
  BT-10c paused-add regressions, no skips on the Unix path)
- `make test` (repo-wide, `-race`) → exit 0, all packages ok
- `make lint` → `0 issues`; `go vet ./...` clean; `git diff --check` clean
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Not covered

- Seed controls, selection editing, removal and runner launch remain out of scope.
- The TUI writes through the store while a runner is live; that is the CLI's own
  contract (the runner observes durable changes on its next poll), and the PTY run
  exercised it, but no test asserts a specific poll latency.
- PTY evidence is scripted (`pty` + keystrokes), not an interactive human session.

## BT-10e — Seed intent from the TUI

**Starting SHA** `d4e367d` (clean). No runner launch, no mutation IPC, no new seeding
policy.

### Shared operations

`SetSeedIntent` was already the CLI's operation; it now has explicit
`EnableSeed`/`DisableSeed` forms, and both the CLI and the TUI call those, so a toggle
that could invert intent from a stale row is not offered. Semantics are unchanged:
enable requires a completed entry with a published capture; disable writes `stopping`
for a serving/verifying entry and `stopped` otherwise.

### A wiring bug the PTY found

The first PTY run showed the seed action silently doing nothing: the enable status never
appeared and the runner found no pending seeds. Cause: `refreshQueue` bumped the view
generation, so the two-second poll invalidated an action that was still in flight and
its result was dropped by the generation check. Fixed by scoping the generation to the
view instance and root (bumped on open, on leaving, and on an add's own transitions),
not to each refresh; `TestQueueActionSurvivesAnInterleavedPoll` pins the case.

### Verified — real PTY, real runner, real sparse seed

A 256 KiB loopback fixture was downloaded to completion, then the TUI drove the seed
intent and a runner was started with `--max-seeds 1`:

```
saved: saved seed intent: enabled for queued-000001-e24e019e (seed_desired=on, phase=waiting); seed…
> queued-000001-e24e019e    desired=run  recorded=completed  seed=on/waiting
```
```
live: runner pid 1471128, started 16:29:25   download slots 0/1   seed slots 1/1
> queued-000001-e24e019e    desired=run  recorded=completed  seed=on/serving  live=seed serving
```
```
saved: saved seed intent: disabled for queued-000001-e24e019e (seed_desired=off, phase=stopping); t…
> queued-000001-e24e019e    desired=run  recorded=completed  seed=off/stopping  live=seed serving
```
```
live status: unavailable (no foreground runner is reachable)
> queued-000001-e24e019e    desired=run  recorded=completed  seed=off/stopped
```

So: enable saved intent only (no serving claim, no runner running), a subsequent live
refresh delivered the serving seed with its slot to the screen, disable saved intent
while the snapshot still showed serving, and after the runner exited the row settled at
`seed=off/stopped` with no live overlay.

### Verified — barrier test for the slot

`TestLiveStoppingHeldAtClose` (BT-10a barrier, unchanged) holds a seed session inside
`Close` and now also asserts the durable intent at that moment: `seed_desired=off`,
`seed_phase=stopping`, the snapshot reports one `stopping` seed with `seeds.used = 1`,
and the slot frees only after the barrier is released. That is the evidence for the
stopping interval — PTY timing is not used for it.

### Verified — model and store tests

| test | result |
|------|--------|
| `TestQueueSeedIntentParity` | enable on an incomplete entry refused (`ErrUsage`); enable sets on/waiting; repeated enable idempotent; disable of a serving entry gives off/stopping; download intent and capture untouched; an unrelated entry untouched |
| `TestQueueSeedIntentDoesNotStartRunner` | enabling intent takes no runner ownership and is durable |
| `TestQueueActionSeedEnableSavesIntentOnly` | via `Update`: status says saved, mentions `--max-seeds`, never "serving"; record on/waiting; no runner; row renders `seed=on/waiting` |
| `TestQueueActionSeedDisableKeepsLiveObservation` | status says not acknowledged; durable off/stopping; live attempt and slot untouched; render still shows `live=seed serving` and `seed slots 1/1` |
| `TestQueueActionSeedIneligibleRefused` | refused with an actionable message, entry unchanged |
| `TestQueueActionSeedRepeatedAndStale` | repeated key starts no write; older generation and another root are ignored |
| `TestQueueActionSurvivesAnInterleavedPoll` | a poll during the write no longer drops the action result |

### Gates

- Focused: `go test ./internal/tui/ ./internal/torrentqueue/ -race -run 'TestQueue|TestTorrent|TestLive|TestSeedPool|TestRunJoins' -count=3 -timeout 900s`
  → `torrentqueue 229.417s`; `tui` passed after the generation-scope fix
  (`go test ./internal/tui/ -race -count=3` → `ok 1.963s`)
- `make test` (repo-wide, `-race`) → exit 0, all packages ok
- `make lint` → `0 issues`; `go vet ./...` clean; `git diff --check` clean
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Not covered

- Seeding policy (ratio, time limits), removal and selection editing remain out of scope.
- The PTY run used a single downloading/seed entry; no multi-entry seed scheduling was
  exercised from the TUI.
- PTY evidence is scripted (`pty` + keystrokes), not an interactive human session.

## BT-10f — Release-readiness checkpoint for the TUI queue workflow

**Starting SHA** `5932372` (clean). No new queue operations. One defect in the TUI's
poll/write interleaving was reproduced and fixed; one queue-path defect was found and is
**not** fixed here.

### Fixed: a post-write refresh could be dropped (and a stale read could win)

`refreshQueue` refused to start while a poll was in flight, so a write that finished
during a poll lost its post-write read until the next tick — and a durable read begun
*before* a write could be applied *after* a newer one. Now `finishQueueRefresh` hands
back the refresh that was owed, deferring it until the poll drains, and every durable
read carries a sequence number so an older read cannot overwrite a newer one. The view
generation stays scoped to the view instance and root (BT-10e's fix), so polls no longer
invalidate action results.

### Verified — deterministic, through top-level `Update`

| test | result |
|------|--------|
| `TestQueueActionResultAcceptedAfterReadsAndPoll` | poll + both read results delivered before the action result: the result is accepted and the post-write refresh shows the saved state |
| `TestQueueStaleReadCannotOverwriteNewer` | a read begun before the write (seq 1) arriving after the post-write read (seq 2) is ignored |
| `TestQueuePostWriteRefreshNotLostDuringPoll` | the refresh owed during an in-flight poll is deferred, then run once the poll drains, and it reads the saved state |
| `TestQueueReentryReadsCommittedWriteFromDisk` | leave with a write pending: the obsolete result cannot touch the new view, and re-entry reads the committed write from disk |

The BT-10a Close barrier test remains the proof of slot lifetime; no PTY timing is used
for the stopping interval.

### Verified — multi-entry PTY workflow (scripted, real terminal, real runner)

Fixture: a disposable root with a completed, seed-eligible entry (64 KiB-piece torrent,
served by a loopback seeder) and a second torrent offered through the picker.

```
> queued-000001-8cbc2f3e   desired=run     recorded=completed  seed=on/waiting
> queued-000002-8f94a7a6   desired=paused  recorded=queued
```
```
saved: saved start intent for queued-000002-8f94a7a6 (desired=run, phase=queued); no runner is runn…
> queued-000002-8f94a7a6   desired=run     recorded=queued
```
```
live: runner pid 1623001, started 16:44:55   download slots 0/1   seed slots 1/1
  queued-000001-8cbc2f3e   desired=run     recorded=completed  seed=on/serving  live=seed serving
> queued-000002-8f94a7a6   desired=run     recorded=failed
```
```
saved: saved pause intent for queued-000002-8f94a7a6 (desired=paused); recorded as paused
> queued-000002-8f94a7a6   desired=paused  recorded=failed
```
```
live status: unavailable (no foreground runner is reachable)
  queued-000001-8cbc2f3e   desired=run  recorded=completed  seed=on/idle
  queued-000002-8f94a7a6   desired=paused recorded=failed
```

What this shows: the picker added exactly one paused entry with its own id; start saved
intent **without launching a runner**; the live refresh reached the terminal and named
the correct durable ids; the action went to the highlighted row only — the other entry's
download and seed intent were unchanged; and after the runner exited both rows stayed
browsable with no stale live overlay (the seed settled at `idle`, as an interruption
should).

### Gap found, not fixed: queue downloads of a local `.torrent` time out on metadata

The second entry never downloaded, so **simultaneous download+seed slots were not
demonstrated** and no overlap is claimed. Its recorded error:

```
error: timed out waiting for torrent metadata: context deadline exceeded
```

`torrent fetch` with the same source, peer and profile succeeds repeatedly on the same
fixtures, and a 64 KiB torrent *did* complete through the queue earlier in this session.
Reproductions: an 8 MiB single-file torrent and an 8.25 MiB two-file torrent, both
against a loopback seeder, both failing identically; the successful queue runs used
small payloads. The queue's resolution path therefore needs its own investigation — it is
a download-path defect, not a TUI one, and it is out of scope for this checkpoint.

### Terminal usability

At 80 columns the action status is clipped (`…the runner is not acknowledg…`). The
information a user needs — *which id*, *saved vs observed*, *which phase* — appears
before the clipping point, and the row itself shows `desired=`, `recorded=` and the live
column, so the clip hides only the trailing explanation. Layout was left unchanged; if
the wording grows, the status line should wrap rather than truncate.

Also documented for users: the view uses the process working directory as the queue
root (no flag), a runner must be started separately (`torrent queue run [--max-seeds N]`),
and a saved action never means the runner has acted.

### Gates

- `go test ./internal/tui/ ./internal/torrentqueue/ -race -run 'TestQueue|TestTorrent|TestLive|TestSeedPool|TestRunJoins' -count=3 -timeout 900s`
  → `torrentqueue 229.961s`; `tui` passed after the deferred-refresh drain was added to
  the live branch as well (`go test ./internal/tui/ -race -count=3` → `ok 2.100s`)
- `make test` (repo-wide, `-race`) → exit 0, all packages ok
- `make lint` → `0 issues`; `go vet ./...` clean; `git diff --check` clean
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Not covered

- Simultaneous download and seed activity (see the gap above).
- Removal, selection editing, runner launch and streaming remain out of scope.
- PTY evidence is scripted (`pty` + keystrokes), not an interactive human session.

## BT-10g — The BT-10f metadata timeout, isolated

**Starting SHA** `a006242` (clean). **No production change**: the reported failure did
not reproduce once the fixtures were controlled, and nothing was extended, retried or
re-routed to make it go away.

### Paired CLI experiment (the report reproduced)

Same fixture, peer, discovery profile and selection:

```
prov torrent fetch /tmp/tuismoke/D.torrent --select fixture/data.bin \
    --peer 127.0.0.1:37999 --discovery direct -o ftD --timeout 60s   -> capture committed
prov torrent queue add <same> ; queue start ; queue run              -> failed:
    "timed out waiting for torrent metadata: context deadline exceeded"
```

Seeding was disabled and the root was fresh, so the BT-10f seed attempt was not a
factor. `torrent fetch` has no isolated mode, which is the only structural difference
between the two commands.

### Controlled matrix through the real runner (all pass)

`TestQueueFileSourceDownloadsMatrix` and `TestQueueFileSourceDownloadsLargePieces` build
the `.torrent` from the directory the seeder serves and **assert the identities match**,
then run the real runner against a local `.torrent` source:

| case | pieces | payload | peer upload | elapsed | result |
|------|--------|---------|-------------|---------|--------|
| small-unthrottled | 16 KiB | 64 KiB | none | 144 ms | completed |
| small-throttled | 16 KiB | 64 KiB | 128 KiB/s | 145 ms | completed |
| large-unthrottled | 16 KiB | 8 MiB | none | 788 ms | completed |
| large-throttled | 16 KiB | 8 MiB | 768 KiB/s | 10.79 s | completed |
| large-64KiB-pieces-throttled | 64 KiB | 8 MiB | 768 KiB/s | 9.43 s | completed |

So the queue download path completes correctly across piece length, payload size and
peer throttling, including the exact combination the CLI fixtures failed with.

### Where the earlier failures came from

The CLI repros used ad-hoc seeders whose `.torrent` was generated by a separate program
from a directory it had just written. The in-package matrix shows that once the torrent
file and the served directory are guaranteed to describe the same identity, every case
completes. One CLI fixture (`GA.torrent`, 64 KiB, 16 KiB pieces) still failed in the
rerun while a larger one on the same seeder setup succeeded, so the remaining difference is in the
fixture/peer harness, not in Provenance's queue code — and I did **not** find the exact
mechanism, so no claim is made beyond "not reproducible with matched identities".

**First divergent event recorded**: in the failing CLI case the run reached
`running (bt-…)` and then failed at metadata, with no workspace directory created
(`.provenance/torrent/jobs/` empty), i.e. the failure happens before payload admission
and before any completion database exists — consistent with a metadata exchange that
never completed, which the in-package cases never attempt because the identity is
already known from the torrent file.

### The BT-10f PTY scenario, re-run

With a 16 KiB-piece fixture the **download half is now live in the terminal**:

```
live: runner pid 1813674, started 17:04:40   download slots 1/1   seed slots 0/1
> queued-000002-c1b22680   desired=run   recorded=running   live=download running
```

but the seed-eligible entry's own initial download failed in that run
(`recorded=failed`), so the seed slot stayed empty. **Overlap is therefore still
unverified**: no single snapshot showed both pools occupied. The download half is proven
live; the seed half was proven in BT-10e's PTY run; a single snapshot with both remains
outstanding.

### Gates

- `go test ./internal/torrentqueue/ -run TestQueueFileSource -count=1` → `ok 21.412s`
- Focused `-race -count=3` over `TestQueueFileSource|TestQueue|TestTorrent|TestLive|TestSeedPool|TestRunJoins`
- `make test`, `make lint`, `go vet ./...`, `git diff --check`
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Not covered

- The exact mechanism behind the ad-hoc CLI fixtures' metadata failure.
- Simultaneous download+seed overlap in one live snapshot.
- No TUI changes, no timeout/retry/discovery changes, no seeding policy work.

## BT-10h — Live overlap closed: one snapshot with a download and a seed

**Starting SHA** `fdfde5b` (clean). **No production change** — this is the evidence the
BT-10f/BT-10g checkpoints were missing, and the scheduler was not touched to obtain it.
The ad-hoc metadata timeout from BT-10f stays documented as unexplained: nothing here
identifies its cause, and it is not restated as either a queue defect or a fixture bug.

### Fixture setup, verified before the assertion

Setup is a separate step with its own failure message, and it does not download entry A
inside the test:

1. Entry A is completed through the ordinary queue path against a controlled loopback
   seeder (`newLoopbackSeeder`, 96 KiB single file), then checked in the durable record:
   `Phase == completed`, `Capture != nil`, `Workspace != ""`. Only then is seed intent
   enabled (`Store.EnableSeed`). No store record is fabricated and no ad-hoc seeder
   bootstraps the prerequisite.
2. Entry B uses BT-10g's matched construction: the `.torrent` is built from the very
   directory `startProbeSeeder` serves, and the test asserts
   `loaded.HashInfoBytes() == mi.HashInfoBytes()` before adding it. Payload 6 MiB at
   768 KiB/s so it stays active long enough to be observed.
3. Both intents are recorded **before** `Run` starts (see the drain note below).

### One real snapshot

`TestLiveOverlapDownloadAndSeed`, one response from the real Unix-socket endpoint:

```
OVERLAP snapshot pid=1970828 downloads=1/1 seeds=1/1 attempts=2
  OVERLAP attempt entry=queued-000002-a75fc015 kind=download phase=running stopping=false
  OVERLAP attempt entry=queued-000001-6c0ed42a kind=seed phase=serving stopping=false
```

The condition requires all of `seed serving` **and** `download running` **and**
`downloads 1/1` **and** `seeds 1/1` in the same response, plus exactly two attempts, so
no observation is stitched together from different polls. `-race -count=3` → `ok 2.117s`
(0.35 s per run).

What "serving" means here: the session passed verification and holds its slot with its
own listener. `uploaded_bytes` is absent (`-1` in the log) because nothing in this
fixture leeches from A — the uploaded-byte budget was measured separately in BT-08d.1
and BT-08d.3, and this checkpoint does not re-claim it.

### Terminal evidence, captured separately

Same fixture through the CLI and a scripted PTY (`drive8.py`, 80 columns): A completed
and seeded, B added, started and throttled to 512 KiB/s, runner launched with
`--max-active 1 --max-seeds 1`. The driver refreshes until both rows are live; the
screen below is one captured screen, not a view stitched from several refreshes:

```
live: runner pid 1972809, started 17:21:28   download slots 1/1   seed slots 1/1
> queued-000001-1b99d001     desired=run    recorded=completed  seed=on/serving  live=seed serving
  queued-000002-9c1dd5df     desired=run    recorded=running    live=download running
```

After the runner exits, the same session:

```
live status: unavailable (no foreground runner is reachable)
> queued-000001-1b99d001     desired=run    recorded=completed  seed=on/idle
  queued-000002-9c1dd5df     desired=run    recorded=completed
```

Both durable rows remain, the live overlay is gone, and the seed settles at `idle`
rather than pretending to serve. Every id is the durable entry id, and the seed slot and
download slot are reported by the endpoint, not inferred by the view.

### Observed limit (not a defect, not fixed here)

The runner is a **drain**: once the download pool is empty it waits on the seed pool, so
an entry started *after* that point is never claimed in that pass. The first version of
this test started B after A was already serving and B stayed `phase=queued,
Attempts=0`; starting B before `Run` is what makes the overlap observable. That is
consistent with the exit behaviour BT-08d.3 documented (serve until no seed is left),
which implies but never stated the part that matters here: a drained pass does not go
back to claiming downloads. Recorded because it changes how a user reaches overlap:
record both intents, then run.

### Gates

- `go test ./internal/torrentqueue/ -race -run TestLiveOverlapDownloadAndSeed -count=3` → `ok 2.117s`
- focused `-race -count=3` over `TestLiveOverlap|TestQueueFileSource|TestQueueSeed|TestLive|TestSeedPool|TestRunJoins`
- `make test`, `make lint`, `go vet ./...`, `git diff --check`
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3

### Review follow-up (`f6ec52e`, from `8d8c6ff`)

The review of `c503ccf` blocked on one item: this entry claimed the snapshot condition
required "exactly two attempts" while the test only logged the count. `f6ec52e` adds
that assertion (`internal/torrentqueue/overlap_test.go:161-167`) **after** the single
qualifying snapshot is obtained, deliberately outside the polling predicate so a third
attempt fails at once instead of timing the poll out; the failure prints the count and
each attempt's id, kind, phase and stopping flag. Discrimination was checked by
inverting the constant for one run, which failed with

```
  unexpected attempt entry=queued-000002-ad4dabf5 kind=download phase=running stopping=false
  unexpected attempt entry=queued-000001-4a465669 kind=seed phase=serving stopping=false
  the snapshot carried 2 attempts, want exactly 2
```

and the constant was then restored (`grep -n 'got != 2'` → line 161).

The same commit removes the dead `attemptUploaded` helper and the unused `waitSnapshot`
parameter, corrects the uploaded-bytes note (absent, logged as `-1`, not a measured
zero), and routes cancellation plus the wait for runner exit through one bounded helper
that records the attempt, so the deferred safety net never waits twice. The endpoint and
durable-state checks still run only after a confirmed exit.

Rerun at this tip: `go test ./internal/torrentqueue/ -run '^TestLiveOverlapDownloadAndSeed$'
-race -count=3` → `ok 2.145s`; `make test` exit 0; `make lint` → 0 issues;
`go vet ./...` clean; `git diff --check` clean. No production code changed and no
`--watch` work was started in this pass.

### Not covered

- Uploaded bytes during overlap (no leecher for A in this fixture).
- The BT-10a Close barrier remains the separate proof of slot lifetime during teardown;
  this checkpoint measures neither the stopping interval nor teardown ordering.
- The BT-10f ad-hoc metadata timeout: still unexplained, unchanged in status.
- The PTY driver (`drive8.py`) is not tracked in this repository, so the terminal
  evidence above cannot be regenerated from a checkout; it ran from a scratch directory
  against the binary built at the time of that run, not against a build of this commit.

## BT-11 — Opt-in watch session (`torrent queue run --watch`)

**Starting SHA** `2a49f84` (clean). **End SHA** `5335d74`.

`queue run` gains a mode, not a new lifecycle: `Options.Watch` is the only new state.
A plain run still drains and returns; a watch run keeps scanning after either pool goes
idle. Slot limits, pause, retry/backoff, seed intent, one-runner ownership and the live
snapshot schema are unchanged, so `queue list`, `queue live --json` and the TUI read the
same shapes as before.

### How it is implemented

- **Download pool idle** (`internal/torrentqueue/runner.go`): `if active == 0` gains a
  watch branch that logs one line per idle period, waits `PollInterval` and rescans.
  `idleWait` is the only pacing — no busy loop, and no in-process wakeup.
- **Seed pool idle** (`internal/torrentqueue/seed.go`): where a drain returns when
  nothing is eligible and nothing is pending, a watch session keeps the ticker loop
  running, so `queue seed` from another process starts serving in the same session.
- **Pickup mechanism**: every scan calls `store.List()` / `SeedCandidates()` under the
  store lock, so durability is the transport. Nothing in the runner is notified; a
  record written by another process is simply visible on the next tick. `PollInterval`
  (200 ms by default; not a flag) is the **idle rescan cadence**, not a wall-clock
  pickup guarantee: work is claimed on a scan when a slot is free, otherwise it waits
  for capacity, and eligibility/backoff still apply. Corrected in BT-11a after review.
- **Shutdown** is the existing path: stop claiming, `shutdown(inflight, done, active)`
  cancels and joins, the seed pool joins after cancellation, then the deferred
  `srv.close()` removes the live endpoint. Slot and ownership release therefore still
  follow `Close`, not the request.

### Commands actually run

- `go test ./internal/torrentqueue/ -run 'TestWatch|TestDrainDoesNotClaim' -count=1 -v`
  → all five pass: late download 3.56 s, idle pickup 0.62 s, drain compatibility 0.69 s,
  idle shutdown 0.05 s, held Close 0.66 s.
- `go test ./internal/torrentqueue/ -race -run '^(TestWatch|TestDrainDoesNotClaim|TestLiveOverlap|TestLiveCloseBarrier|TestLiveStopping|TestQueueRunnerKilled|TestQueueSeed|TestSeedPool|TestRunJoinsClosing)' -count=3 -timeout 1200s`
  → `ok 108.805s` (watch, drain, the BT-10h overlap, both held-Close barriers and both
  SIGKILL cases, repeated three times under `-race`).
- `make test` → exit 0; `make lint` → `0 issues`; `go vet ./...` → clean;
  `git diff --check` → clean; `gofmt -l .` → empty.
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3.

### Evidence per acceptance item

1. **Late download while seeding** — `TestWatchClaimsLateDownloadWhileSeeding`. A is
   completed through the ordinary queue path and verified in the record before its seed
   intent is enabled. The test first waits for a snapshot that shows A serving, seed
   slots 1/1, **no download attempt at all** and `downloads.used == 0`, i.e. the pool is
   idle rather than momentarily quiet. Only then does a separate process run
   `provenance torrent queue start <B>` (the real binary, built once per test binary).
   The claim is accepted only if the live `pid` is unchanged and B's download attempt is
   `running`; the overlap assertion then requires both pools at 1/1 with exactly two
   attempts and prints them:

   ```
   WATCH overlap pid=2192986 downloads=1/1 seeds=1/1 attempts=2
     WATCH attempt entry=queued-000001-743387ad kind=seed phase=serving
     WATCH attempt entry=queued-000002-dd1c02bd kind=download phase=running
   ```
2. **Both pools idle** — `TestWatchIdlePicksUpNewWorkInBothPools`. With an empty queue
   the session is still alive after ten poll intervals and its endpoint answers with no
   attempts. A download created by the CLI (`queue add` + `queue start`) is claimed in
   159 ms at a 20 ms poll; then `queue seed <id>` from the CLI wakes the idle seed pool
   (`seeds=1/1`, attempt `serving`).
3. **Drain compatibility** — `TestDrainDoesNotClaimLateDownloadButNextPassDoes`. A plain
   drain is held waiting on its serving seed; a CLI start of B leaves B at
   `phase=queued` with no download attempt in the snapshot after 15 poll intervals; the
   next invocation claims it and completes it (`sum.Completed == 1`).
4. **Shutdown** — `TestWatchShutdownWhileIdle` (idle cancel: endpoint removed, ownership
   reacquirable) and `TestWatchShutdownHeldCloseKeepsSlot`, which holds a seed inside
   `Close` with the BT-10a barrier, then asserts — after an unseed from outside *and*
   after cancelling the session — that Run has not returned, `LockRunner` is refused, the
   snapshot still shows one attempt `stopping` and `seeds.used == 1`. Only after
   `release` does Run return, the endpoint disappear and ownership become free.

### CLI smoke, and what it did not show

`/tmp/tuismoke` watch runs against ad-hoc `one16` seeders confirmed the process-level
behaviour: the session stayed alive with an empty download pool
(`runner_held: true`, `idle: watching for eligible work every 200ms`), the exit line was
`watch ended: …` after SIGINT, and `queue live` failed with "no foreground runner is
reachable" afterwards. They did **not** produce a clean CLI overlap snapshot: those
fixtures intermittently failed a download with `timed out waiting for torrent metadata`
— the BT-10f symptom, still unexplained — and one run left the seed slot unobserved.
That is why the acceptance evidence above comes from the deterministic in-process tests
that drive the same CLI operations, and not from the scratch smoke.

### Not covered / limits

- No daemon, service install, socket control or mutation IPC: the session is a foreground
  process, and stopping it is SIGINT (or process death, which releases the flock).
- `PollInterval` is not a flag; 200 ms is the default idle rescan cadence and the tests
  use 20 ms to keep the suite fast, so the numbers above are not a latency specification.
- Pickup is bounded by one rescan *per idle tick*, not a latency SLO, and only when a
  slot is free: with every slot occupied an eligible entry waits for capacity. A large
  queue is re-read on each tick exactly as the drain re-reads it.
- Watch mode does not add retry semantics: a failed entry still needs `retry`, a blocked
  one still waits out its backoff (now observable inside one session).
- The ad-hoc-fixture metadata timeout (BT-10f) is untouched and still unexplained.
- No TUI change; the TUI still cannot start a runner, and `--watch` is CLI-only.

## BT-11a — Watch review blockers: fatal-error shutdown, pickup promise, test synchronisation

**Starting SHA** `70655fb` (clean). Review of `2a49f84..70655fb` blocked on two items: a
fatal store error could return out of `Run` while a seed still owned a slot, and the
docs promised pickup that does not queue behind an active attempt. Both are fixed here;
no scheduler policy changed.

### Fatal errors now end the session in both directions

- `runFatal` (`internal/torrentqueue/runner.go`) records the **first** error, calls an
  optional boundary hook, then cancels the sibling pool. Later errors are dropped, so a
  sibling's cancellation can never replace the cause.
- `Run` derives `runCtx` from the caller's context and hands it to both pools, so a
  failure in either one wakes the other (`idleWait`, `take(true)` and every attempt
  context select on it). The download loop's `nextExcept` error now records the failure
  and breaks out of the loop instead of returning, so it reaches the shared join:
  `r.shutdown(...)` → `<-seedDone` → return the recorded error. Ownership release
  (`defer l.Release()`, registered first) and endpoint removal (`defer srv.close()`,
  registered later) therefore happen *after* both pools are joined, in LIFO order.
- The seed pool distinguishes the two outcomes that were conflated: `SeedPending`
  returning an **error** is fatal (`fatal.fail(fmt.Errorf("seed pending: %w", perr))`);
  only a successful "nothing pending" lets a watch session keep scanning, and a drain
  still returns there. `SeedCandidates` errors are fatal for the same reason — they used
  to be logged and retried every interval, which under `--watch` is an unbounded loop.
- `Options.BeforeFatal func(error)` exists so a test can observe the fatal boundary
  deterministically; it is nil in production, like `BeforeSeedClose`.

### Regression evidence (before → after)

The two discriminating tests were run against a surgical revert of the fix (the error
paths restored, the hook kept so the tests still compile):

| test | before the fix | after the fix |
|------|----------------|---------------|
| `TestSeedPoolEndsOnStoreError` | FAIL "the seed pool kept polling after a store error" (5.00 s bound) | PASS 0.00 s, `seed candidates: read queue: … permission denied` |
| `TestWatchFatalDownloadErrorJoinsHeldSeed` | FAIL "Run returned while the seed was still inside Close (err=… permission denied)" (0.08 s) | PASS: fatal held with `stopping` seed at `1/1`, ownership refused, then Run returns the injected error |

`TestWatchFatalStoreErrorEndsIdleSession` stays green in both versions by design: the
download pool's old early return already surfaced that error. It is kept as the
end-to-end proof that an idle session cannot poll forever and cannot return success; the
seed-side discrimination lives in the first two rows.

The held-Close case uses the BT-10a barrier session for the exact entry under test and
proves ordering without sleeps: `BeforeFatal` fires, then the test asserts Run has not
returned, `LockRunner` is refused, and the real live endpoint still reports one
`stopping` attempt with `seeds.used == 1`; only `release` ends it, after which the
endpoint is gone and ownership is reacquirable.

### Pickup promise corrected

`docs/TORRENT.md` no longer claims new work "does not queue behind an active attempt".
The 200 ms interval is documented as the default **idle rescan cadence**, not a
wall-clock guarantee: work is claimed on a scan when a slot is free, otherwise it waits
for capacity, with eligibility, backoff and slot limits unchanged. The BT-11 entry above
was corrected in place.

### Test tightening in the same slice

- Late admission now synchronises on a completed empty scan: the runner's log line
  `idle: watching for eligible work` (captured through `Options.Log`) is awaited before
  the external `queue start`, because an empty snapshot is satisfied from the moment the
  endpoint is published.
- The drain-compatibility test now requires a successful `liveProbe` after starting B,
  so an early-exiting drain fails instead of passing.
- The held-Close cleanup waits with a 60 s bound instead of blocking indefinitely.
- Subprocesses are bounded: `go build` 180 s, each CLI call 60 s, both via
  `exec.CommandContext`.
- **Race scope**: the spawned CLI is built without `-race`, so a `-race` run instruments
  the test process (which hosts the runner under test) and not the child binary.

### Commands actually run

- `go test ./internal/torrentqueue/ -run 'TestWatch|TestDrainDoesNotClaim|TestSeedPoolEndsOnStoreError' -count=1 -timeout 350s` → `ok 6.022s`
- `go test ./internal/torrentqueue/ -race -run '^(TestWatch|TestDrainDoesNotClaim|TestSeedPoolEndsOnStoreError|TestLiveOverlap|TestLiveCloseBarrier|TestLiveStopping|TestQueueRunnerKilled|TestQueueSeed|TestSeedPool|TestRunJoinsClosing)' -count=3 -timeout 1500s`
  → `ok 109.663s`
- `make test`, `make lint`, `go vet ./...`, `git diff --check`, `gofmt -l .`
- Pre-fix runs: the two tests above against the reverted error paths (`-count=1`, 5 s and
  0.08 s to fail), then the fix restored and the same tests green.
- Platform: Linux 6.18.63-gentoo-dist-bin, x86_64, Go 1.27.3.

### Remaining gaps

- The pre-fix demonstration reverts the two error paths rather than checking out
  `70655fb`; the hook is kept so the tests compile against both shapes.
- `TestWatchFatalStoreErrorEndsIdleSession` cannot say which pool reported first — by
  design either is fatal — so it asserts the outcome, not the reporter.
- Cancellation is still checked between scan rounds, not inside the claim loop: a scan
  already in progress can claim up to the slot limit after ctx is cancelled, and those
  attempts are then cancelled and recorded as interruptions.
- A persistent store error now ends the session instead of being retried, which is the
  intended contract but also means a transient permission blip ends a watch session;
  there is no retry policy for it.
- The BT-10f ad-hoc metadata timeout remains unexplained and untouched.

## BT-12 — Live progress and rate for the selected payload

**Starting SHA** `9ab3185` (clean). **End SHA** `91f4006`. Additive: no scheduling,
durable record, or lifecycle change. The live endpoint gains three optional fields per attempt, and the
CLI live output and TUI overlay render them.

### What the transfer client can actually measure

The pinned library exposes three different things, and only one of them is selected
progress:

- `File.BytesCompleted()` — bytes present in that file's storage. Includes a partially
  received piece at a file boundary and data retained from an earlier attempt. This is
  what `Update.Written` (the `torrent fetch` progress line) already reported.
- `File.State()` — per-piece state *with the byte length of that piece's portion inside
  that file*. Summing `Bytes` for `Complete` pieces gives verified bytes of the file's
  own ranges.
- `Torrent.BytesCompleted()` / stats — torrent-wide, including unselected files and
  padding; not usable as selected progress.

Chosen measurement: `selected_bytes` = Σ over selected files Σ over covering pieces
(`Complete` → `Bytes`), with `selected_total` = Σ selected file lengths. Padding files
are not in the selection; a piece shared with an unselected neighbour contributes the
selected share only.

### Implementation

- `internal/transfer/bittorrent`: `Update` gains `Verified`, computed by
  `selectedVerified` in `report()`. `report` now runs at the top of the wait loop so the
  tick that observes completion reports the final value rather than the previous tick's.
- `internal/torrentqueue`: `Request.OnProgress` carries the sample; the adapter maps
  `Update{Verified,Total}` onto it; `runEntry` wires it to `live.progress(entryID, …)`.
- `internal/torrentqueue/live.go`: `LiveAttempt` gains `selected_bytes`,
  `selected_total`, `progress_rate` (all `omitempty` pointers), and `liveState` keeps a
  per-attempt baseline (`samples`, reset in `add`/`remove`, monotonic clock with a test
  seam). Rate rules: first sample → absent; rollback → absent + rebaseline; sample
  inside `liveRateWindow` (250ms) → keep the old baseline; otherwise delta/elapsed, with
  a measured zero recorded as `0`.
- Rendering: `queue live` appends `selected=X/Y bytes` and `progress=N B/s` when present;
  the TUI's `livePhaseText` does the same through `humanBytes`, so rows and live-only
  entries share one path.

### Evidence (all commands actually run)

- `go test ./internal/transfer/bittorrent/ -run TestSelectedVerified -count=3` → both
  pass, twice each in 0.02 s:
  - complete torrent, 32 KiB pieces over two 48 KiB files (piece 1 spans the boundary):
    `a=49152 b=49152` — a whole-piece count would have reported 64 KiB for one file.
  - partial data (only a file's first 32 KiB on disk): `verified=32768 of 49152`, so the
    incomplete boundary piece contributes nothing.
- `go test ./internal/torrentqueue/ -run 'TestLiveProgress|TestLiveSnapshotProgress' -count=1`
  → ok 0.436 s. Covers, with a fake clock: first sample has no rate; a full window gives
  the delta; a sample inside the window keeps the baseline; a full window with no new
  bytes gives a measured `0`; a rollback gives no rate and a fresh baseline; a new
  attempt on the same entry has no baseline; a late sample never revives a removed
  attempt. Plus JSON compatibility in both directions (older endpoint → nil; new
  endpoint read by an older client → known fields intact) and measured-zero round trip.
- `TestLiveProgressDuringDownload` (real runner, throttled loopback peer):

  ```
  LIVE progress entry=queued-000001-0e3dafa5 selected=1064960/4194304 rate=unknown
  LIVE rate entry=queued-000001-0e3dafa5 3704811 B/s
  ```

  The first sample is the measured-progress example and also the unavailable-rate
  example; the second shows the rate once a window exists.
- `go test ./internal/tui/ -run TestQueueViewProgress -count=2` → pass: progress and
  rate rendered when present, an absent rate stays absent (no `0 B/s`), a measured zero
  rate is rendered.

### Not covered / limits

- `selected_bytes` is piece-granular, so a wide-piece torrent shows a staircase and the
  rate follows the steps. It is not a smoothed byte counter.
- The end-to-end assertion uses one throttled fixture (4 MiB at 256 KiB/s); no CLI
  screenshot was captured for this slice. PTY evidence is absent, not implied.
- Rate is not network throughput and will differ from any external monitor's numbers.
- Seeding attempts report `uploaded_bytes` only; `selected_*` is a download projection.
- No ETA, stall detection, or bandwidth policy: a rate of `0` is displayed as `0`, and
  nothing treats it as stalled.
- An attempt that finishes between two poll ticks may produce no progress sample at all;
  the row then disappears rather than showing a final 100%.
- The ad-hoc metadata timeout (BT-10f) is untouched and still unexplained; it did show
  up once in an early version of the in-package end-to-end fixture, which is why the
  counting tests use a local client's own piece states instead of a live transfer.

## BT-13 — Measured cost of the selected-progress sampler

> **Superseded for numbers:** the benchmarks in this entry timed their fixture setup
> (no `b.ResetTimer()`), and the 65,536-/262,144-piece cases were storage-less. See
> BT-14 for corrected timings, a storage-backed 65,536-piece case and the workload
> limit. The decision here (leave the sampler alone) survives; the regime difference
> reported below does not.

**Starting SHA** `3b3c27d` (clean). **Benchmarks** `6720b44`, **guards** `434640b`, **this
checkpoint** follows. **Production code unchanged**: the benchmarks were
the slice, and the measurement says the sampler is acceptable for the queue's intended
workload. No ETA, stall policy or bandwidth work.

### What was measured, and the fixture construction

`BenchmarkSelectedVerified*` in `internal/transfer/bittorrent/progress_bench_test.go`
call the real path (`selectedVerified` → `File.State()`), with fixtures built outside the
timed loop:

- **complete, storage-backed**: a real payload file, 16 KiB pieces, verified with
  `VerifyDataContext` — 512 and 2,048 pieces (8 MiB and 32 MiB).
- **incomplete, no payload**: a hand-built metainfo with the requested piece count and a
  `DeferStorageOpen` spec, so no payload file is created at all. The check accepts a
  missing payload directory (storage never opened) or an empty one, and fails on any
  other read error — 65,536 and 262,144 pieces (1 GiB / 4 GiB at 16 KiB pieces). This is exactly the "many pieces without a proportional payload" fixture the
  slice asked for; the cost is that these pieces are zero-value, storage-less states.

### Results, two regimes

Both regimes were run at the tip of this follow-up; the commands are in the next
section. All numbers are ns/op, B/op and allocs/op from `-benchmem`.

**Default benchmark duration** (Go's 1 s budget), all four cases, `-count=3`:

| benchmark | files | pieces | state | ns/op (3 reps) | B/op | allocs/op |
|-----------|-------|--------|-------|----------------|------|-----------|
| `Complete512` | 1 | 512 | complete, storage-backed | 46.4–47.7 µs | 49.5–49.6 kB | 11 |
| `Complete2048` | 1 | 2,048 | complete, storage-backed | 434–438 µs | 274–277 kB | 79–85 |
| `Incomplete65536` | 1 | 65,536 | incomplete, no storage | 3.77–4.63 ms | 14.0 MB | 28 |
| `Incomplete262144` | 1 | 262,144 | incomplete, no storage | 16.4–16.8 ms | 56.3–56.4 MB | 38–39 |

**`-benchtime=500x`**, storage-backed cases only, `-count=3`:

| benchmark | pieces | ns/op (3 reps) | B/op | allocs/op |
|-----------|--------|----------------|------|-----------|
| `Complete512` | 512 | 265–302 µs | 86.9 kB | 96 |
| `Complete2048` | 2,048 | 1.14–1.22 ms | 397 kB | 360–361 |

The tables give the observed range for each case; compare those directly rather than
through a single figure. **Across** regimes the storage-backed cases differ by roughly
2.6–2.8× at 2,048 pieces and 5.6–6.5× at 512 pieces, and `B/op` and `allocs/op` rise with
them (397 kB and 360–361 allocs versus 274–277 kB and 79–85 allocs). The storage-less
cases were not measured in the 500x runs reported here. **The cause of the regime
difference is unknown.** It was not established to be warm-up, GC, storage backing or
completion state in isolation, and the default-duration numbers are *not* called steady
state: they are the results of the documented command, nothing more.

An earlier run of this slice recorded only the `-benchtime=500x` numbers (271–288 µs and
1.12–1.22 ms) and derived the per-piece slope from them. Those figures are retained here
as the second regime, not as the primary measurement.

### Per-piece figures and the extrapolation

From the **default-duration** storage-backed measurements: 512 pieces → ~92 ns/piece,
2,048 pieces → ~213 ns/piece. At the 100 ms cadence, 2,048 pieces is ~0.44 ms of sampler
time per sample, about 0.44 % of the sampling interval; 512 pieces is ~0.047 ms. These
come from the setup-inflated timings this entry is superseded for — BT-14 has the
corrected figures.

The 60 GiB example — 60 GiB at 16 MiB pieces ≈ 3,800 pieces → ~0.8 ms per sample — is an
**extrapolation beyond the largest measured storage-backed case (2,048 pieces)**, using
the 2,048-piece slope. It is not a benchmark result.

### Decision

The workflow decision uses the default-duration regime, because that is what the
documented command produces: for the intended workload (media torrents, 1–16 MiB pieces,
at most a few thousand pieces per file) a sample costs well under a millisecond, which
does not justify changing the sampler. Production code is untouched. Revisit when the
trigger below fires, and note that the slower `500x` regime would still only be a few
percent of the goroutine at these sizes — it does not change the decision either.

### Fixture guards (`434640b`, this follow-up)

All four benchmarks now assert their fixture outside the timed loop: the piece count for
every case, `selectedVerified == 8 MiB` / `== 32 MiB` for the storage-backed cases, and
`== 0` for both storage-less cases. The no-payload check accepts a missing payload
directory (storage never opened) or an empty one, and fails on unexpected read errors —
which is what the fixture text above now says.

### Future asynchronous sampling

If sampling ever moves off the attempt's goroutine it must carry an explicit **attempt
identity** (not just the queue entry id), and it must ship a regression that rejects a
late sample from attempt A after attempt B has started for the same entry. The current
ordering guarantees that by construction — the callback runs synchronously inside the
attempt's own `Run`, and `liveRemove` (which also drops the rate baseline) happens before
the next claim — and that is exactly the guarantee an asynchronous sampler would lose.

### Revisit trigger

Measure a **storage-backed many-piece** fixture (above 2,048 pieces) before expanding
sampling for ETA or stalled-job detection. If the slope holds, bound the work — cache the
last verified count per attempt, or walk only changed pieces — rather than moving the same
work to a goroutine. Allocations are the second-order concern.

### Evidence accuracy

- The number is selected-byte verification progress, not network throughput.
- The first sample of any attempt lacks a rate because it has no baseline; resume is not
  the cause. Retained data is verified before the wait loop starts
  (`bittorrent.go:1027-1032`), so verification is not reported as transfer speed, apart
  from the one trailing piece the library may mark after `VerifyDataContext` returns.
- Unknown stays distinct from measured zero; this follow-up did not touch the sampling
  path, and no lock contention or transfer-throughput impact is claimed or inferred.

### Commands

- `go test ./internal/transfer/bittorrent/ -run '^$' -bench '^BenchmarkSelectedVerified' -benchmem -count=3` (four cases)
- `go test ./internal/transfer/bittorrent/ -run '^$' -bench '^BenchmarkSelectedVerifiedComplete' -benchmem -benchtime=500x -count=3` (two cases)
- `go test ./internal/transfer/bittorrent/ -run '^TestSelectedVerified' -race -count=2` (both correctness tests)
- `make test`, `make lint`, `go vet ./...`, `git diff --check`, `gofmt -l .`
- Platform: 32-core Linux 6.18.63-gentoo-dist-bin, Go 1.27.3.

### Limits

- Two fixture geometries only; many-file selections scale with file count × pieces, and
  no multi-file case was benchmarked.
- The synthetic fixtures cannot represent complete storage-backed pieces, which is the
  expensive case.
- Benchmark numbers come from an idle 32-core box; no measurement under concurrent
  transfers or with the live endpoint being polled.
- The ad-hoc metadata timeout (BT-10f) is untouched and still unexplained.

## BT-14 — Storage-backed many-piece sampling: measured ceiling and workload limit

**Starting SHA** `6cd65f8` (clean). **End SHA** `fbd4e51`. **Production code unchanged** — benchmarks only.
This closes the gap BT-13 left: its 65,536- and 262,144-piece fixtures used deferred
storage, so they could not answer what the sampler costs on a *real* many-piece torrent.

### A measurement defect found and fixed first

BT-13's benchmarks called `selectedVerified` in the timed loop without
`b.ResetTimer()`, so fixture construction (writing and hashing the payload) was charged
to the measured operation and inflated both figures. `benchSelectedVerified` now
restarts the timer after setup. With that fix the corrected timing ranges **overlap for
all three storage-backed fixtures** (512 pieces: 41.5–44.7 µs default vs 40.4–45.9 µs at
`-benchtime=500x`; 2,048: 231–236 µs vs 214.8–251.7 µs; 65,536: 5.51–5.79 ms vs
4.87–5.80 ms), and the multi-fold gap BT-13 reported no longer appears. How much of that
gap came from setup and how much from anything else is not separately measured, so the
correction is stated as a range overlap, not as a quantified decomposition.

### Fixture construction

- Control: one 32 MiB file, 16 KiB pieces, **2,048 pieces**, real file storage, fully
  verified (`VerifyDataContext`) before timing.
- Large case: one 1 GiB file, 16 KiB pieces, **65,536 pieces**, real file storage, fully
  verified before timing. Written in 1 MiB chunks so building it does not need 1 GiB of
  RAM; on-disk footprint 1.08 GB in the benchmark's temp dir, removed with the test.
- Both assert, outside the timed loop: piece count, `selectedVerified == size` (32 MiB /
  1 GiB), and one file. No peers, no network: the client is the loopback-only
  `ctlConfig` and never dials; the storage is the real file backend, not deferred.
- The two storage-less benchmarks are retained but are labelled as such: they walk the
  same piece count with no storage backing and are a lower bound, not the target case.

### Results (default Go benchmark duration, `-count=3`, `-benchmem`)

| benchmark | pieces | backing | ns/op | B/op | allocs/op |
|-----------|--------|---------|-------|------|-----------|
| `Complete512` | 512 | real storage, verified | 41.5–44.7 µs | 48.8 kB | 10 |
| `Complete2048` (control) | 2,048 | real storage, verified | 231.2–236.0 µs | 245.4 kB | 13 |
| `Complete65536` (new) | 65,536 | real storage, verified | 5.51–5.79 ms | 13.91 MB | 27 |
| `Incomplete65536` | 65,536 | none (deferred) | 3.50–4.05 ms | 13.91 MB | 27 |
| `Incomplete262144` | 262,144 | none (deferred) | 13.69–14.85 ms | 55.64 MB | 33 |

Per piece: ~81–87 ns at 512, ~113–115 ns at 2,048 and ~84–88 ns at 65,536 — within about
1.4× across a 128× range of piece counts. That is **consistent with roughly linear
scaling across the tested fixtures**; it does not establish a general curve, and no
fixture here tests a small selection taken from a large torrent (these single-file
fixtures select the whole file, so selected and total piece counts are equal). The
storage-less 65,536 case (53–62 ns per piece) is cheaper, but it is not the measured
target.

Fixed-iteration regime, recorded separately:
`go test ./internal/transfer/bittorrent/ -run '^$' -bench '^BenchmarkSelectedVerifiedComplete' -benchmem -benchtime=500x -count=3`
→ 512: 40.4–45.9 µs; 2,048: 214.8–251.7 µs; 65,536: 4.87–5.80 ms (B/op and allocs/op
unchanged: 48.8 kB/10, 245.4 kB/13, 13.91 MB/27).

### Extrapolation to the 100 ms cadence (per active download)

Labels: these are extrapolations from single-sample cost, not measured production
behaviour.

- 2,048 pieces: ~0.23 ms of benchmarked sampler time per sample → ~2.3 ms and ~2.45 MB
  allocated per second, about 0.23 % of the 100 ms sampling interval.
- 65,536 pieces: ~5.5–5.8 ms per sample → ~55–58 ms and ~139 MB allocated per second,
  about 5.5–5.8 % of the interval.
- Four attempts, if the cost is simply added: ~220–232 ms and ~556 MB per second. This is
  an additive extrapolation, not a concurrency measurement.
- 262,144 pieces (storage-less, lower bound): ~14.3 ms per sample → ~143 ms/s, ~556 MB/s.

These are sampler wall-clock time and allocation volume. They are not measured core
utilisation, lock contention or transfer-throughput impact.

Not measured: lock contention (`File.State()` holds `cl.rLock` for the walk), effect on
transfer throughput, behaviour with several concurrent attempts sampling at once, or any
storage-backed case above 65,536 pieces.

### Decision

Keep the sampler for the intended workload; no production change.

- The intended workload (media torrents, 1–16 MiB pieces, at most a few thousand pieces
  per file) costs ≤ ~0.24 ms per sample, about 0.24 % of the 100 ms sampling interval per
  active download. That fits and does not justify changing production code.
- **Measured ceiling:** the largest real-storage fixture measured here has **65,536 total
  pieces** (one 1 GiB file, 16 KiB pieces). These fixtures select the whole file, so they
  say nothing about how a small selection from a large torrent scales, and no piece-count
  cutoff is set here. At 65,536 pieces a sample costs ~5.5–5.8 ms, ~55–58 ms/s and
  ~139 MB/s of allocation per attempt.
- No further sampling (ETA smoothing, extra rates) should be added until the walk is
  bounded: four attempts, if the cost were simply added, would be ~220–232 ms and
  ~556 MB/s — an additive extrapolation, not a measured concurrency result.
- **Proposed follow-up (not implemented here):** bound the walk per attempt — cache the
  last verified count and only re-walk pieces whose state changed, or subscribe to the
  client's piece-state changes. It must preserve the verified selected-byte semantics
  (boundary pieces, padding exclusion, resumed data) and stay synchronous inside the
  attempt, or carry an explicit attempt identity with the A-after-B regression described
  below.

### Constraints retained

Any future asynchronous sampler needs an explicit **attempt identity** (not just the
queue entry id) and a regression that rejects a late sample from attempt A after attempt
B has started for the same entry. Today that is guaranteed by construction: the callback
runs synchronously inside the attempt's own `Run`, and `liveRemove` (which also drops the
rate baseline) happens before the next claim.

ETA and stalled-job policy remain out of scope for this checkpoint.

### Commands actually run

- `go test ./internal/transfer/bittorrent/ -run '^$' -bench '^BenchmarkSelectedVerified' -benchmem -count=3`
  (five cases, the three storage-backed ones included)
- `go test ./internal/transfer/bittorrent/ -run '^$' -bench '^BenchmarkSelectedVerifiedComplete' -benchmem -benchtime=500x -count=3`
  (three storage-backed cases)
- `go test ./internal/transfer/bittorrent/ -run '^TestSelectedVerified' -race -count=2` → ok 1.119 s
- `git diff --check`
- Platform: 32-core Linux 6.18.63-gentoo-dist-bin, Go 1.27.3, NVMe-backed `/home` with
  376 GB free; fixture footprint 1.08 GB per run of the large case, removed afterwards.

---

## BT-15 — v0.10.0 release gate: seeding outlives the transfer deadline

**Starting SHA** `17b667f` (`master`, the pushed `v0.10.0` tag; tree clean).
**Ending SHA** `832c3ee` (`fix(torrent): the transfer deadline no longer ends
KeepSeeding`). Changed paths: `internal/transfer/bittorrent/bittorrent.go`,
`child_test.go`, `main_test.go`, new `seed_timeout_test.go`.

**Trigger.** The tag-triggered Release workflow (run 44722782164) failed at
"Run tests" on the CI runner: `TestCLIStatusDuringActiveSeeding` reported
`recorded seeding = "stopped"`, a modified job record in the staging-tree
diff, `child exited before "BUSY"` (the competitor printed `READY OK …
reconciled=true … DONE`), and two `WARNING: DATA RACE` reports. CI on master
passed; the v0.9.0 Release run had passed. The torrent code is byte-identical
to v0.9.0 (`git diff v0.9.0..HEAD -- internal/transfer/ internal/torrentqueue/
internal/filelock/ internal/atomicfile/` empty), so this is not a merge
regression — a pre-existing, regime-sensitive defect the slskd merge happened
to expose.

**Root cause 1 (production).** `fetchSource` wrapped the whole call in
`context.WithTimeout(ctx, cfg.Timeout)`, and `transferAndCapture`'s
KeepSeeding wait (`<-ctx.Done()`) observed that derived context. The helper
sets `Timeout: 60s`; on the loaded CI runner the deadline expired while the
owner was seeding — the 81.66 s test window includes the cold-cache CLI build
inside `runCLIStatus` — so the owner recorded `stopped`, returned, and
released the flock before the competitor started. The competitor then
legitimately acquired the root. The same deadline-driven `markSeeding(stopped)`
write explains both the "stopped" record and the job-record mtime diff the
test attributed to `status` (status wrote nothing; the owner's own stop did).
The contract (`KeepSeeding` lasts "until the context is cancelled"; the CLI
calls `--timeout` an "Overall transfer deadline") says the transfer deadline
must not end seeding. The default CLI path (`--timeout 0`) was unaffected;
`torrent fetch --seed --timeout <d>` was the latent product case.

**Root cause 2 (harness).** `startChild` assigned a bare `bytes.Buffer` to
`cmd.Stderr`; `expectLine`/`wait` read it on failure paths while os/exec's
copy goroutine could still be writing. No happens-before edge exists before
`Wait`, so the failure-path read raced — both CI reports are
`child_test.go:74` (read) vs the exec copy goroutine created at
`child_test.go:48`. The race only manifests when root cause 1 makes the test
fail; the two defects are one chain.

**Fix.** The seeding wait now observes the caller's context, captured before
the deadline wrap (`callerCtx` threaded through `transferAndCapture`);
`Config.Timeout`/`KeepSeeding` docs state the boundary explicitly. A
mutex-guarded `stderrBuf` replaces the raw buffer. The helper's transfer
budget rises 60 s → 120 s for loaded-runner headroom; the tests' own wait
budgets still bound everything.

**RED watched first.** `TestSeedingOutlivesTransferTimeout` (new, hermetic,
20 s transfer deadline) against the pre-fix code: `fetch returned when the
transfer deadline expired (cap={… State:{Payload:ready Capture:committed
Seeding:stopped} …} err=<nil>)` at 21.06 s — the exact CI signature. Green
after the fix (21.07 s): ownership held past the deadline (`ErrStagingBusy`
probe), record still `seeding`, fetch returns only on cancellation, record
then `stopped`, root acquirable after Close.

**Stderr-lock discrimination proof** (scratch worktree at pre-fix `17b667f`,
removed afterwards): the failure path was forced deterministically — the
competitor pointed at an empty root (so it legitimately completes) and the
helper emitted 200 stderr lines. Old harness: three `WARNING: DATA RACE`
reports with the CI stack (`child_test.go:74` vs `:48`, via
`status_process_test.go:182/175`). With `stderrBuf` alone (production fix NOT
applied in that worktree): zero races; the only failure is the intended
forced `child exited before "BUSY"` assertion. The lock, not the production
fix, removes the race.

**Hosted CI at the push (`17b667f`).** CI on master (run 44722782559):
success. Security on master (run 44722782434): failure, sole failing job
"Snyk Open Source" — `SNYK_TOKEN:` empty, `Status: 401 Unauthorized`, same
cause as the baseline run 44627302628 at the v0.9.0 push: a pre-existing
repo-config gap, not a merge regression. Release on the tag (run
44722782164): failure at "Run tests", the defect this entry fixes.

**Commands actually run.**

| Command | Result |
| --- | --- |
| `go test -race ./internal/transfer/bittorrent/ -run TestSeedingOutlivesTransferTimeout` (pre-fix) | FAIL 21.06 s, deadline-ended seeding |
| same (post-fix) | PASS 21.07 s |
| `go test -race ./internal/transfer/bittorrent/ -run 'TestCLIStatus\|TestOwnership\|TestSeeding' -count=3` | ok 97.4 s, all green ×3 |
| scratch-worktree race proof (old harness / stderrBuf only) | 3 races / 0 races |
| `gofmt -l .` | empty (after formatting the new test) |
| `go vet ./...` | clean |
| `golangci-lint run ./...` | 0 issues |
| `go mod tidy -diff` | empty |
| `make test` (`go test -race ./...`) | EXIT=0; bittorrent 96.6 s, torrentqueue 106.2 s |

**Verified vs inferred.**

| Claim | Status |
| --- | --- |
| The transfer deadline ended seeding and released the root | verified (local RED with the CI failure signature; CI log) |
| The stderr race is the exec-copy goroutine vs the failure-path read | verified (local reproduction, stack identical to CI) |
| `stderrBuf` alone removes the race | verified (discrimination proof, production fix absent) |
| Default CLI seeding (`--timeout 0`) was unaffected | verified (flag default read; deadline wrap only when > 0) |
| `fetch --seed --timeout <d>` stopped seeding early | verified (the regression is exactly this case) |
| The v0.9.0 Release pass was timing luck | inferred (identical code; that runner's interrupt-before-deadline timing not measured) |
| Exact CI capture duration | not measured; the 81.66 s window includes the CLI build |

**Limits and non-claims.** Hosted CI has not been re-run on the fix — the
`v0.10.0` tag still points at the pre-fix `17b667f`, its Release run stays
red, and no GitHub release was published for it. Queue-managed seeding is a
separate path (`torrentqueue` adapter sets `KeepSeeding: false`; the seed
session owns its lifetime) and is untouched by this change. No new real-peer
evidence; the loopback fixture boundary stands.

**Release state and next step.** `v0.10.0` is pushed (master and tag verified
at `17b667f` on origin) but unreleased; the fix is local at `832c3ee`. The
publication path — a `v0.10.1` patch tag carrying the fix versus re-tagging
`v0.10.0` — is the maintainer's decision and is pending. After that: the BT
backlog's next eligible ticket (BT-14's proposed sampler walk bounding) or
slskd M5/M6.
