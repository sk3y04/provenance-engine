# Findings: live `slskd` 0.26.x contract

Date: 2026-11-20
Branch: `feat/slskd-integration` (from `61f1cb3`)
Host Docker: `Docker version 29.8.1`
Images: `slskd/slskd:0.26.0` (digest `sha256:ecd4026d4f8fb504e2cc55323efa2c1f5b56d20d3686b018249cc36b48ea17a6`),
`slskd/slskd:0.26.2`

Everything below was run against the real binaries. Where a claim is *inferred* rather than observed
it is marked. Nothing here is copied from upstream documentation without an observation behind it —
in two places the upstream documentation is demonstrably wrong.

## 1. Harness

```
docker run -d --name slskd026 -p 127.0.0.1:15030:5030 \
  -v "$PWD/slskd.yml:/app/slskd.yml:ro" \
  -v "$PWD/downloads:/downloads" -v "$PWD/incomplete:/incomplete" \
  -v "$PWD/shares:/shared:ro" slskd/slskd:0.26.0
```

`slskd.yml` used for the spike (the API keys below are throwaway, non-secret values):

```yaml
web:
  authentication:
    api_keys:
      - key: spikeadmin0123456789abcdefghijkl   # administrator
        role: administrator
        cidr: 0.0.0.0/0,::/0
      - key: spikewriter0123456789abcdefghijk   # readwrite
        role: readwrite
        cidr: 0.0.0.0/0,::/0
feature:
  swagger: true
shares:
  directories:
    - "[Provenance]/shared"
  cache:
    storage_mode: disk
```

`feature.swagger: true` served `/swagger/v0/swagger.json` with `200` (260 381 bytes) and
`/swagger/v0/swagger.yaml` with `200` (191 855 bytes). Both files are checked in under
`internal/slskd/testdata/`.

## 2. `searchTimeout` is milliseconds (the code comment and Swagger are wrong)

`SearchRequest.cs` at tag `0.26.0` says:

```
/// Gets or sets the search timeout value, in seconds, used to determine when the search is complete. (Default = 15).
[Range(5, int.MaxValue)]
public int? SearchTimeout { get; set; }
```

The Swagger schema repeats "in seconds", `minimum: 5`. The running binary disagrees:

```
POST /api/v0/searches {"searchText":"provzqxjkvb nonsense","searchTimeout":5,...}
  -> 200 {"state":"InProgress","startedAt":"2026-11-19T23:10:28.7960028Z"}
GET  /api/v0/searches/{id}  (polled at 1 s intervals)
  -> {"state":"Completed, TimedOut","endedAt":"2026-11-19T23:10:28.8491383Z","isComplete":true}
```

The whole search lasted **53 ms** for `searchTimeout: 5`. In seconds that request would have been a
no-op timeout; in milliseconds it is exactly what happened. A second search with
`searchTimeout: 3000` ran for ~800 ms once the response limit was reached
(`"state":"Completed, FileLimitReached"`, `responseCount: 2`, `fileCount: 133`), consistent with a
3 s cap that never had to fire.

Decision: `internal/slskd` sends **milliseconds** and names the field `SearchTimeoutMS`.

## 3. Search request/response

```
POST /api/v0/searches
{"id":"4444...","searchText":"beethoven symphony flac","searchTimeout":3000,
 "responseLimit":3,"fileLimit":50,"filterResponses":true}
-> 200, body is the search document (fileCount, id, isComplete, lockedFileCount,
   responseCount, responses[], searchText, startedAt, state, token)
```

`GET /api/v0/searches/{id}?includeResponses=true` → `responses[]`, each:

```
{"fileCount":0,"files":[],"hasFreeUploadSlot":true,"lockedFileCount":1,
 "lockedFiles":[{"bitDepth":16,"code":1,"extension":"","filename":"@@jwuyr\\mp3\\...\\x.flac",
                 "length":320,"sampleRate":44100,"size":34429672,"isLocked":false}],
 "queueLength":0,"token":23,"uploadSpeed":1290178,"username":"superphunk2"}
```

Notes that matter for the coordinator:

- `filterResponses: true` moves files a peer has locked into `lockedFiles` and leaves `files` empty
  for that peer. Grouping must consider both arrays, and locked-only peers must be excluded by
  default (they cannot be downloaded without a grant).
- `extension` came back as `""` on every observed file; the real extension is in `filename`. Any
  format filter must parse the filename, not the field.
- `length` is the media duration in seconds; `size` is bytes.

## 4. Transfer states are flags strings, comma + space

Observed in the search document: `"InProgress"`, `"Completed, TimedOut"`,
`"Completed, FileLimitReached"`. The transfer endpoint uses the same `System.Text.Json` flags
serialisation, so a failed download is expected to read `"Completed, Errored"` / `"Completed,
Rejected"` / `"Completed, Aborted"` / `"Completed, TimedOut"` / `"Cancelled"`.
**Inferred, not observed**: a real peer download was not performed in the spike, and a transfer to
an offline peer is never created (see §5). The implementation therefore parses states as a *set*
(split on `,`, trim, case-insensitive) instead of comparing literal strings, which is correct for
any spacing the serialiser chooses.

## 5. Batch enqueue

```
POST /api/v0/transfers/downloads/batches
{"id":"<uuid>","username":"nonexistent_user_zzz","files":[{"filename":"music\\x.mp3","size":100}],
 "options":{"externalId":"prov-test1","destination":"provenance/prov-test1"}}
-> 404 "User nonexistent_user_zzz appears to be offline"

... username "spikeuser" (self, listen port unreachable from inside the container)
-> 500 "Failed to connect to user spikeuser: Failed to establish a direct or indirect message
   connection to spikeuser (83.22.254.115:50300)"
```

Both attempts left `GET /api/v0/transfers/downloads?includeRemoved=true` returning `[]`: a batch that
never reaches a peer produces no transfer record. So an enqueue failure must be classified from the
HTTP status + body, not from a later transfer poll. Status map from the Swagger, confirmed for `404`
and `500`: `201` all accepted, `207` partial, `200` all failed, `400` bad request, `403` forbidden,
`409` duplicate batch id, `429` throttled.

## 6. Shares, and a share-scanner escape

Fixture inside the `:ro` mounted share root:

```
shares/Album/track1.flac             2048 bytes, link count 2 (hardlinked)
shares/Album-hardlink.flac           hardlink of track1.flac
shares/Album/cover.jpg               1024 bytes
shares/Album/link.flac -> track1.flac   (symlink, internal)
shares/Album/track2link.flac         512 bytes
```

Startup scan: `Found 2 shared directories (and 0 were excluded)`, `Scan found 5 files`.

```
GET /api/v0/shares
{"local":[{"id":"DD35A816...","alias":"Provenance","isExcluded":false,
           "localPath":"/shared","raw":"[Provenance]/shared",
           "remotePath":"Provenance","directories":1,"files":5}]}
```

- The map key is **`local`**, not the alias.
- Hardlinks are indexed (`Album-hardlink.flac`, `track1.flac`), which is what makes the curated
  hardlink share design viable.
- Symlinks **inside** the root are indexed as ordinary files (`link.flac`, 2048 bytes).

Then two escaping symlinks were added to the same `:ro` root:

```
shares/evil.txt   -> /etc/hostname     (absolute)
shares/shadow.txt -> ../../../../etc/shadow
```

`PUT /api/v0/shares` → `200`, log: `Scan found 5 files` → `Shares scanned successfully. Found 2
directories and **7** files`, and:

```
GET /api/v0/shares/contents
Provenance\Album  ['cover.jpg','link.flac','track1.flac','track2link.flac']
Provenance        ['Album-hardlink.flac','evil.txt','shadow.txt']
```

**`slskd` advertises files outside the share root when a symlink points at them, and the `:ro` mount
does not prevent it.** Symlinks were removed immediately after the observation. This is why
`internal/slskd/shares.go` materialises hardlinks only, refuses any symlinked source path, and
re-audits the materialised tree with `Lstat` before telling `slskd` to rescan.

## 7. Health, options and `#1905`

```
0.26.0 GET /api/v0/options  (administrator) -> 200
0.26.0 GET /api/v0/options  (readwrite)     -> 200
0.26.2 GET /api/v0/options  (administrator) -> 200
0.26.2 GET /api/v0/options  (readwrite)     -> 403
```

`#1905` reproduced exactly: a `403` on `/options` is a version/role artefact, never a connection
failure.

`GET /api/v0/server` (0.26.0) returns the flattened client state, not the Swagger's `ServerState`:

```
{"state":"None","isConnected":false,"isConnecting":false,"isLoggedIn":false,
 "isLoggingIn":false,"isTransitioning":false}
```

`GET /api/v0/application` wraps it:

```
{"version":{"full":"0.26.0.0 (0.26.0.0+e42a525d)","current":"0.26.0.0","latest":"0.26.2",
            "isUpdateAvailable":true,"isCanary":false,"isDevelopment":false},
 "pendingReconnect":false,"pendingRestart":false,
 "server":{...},"connectionWatchdog":{...}}
```

`GET /api/v0/application/version` returns a bare JSON string: `"0.26.0.0"`. Version comparison must
parse a dotted quad with a possible suffix, not a semver triple.

`GET /api/v0/options` `shares` block as observed:

```
{"directories":["[Provenance]/shared"],"filters":[],"cache":{"storageMode":"disk","workers":32}}
```

and `flags` uses camelCase keys (`noShareScan`, `volatile`, `forceShareScan`, …) — JSON casing is
camelCase throughout, so every request/response struct in the Go client needs explicit tags.

`GET /api/v0/files/downloads/directories?recursive=false`:

```
{"name":"downloads","fullName":"","attributes":"Directory",
 "createdAt":"...","modifiedAt":"...","files":[],"directories":[]}
```

File/directory delete endpoints take a **base64** relative name in the path
(`/api/v0/files/downloads/files/{base64FileName}`) and require `remote_file_management: true`.

`GET /api/v0/transfers/uploads` returned `[]` (no peers were served during the spike).

## 8. There is no SSE

`GET /api/v0/events` is documented as "Retrieves a paginated list of past event records" with
`offset`/`limit` parameters and returns `[{timestamp,type,data,id}]`. There is no streaming endpoint
in either Swagger. `POST /api/v0/events/{type}` raises a sample event and returns `201` with
`{id,timestamp,type,version}` — that is the webhook self-test path.
`EventType` enum: `None, Any, DownloadFileComplete, DownloadDirectoryComplete, UploadFileComplete,
PrivateMessageReceived, RoomMessageReceived, SoulseekClientConnected,
SoulseekClientDisconnected, Noop` (`UploadDirectoryComplete` is absent, confirming it is disabled
upstream).

## 9. Version delta 0.26.0 → 0.26.2 (Swagger diff)

Added paths: `/api/v0/logs/files`, `/api/v0/logs/files/{filename}`, `/api/v0/logs/live`.
Removed paths: `/api/v0/application/dump`, `/api/v0/application/gc`,
`/api/v0/application/loopback`, `/api/v0/logs`.
Changed schemas: `Options+FlagsOptions` (adds `dumpable`), `Options+LoggerOptions`,
`Options+RetentionOptions`, `Options+SoulseekOptions+ConnectionOptions`.
None of these touch the surfaces this integration uses; the client is version-tolerant by design
(only `/application*`, `/server`, `/options`, `/searches*`, `/transfers*`, `/files*`, `/shares*`,
`/users*`, `/events*`).

## 10. What this spike did **not** prove

- No real peer-to-peer download was completed, so `FileService.MoveFile`'s resulting permissions
  (`permissions.mode: 755` vs `775`) and the `_{ticks}` rename-collision suffix were not observed on
  disk. Both are handled defensively in `ingest.go` and covered by hermetic tests.
- `#1891`'s 180 s enqueue crash (unhandled `ObjectDisposedException`, exit 134) was not reproduced;
  the client treats any transport error during enqueue as `SLSKD_UNREACHABLE` and never blocks a
  caller for longer than the configured enqueue timeout.
- `#1818` (255-byte path segment loop), `#1819` (`search.db` concurrency), `#1609` (duplicate
  `Queued, Locally` uploads) and `#1663` (`no_share_scan`) were not reproduced. Their handling in
  code is defensive and unit-tested against the described symptom, not against the live defect.
- `SLSKD_SHARED_DIR` vs YAML `shares.directories` array merging was not tested; the Compose template
  in M6 simply never sets the env var.