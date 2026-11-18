# Feasibility spike: bare v2-only btmh magnet → verified, restartable transfer

Scope: investigation only. No production code, `go.mod`, or admission guard changed.
Date: 2026-11-18. Platform: linux/amd64, go1.27.3.

## Verdict

**GO** — with conditions, and **without** a dependency upgrade. The pinned
`github.com/anacrolix/torrent v1.62.0` already acquires, validates and persists
BEP 52 piece hashes, so a v2-only magnet can become a verified transfer. The
current "no piece layers" refusal is a Provenance admission rule, not a library
limitation.

Two earlier claims in this branch's docs were wrong and are corrected below.

## Upstream audit

| capability | v1.62.0 | where |
|---|---|---|
| BEP 9 info acquisition | present, exercised | `torrent.go` `setInfoBytesLocked`, `GotInfo`, `metadataBytes` |
| BEP 52 hash request | present, exercised | `peerconn.go` `shouldRequestHashes`, `requestMissingHashes`, called from `fillWriteBuffer` once the peer is usable |
| BEP 52 hash receive + validate | present, exercised | `peerconn.go` `onReadHashes`: rejects unsolicited replies and `ProofLayers != 0`, recomputes `merkle.RootWithPadHash(filePieceHashes, metainfo.HashForPiecePad(pieceLen))` and adopts hashes only if it equals the file's `pieces root` (`piece.setV2Hash`) |
| hash serving | present | `peerconn.go` `onHashRequest` |
| restart-grade persistence | present, exercised | `t.go` `Metainfo()` → `torrent.go` `newMetaInfo()` → `pieceLayers()`, which rebuilds the top-level `piece layers` map from `pieces[i].hashV2` — i.e. **from hashes acquired over the wire**, not only from the original `.torrent` |

`requestMissingHashes` runs continuously from `fillWriteBuffer`, so acquisition
starts after `GotInfo` (it needs `pc.t.haveInfo() && pc.v2 && info.HasV2()`) and
does not need storage to be open. It skips files with a single piece (their piece
hash *is* the file's pieces root) and needs the peer to advertise the piece.

Newer candidate: **v1.63.0** (the only tag after the pin). `onHashRequest`,
`shouldRequestHashes` and `pieceLayers` are byte-identical to v1.62.0.
`requestMissingHashes` now iterates `*t.files` instead of `info.UpvertedFiles()`,
and `onReadHashes` tracks per-piece receipt (`receivedFileHashes`) and bounds
`msg.Index` earlier. Both are hardening, not capability. No upgrade required.

## Spike result (isolated module, `/tmp/v2magnet-spike`, v1.62.0, Loopback)

Fixture: one 200000-byte file, 32 KiB pieces (7 pieces), v2-only metainfo, piece
layers present in the seeder's `.torrent` only.
`v1=e8476b94…`, `v2=9c5ea9d2…`, `piecesRoot=2913ff76…`.

Receiver: empty directory, **only** `InfoHashV2` and one loopback peer address.
No `.torrent`, no `xs`/`as`, no v1 hash, no DHT/PEX/trackers.

```
stage 1 info dictionary:           20 ms   (BEP 9)
stage 2 piece hashes:              20 ms   layers=1, validated against pieces root
  payload bytes before hashes:         0
stage 3 payload:                  1.03 s   200000 bytes read, complete
stage 4 file digest:              eba28d6f… matches original
persisted metainfo:               layers=1, info bytes byte-identical to source
restart, zero peers, VerifyData:  err=nil, complete=true, missing=0
```

Interrupt/resume (seeder upload capped at 100 KB/s so the cut lands mid-transfer):

```
phase B: interrupted after 81920 bytes, layers=1
phase B: layers known before any peer connected = 1
         completed=true, extra bytes read=167232, digest ok=true
```

Resume reused one piece of already-verified data (200000−167232 = 32768) after an
abrupt `Close()`; with a flushed completion DB the retained set would be larger.
Restart needed no info dictionary and no hash requests: the saved metainfo
carried the layers, so every piece already had its `hashV2`.

Negative control, hash data withheld (persisted metainfo with `piece layers`
stripped, payload file truncated to 70000 bytes, **no peers**):

```
layers=0  VerifyData err="verifying piece 0: piece hash unknown"
complete=false  missing=200000  bytesRead=0  digestMatchesOriginal=false
```

Nothing was verified, nothing was read, and no unverified bytes were adopted.

## What the spike proves, and what it does not

- Proven: with v1.62.0, a bare `btmh` identity plus one v2-capable peer yields a
  verified transfer, and the hashes it acquires survive into a saved `.torrent`
  that a fresh process verifies with zero peers.
- Not proven: interoperability with non-anacrolix peers (both ends here are the
  same implementation), the same flow over a real swarm, or padded-hybrid
  behaviour (the foreign torrenttools fixture is untouched by this spike).
- Not attempted: any change to Provenance, the pin, or the admission guard.

## Corrections to earlier notes in this branch

1. "The pinned library never obtains piece layers over the wire" — **wrong**.
   v1.62.0 implements request/receive/validate; BEP 52 hash messages are in the
   pinned version. What is true: BEP 9 metadata exchange carries only the `info`
   dictionary, and *Provenance's admission guard* refuses a v2-only torrent whose
   metainfo has no layers before any peer is contacted.
2. "`Torrent.Metainfo()` will not serialise hashes learned at runtime" — **wrong**.
   `pieceLayers()` builds the top-level map from each piece's `hashV2`, which
   `onReadHashes` sets, so the saved metainfo is restart-grade.

## If this is implemented later (not in this PR)

1. Replace the v2-only missing-layer refusal with a *deferred* requirement: admit
   on layout + selection, keep `DeferStorageOpen` and the payload gates, and
   require layers only where they are actually needed (a restart that cannot
   reach a peer, or an offline verification path).
2. Bound hash acquisition with its own timeout, distinct from metadata timeout,
   and fail with a named error when no peer supplies hashes.
3. Persist metainfo (which now includes layers) only after the selected files
   verify; a metainfo saved mid-transfer may carry a partial layer set.
4. Keep the existing rule that `GotInfo` and `BytesCompleted` are not proof of
   verification; verification remains the hash the backend reports per piece.
5. Re-run the full torrent suite plus `govulncheck`; the pin stays at v1.62.0
   unless v1.63.0's hardening is separately justified.