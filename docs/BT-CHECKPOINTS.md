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