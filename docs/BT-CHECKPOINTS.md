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
