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
