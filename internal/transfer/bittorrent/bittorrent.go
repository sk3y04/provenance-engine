// Package bittorrent is a selective BitTorrent adapter.
//
// It wraps github.com/anacrolix/torrent (MPL-2.0) to add one magnet link or
// one local .torrent file to a private client, list the files in it, download
// only the files the caller named, and hand back their bytes as verified
// capture assets. It implements no peer protocol of its own: piece transfer,
// peer connections and metainfo exchange all belong to the library.
//
// The adapter deliberately does not resolve peers, publish job state, or
// expose a remote endpoint. It has one writer, one staging root per job, and
// no knowledge of the vault; capture manifests are written through
// internal/manifest so the existing verify and archive paths apply unchanged.
//
// Safety model:
//   - Every path component that comes from metainfo is treated as hostile.
//     Unsafe components (traversal, absolute, separators, reserved device
//     names, separators hidden in UTF-8, over-long) reject the whole torrent
//     instead of being sanitised into a different path.
//   - Writes and reads are constrained to the staging root, including
//     symbolic links that already exist inside it.
//   - Metadata arrives before any payload moves: a torrent is added with its
//     storage deferred and both data directions disabled, and payload storage is
//     opened only after the layout, the selection and the staging paths have
//     passed admission. A rejected torrent therefore never creates a payload
//     directory, part file or completed piece. Inspection (see Inspect) goes
//     further and never builds a session, a lock or a staging write at all.
//   - Only selected files whose covering pieces are hash-verified complete are
//     ever captured. Adjacent bytes from a shared edge piece may reach the
//     disk; they are not captured.
//   - Uploads are enabled while fetching (normal BitTorrent reciprocity, and
//     some peers are only willing to send if they can request). Continued
//     seeding after completion is opt-in through Config.KeepSeeding.
package bittorrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
	"github.com/sk3y04/provenance-engine/internal/filelock"
)

const (
	// Backend is the transfer backend name recorded on jobs and captures.
	Backend = "bittorrent"

	// maxMetainfoBytes caps a local .torrent file we are willing to parse.
	maxMetainfoBytes = 16 << 20

	// pollInterval is how often the adapter samples transfer state.
	pollInterval = 100 * time.Millisecond

	defaultMetadataTimeout = 2 * time.Minute

	// finalizeGrace is how long verified pieces may sit unfinished before the
	// adapter reports a finalization failure instead of waiting. Promotion runs
	// synchronously when a piece is marked complete, so this only absorbs the
	// gap between that and the next poll.
	finalizeGrace = 2 * time.Second
)

// Sentinel errors. Callers (and tests) can match them with errors.Is.
var (
	ErrUnsupportedSource = errors.New("unsupported torrent source")
	ErrSelectionRequired = errors.New("explicit file selection is required")
	ErrSelectionNotFound = errors.New("selected file not found in torrent")
	ErrSelectionTooLarge = errors.New("selected files exceed the size limit")
	ErrMetadataTimeout   = errors.New("timed out waiting for torrent metadata")
	ErrTransferTimeout   = errors.New("torrent transfer did not complete")
	ErrNotFinalized      = errors.New("selected file was not finalized on disk")
	ErrSelectionLocked   = errors.New("job selection is immutable")
	// ErrStagedPayloadInvalid means the bytes on disk no longer match what the
	// job verified. The attestation is dropped and the existing re-verify-and-
	// repair path takes over; it is never captured as-is.
	ErrStagedPayloadInvalid = errors.New("staged payload no longer matches the verified job")
	// ErrStorageFailure means the storage backend refused a piece write. The
	// library's default handler logs it and silently stops downloading, so the
	// adapter records the first error and fails the transfer with it instead of
	// leaving the caller to hit a timeout with no cause.
	ErrStorageFailure = errors.New("torrent storage write failed")
	// ErrStagingBusy means another Session owns the staging root. It is
	// retryable: nothing in this job was read or written, and the caller can
	// try again once the owner exits (or releases the root by closing its
	// Session).
	ErrStagingBusy = errors.New("staging root is busy")
)

// Test seams for the two durability boundaries this slice defines. Production
// code leaves them nil; tests set them to simulate a crash by returning an
// error at exactly the point where job state and committed capture can diverge.
var (
	failAfterPayloadReady  func() error
	failAfterCaptureCommit func() error
)

// Config configures a Session. StagingDir is required; everything else has a
// safe default.
type Config struct {
	// StagingDir is the per-job working root. Downloaded files land in
	// StagingDir/<torrent name>/... and all capture paths are relative to it.
	StagingDir string

	// Peers is an optional list of "host:port" peers to dial directly, in
	// addition to whatever trackers or DHT find. Private swarms use this.
	Peers []string

	// MaxSelectedBytes rejects a selection larger than this before any
	// payload is fetched. Zero means no limit.
	MaxSelectedBytes int64

	// MaxPeers bounds established connections per torrent. Zero uses the
	// library default.
	MaxPeers int

	// MetadataTimeout bounds the wait for metainfo from peers. Zero uses
	// two minutes.
	MetadataTimeout time.Duration

	// Timeout bounds the whole fetch, metadata included. Zero means no
	// deadline beyond the caller's context.
	Timeout time.Duration

	// KeepSeeding keeps the client alive after a successful capture until the
	// context is cancelled. Off by default: the slice stops at capture.
	KeepSeeding bool

	// DisablePeerDiscovery disables DHT, PEX, tracker announces and webseeds
	// and binds the listener to loopback. It exists for hermetic tests and
	// offline/private swarms; production fetches leave it false.
	DisablePeerDiscovery bool

	// OnUpdate, when non-nil, receives throttled aggregate progress.
	OnUpdate func(Update)

	// storage, when non-nil, replaces the library's default file storage. It
	// exists so tests can prove that a rejected job never opened payload
	// storage; production leaves it nil.
	storage storage.ClientImpl
}

// Update is one progress sample.
type Update struct {
	Written int64
	Total   int64
}

// File describes one file in a torrent, as listed by metainfo.
type File struct {
	// Path is slash-separated and relative to the torrent root
	// ("<name>/<path>").
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

// Info is metadata-only torrent information.
type Info struct {
	InfoHash    string `json:"info_hash"`
	Name        string `json:"name"`
	TotalLength int64  `json:"total_length"`
	Files       []File `json:"files"`
	// Source is the sanitised source reference (a magnet URI carrying only
	// the v1 infohash, never trackers or passkeys).
	Source string `json:"source"`
}

// Asset is one captured file: a selected, hash-verified, finalized file.
type Asset struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

// Capture is the result of a successful Fetch.
type Capture struct {
	InfoHash   string  `json:"info_hash"`
	JobID      string  `json:"job_id"`
	StagingDir string  `json:"staging_dir"`
	Assets     []Asset `json:"assets"`
	// State reports the three job dimensions at the moment of return:
	// payload readiness, capture completion and seeding.
	State JobState `json:"state"`
	// CaptureKey is the idempotency key: job identity plus the identity of the
	// selected outputs. It is stable across restarts and is what makes a
	// committed capture recognisable.
	CaptureKey string `json:"capture_key"`
	// Reconciled is true when an existing committed capture was adopted instead
	// of writing a new one.
	Reconciled bool `json:"reconciled"`
	// Warnings are non-fatal recovery events, such as a stale payload
	// attestation that forced a re-verification.
	Warnings []string `json:"warnings,omitempty"`
	// BytesRead is useful payload read from peers during this run. A resumed
	// job that reuses verified bytes reads strictly less than the selection
	// size; a capture retry that never opens the swarm reads zero.
	BytesRead int64 `json:"bytes_read"`
	// ManifestPath is the capture manifest written through
	// internal/manifest, relative to StagingDir.
	ManifestPath string `json:"manifest_path"`
}

// Session owns one torrent client and, for its whole lifetime, exclusive
// ownership of one staging root.
//
// Ownership is root-scoped, not torrent-scoped, because the root is what is
// shared: the backend's piece-completion database lives at <root>/.torrent.db
// and is keyed by infohash but written by one library instance per root, two
// torrents can resolve to the same payload directory when their names match,
// and capture run manifests are named by second (<root>/.provenance/runs/
// <ts>.json), so two captures in the same second collide. Per-torrent locking
// would leave all three races open.
//
// Acquire ownership before any job, resume, payload or capture state is read or
// written; the lock is released by Close, or by the OS when the process dies.
type Session struct {
	cfg     Config
	client  *at.Client
	peers   []at.PeerInfo
	staging string

	// runMu serializes List and Fetch on this Session. The backend holds one
	// client and one job's mutable state, so overlapping calls are queued
	// rather than interleaved.
	runMu sync.Mutex

	// ownMu guards the ownership handle, which Close clears. Callers must not
	// run Close concurrently with List or Fetch, but a concurrent Close must
	// never corrupt the handle.
	ownMu sync.Mutex
	owner *filelock.Lock
}

// lockPath is the stable sidecar for this root's ownership. filelock appends
// ".lock"; the path is inside the root so any spelling that resolves to the
// same directory contends on the same inode.
func (s *Session) lockPath() string {
	return filepath.Join(s.staging, filepath.FromSlash(torrentStateDir), "staging")
}

// New validates cfg, takes exclusive ownership of the staging root, and starts
// a client.
//
// A competing Session over the same root fails with ErrStagingBusy before
// anything is read or written: no job record is created, no torrent is added,
// no swarm connection is attempted.
func New(cfg Config) (*Session, error) {
	staging := strings.TrimSpace(cfg.StagingDir)
	if staging == "" {
		return nil, errors.New("bittorrent: a staging directory is required")
	}
	abs, err := filepath.Abs(staging)
	if err != nil {
		return nil, fmt.Errorf("bittorrent: resolve staging dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("bittorrent: create staging dir: %w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("bittorrent: staging dir %s is not a directory", abs)
	}
	cfg.StagingDir = abs
	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = defaultMetadataTimeout
	}
	if cfg.MaxSelectedBytes < 0 {
		return nil, errors.New("bittorrent: max selected bytes must not be negative")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("bittorrent: timeout must not be negative")
	}

	peers, err := parsePeers(cfg.Peers)
	if err != nil {
		return nil, err
	}

	s := &Session{cfg: cfg, peers: peers, staging: abs}
	if err := s.acquireOwnership(); err != nil {
		return nil, err
	}
	client, err := newClient(abs, cfg)
	if err != nil {
		_ = s.releaseOwnership()
		return nil, err
	}
	s.client = client
	return s, nil
}

// acquireOwnership takes the root lock without waiting. Contention is the
// ordinary "another run is active" case and must not block or touch anything.
func (s *Session) acquireOwnership() error {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.owner != nil {
		return nil
	}
	l, err := filelock.TryAcquire(s.lockPath())
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return fmt.Errorf("%w: %s is owned by another provenance process; retry after it exits",
				ErrStagingBusy, s.staging)
		}
		return fmt.Errorf("bittorrent: acquire staging ownership: %w", err)
	}
	s.owner = l
	return nil
}

// releaseOwnership drops the lock exactly once. Release is explicit on success,
// error and cancellation (all of which end at Close); process death releases it
// through the OS, because the lock is an flock/LockFileEx held on an open
// descriptor.
func (s *Session) releaseOwnership() error {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.owner == nil {
		return nil
	}
	err := s.owner.Release()
	s.owner = nil
	return err
}

// checkOpen fails fast when the session has already been closed.
func (s *Session) checkOpen() error {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.owner == nil {
		return errors.New("bittorrent: session is closed")
	}
	return nil
}

// StagingDir returns the absolute staging root.
func (s *Session) StagingDir() string { return s.staging }

// Close stops the client and releases staging ownership. Safe to call more than
// once; the client is stopped before the lock is released so no background
// writer can outlive ownership.
func (s *Session) Close() error {
	var err error
	if s.client != nil {
		errs := s.client.Close()
		s.client = nil
		for _, e := range errs {
			err = errors.Join(err, e)
		}
	}
	return errors.Join(err, s.releaseOwnership())
}

// newClient builds the library client. Limits are conservative; the library
// defaults assume a long-lived desktop client, not one short job.
func newClient(staging string, cfg Config) (*at.Client, error) {
	cc := at.NewDefaultClientConfig()
	// The library derives <DataDir>/<torrent name>/<path> for file storage,
	// so the staging root is the client's data directory. That also keeps the
	// piece-completion database and .part files inside the job's root.
	cc.DataDir = staging
	// Note: ClientConfig has no DeferStorageOpen switch (that lives on
	// AddTorrentOpts, applied per torrent in open). The default file storage
	// this config implies still creates <DataDir> and <DataDir>/.torrent.db
	// eagerly here, at client construction — which is why inspection never
	// builds a client over a staging root.
	if cfg.storage != nil {
		cc.DefaultStorage = cfg.storage
	}
	cc.Seed = cfg.KeepSeeding
	cc.ListenPort = 0
	cc.NoDefaultPortForwarding = true
	cc.DisableWebtorrent = true
	cc.DisableWebseeds = true
	if cfg.MaxPeers > 0 {
		cc.EstablishedConnsPerTorrent = cfg.MaxPeers
		cc.HalfOpenConnsPerTorrent = min(cfg.MaxPeers, 8)
		cc.TotalHalfOpenConns = min(cfg.MaxPeers*4, 64)
		cc.TorrentPeersHighWater = cfg.MaxPeers * 4
		cc.TorrentPeersLowWater = max(cfg.MaxPeers, 1)
	}
	if cfg.DisablePeerDiscovery {
		cc.NoDHT = true
		cc.DisableTrackers = true
		cc.DisablePEX = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.ListenHost = func(string) string { return "127.0.0.1" }
	}
	return at.NewClient(cc)
}

// parsePeers validates explicit peer addresses.
func parsePeers(raw []string) ([]at.PeerInfo, error) {
	out := make([]at.PeerInfo, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		host, port, err := net.SplitHostPort(p)
		if err != nil {
			return nil, fmt.Errorf("bittorrent: peer %q must be host:port: %w", p, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("bittorrent: peer %q has an invalid port", p)
		}
		out = append(out, at.PeerInfo{Addr: peerAddr{host: host, port: n}, Trusted: true})
	}
	return out, nil
}

// peerAddr is the minimal peer address the library asks for.
type peerAddr struct {
	host string
	port int
}

func (p peerAddr) String() string  { return net.JoinHostPort(p.host, strconv.Itoa(p.port)) }
func (p peerAddr) Network() string { return "tcp" }

// source is a parsed torrent source. Raw magnet URIs and local paths stay in
// memory only: they can carry trackers, passkeys, and local paths that must
// not reach a capture manifest or a diagnostic.
type source struct {
	kind     string // "magnet" or "file"
	infoHash metainfo.Hash
	magnet   string
	path     string
}

func (s source) canonical() string {
	return "magnet:?xt=urn:btih:" + s.infoHash.HexString()
}

func parseSource(raw string) (source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return source{}, fmt.Errorf("%w: empty source", ErrUnsupportedSource)
	}
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		m, err := metainfo.ParseMagnetUri(raw)
		if err != nil {
			return source{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
		}
		if m.InfoHash == (metainfo.Hash{}) {
			// BitTorrent v2-only magnets (urn:btmh) are out of scope for this
			// slice: selection and capture are defined over v1 file offsets.
			return source{}, fmt.Errorf("%w: only v1 (urn:btih) magnets are supported", ErrUnsupportedSource)
		}
		return source{kind: "magnet", infoHash: m.InfoHash, magnet: raw}, nil
	}
	fi, err := os.Stat(raw)
	if err != nil {
		return source{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
	}
	if !fi.Mode().IsRegular() {
		return source{}, fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, raw)
	}
	if fi.Size() > maxMetainfoBytes {
		return source{}, fmt.Errorf("%w: metainfo larger than %d bytes", ErrUnsupportedSource, maxMetainfoBytes)
	}
	mi, err := metainfo.LoadFromFile(raw)
	if err != nil {
		return source{}, fmt.Errorf("%w: parse %s: %v", ErrUnsupportedSource, filepath.Base(raw), err)
	}
	if len(mi.InfoBytes) == 0 {
		return source{}, fmt.Errorf("%w: %s has no info dictionary", ErrUnsupportedSource, filepath.Base(raw))
	}
	return source{kind: "file", infoHash: mi.HashInfoBytes(), path: raw}, nil
}

// opened is one torrent with metadata available and paths validated.
// applyAdmissionOpts closes the window between "torrent exists" and "layout has
// been checked": storage is deferred, and no piece data moves in either
// direction, from the moment the torrent is created. The explicit
// Disallow*Data* calls afterwards are belt and braces for a torrent the client
// already knew, where AddTorrentOpts no longer applies.
func applyAdmissionOpts(spec *at.TorrentSpec) {
	spec.DeferStorageOpen = true
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
}

// openPayload opens payload storage. It is called only after the layout and the
// requested selection have passed admission, so a rejected job never creates a
// payload directory, part file or completed piece.
func (o *opened) openPayload(ctx context.Context) error {
	if err := o.t.OpenStorage(ctx, nil); err != nil {
		return fmt.Errorf("open payload storage: %w", err)
	}
	return nil
}

// validateStagingPaths rejects a symlink anywhere on a path this job could
// write through, including files that are not selected: a piece can span files,
// so an unselected neighbour is still a write target.
//
// The check covers what exists at admission time. Two limits are documented
// rather than papered over: the window between this check and the library's
// first write is not atomic (another process able to write inside the staging
// root could swap a component), and the library itself refuses nothing — its
// file backend joins metainfo components directly, and storage.ToSafeFilePath
// is only used by the mmap backend.
func (o *opened) validateStagingPaths() error {
	rels := make([]string, 0, len(o.byPath))
	for rel := range o.byPath {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := checkNoSymlink(o.staging, rel); err != nil {
			return err
		}
	}
	return nil
}

// enableTransfer lifts the admission gate once priorities describe the selected
// files. Uploads stay enabled during the transfer: reciprocity is what keeps
// some peers willing to send.
func (o *opened) enableTransfer() {
	o.t.AllowDataDownload()
	o.t.AllowDataUpload()
}

type opened struct {
	t           *at.Torrent
	src         source
	info        Info
	files       []*at.File
	byPath      map[string]*at.File
	staging     string
	stagingRoot string

	// errMu guards writeErr, which records the first storage write failure the
	// library reports asynchronously. It is read from the completion poll loop
	// and written from a library goroutine.
	errMu    sync.Mutex
	writeErr error
}

// noteStorageError records the first storage write failure. Installing this
// handler replaces the library default, which logs and disables data download
// without telling the caller: a failed write would otherwise look like a
// transfer that never finishes.
func (o *opened) noteStorageError(err error) {
	if err == nil {
		return
	}
	o.errMu.Lock()
	if o.writeErr == nil {
		o.writeErr = err
	}
	o.errMu.Unlock()
}

// storageError returns the recorded write failure, if any.
func (o *opened) storageError() error {
	o.errMu.Lock()
	defer o.errMu.Unlock()
	return o.writeErr
}

// Fetch downloads exactly the selected files and returns verified assets.
//
// Selection entries are exact, slash-separated paths as reported by List
// ("<name>/<path>"). An empty selection is rejected: this backend never
// downloads a whole torrent by accident.
//
// A job is durable across processes. Every call reconciles what already exists
// before doing work, in this order:
//
//  1. a committed capture for this job and selection is adopted, so a restart
//     after a commit never records a duplicate;
//  2. a verified payload is captured straight from staging, without opening the
//     swarm, so a capture failure is retried without fetching again;
//  3. only then is the transfer resumed.
func (s *Session) Fetch(ctx context.Context, rawSource string, selection []string) (Capture, error) {
	// Everything below runs under exclusive staging ownership taken in New, so
	// the job read, the reconciliation decision and the capture commit cannot
	// interleave with another Session over this root. runMu only serializes
	// concurrent Fetch/List calls on this Session.
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Capture{}, err
	}
	src, err := parseSource(rawSource)
	if err != nil {
		return Capture{}, err
	}
	if len(selection) == 0 {
		return Capture{}, ErrSelectionRequired
	}
	if s.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}

	job, err := s.loadOrCreateJob(src, normalizeSelection(selection))
	if err != nil {
		return Capture{}, err
	}

	// 1. Committed already? The key is derived from the recorded payload, so
	// this works even when the previous process died before updating the job.
	//
	// Adoption still re-checks the staged bytes: a capture record that is
	// correct does not mean the working copy on disk still is. A stale payload
	// is reported as a warning, dropped back to unverified, and repaired by the
	// normal resume path (which adopts the same capture again afterwards, so no
	// duplicate is written).
	var warnings []string
	if job.Payload != nil {
		key := captureKey(job.JobID, job.Payload.Assets)
		if path, ok := s.findCommittedCapture(key); ok {
			if verr := s.verifyStagedAssets(job.Payload.Assets); verr == nil {
				cap, cerr := s.adoptCapture(job, key, path, 0, warnings)
				if cerr != nil {
					return Capture{}, cerr
				}
				return s.clearStaleSeeding(job, cap)
			} else {
				warnings = append(warnings, verr.Error())
				if err := s.markPayloadPending(job); err != nil {
					return Capture{}, err
				}
			}
		}
	}

	// 2. Payload verified on an earlier run: capture from staging only.
	if job.State.Payload == PayloadReady && job.Payload != nil && len(job.Payload.Assets) > 0 {
		if verr := s.verifyStagedAssets(job.Payload.Assets); verr == nil {
			cap, cerr := s.captureStaged(job, 0, warnings)
			if cerr != nil {
				return Capture{}, cerr
			}
			return s.clearStaleSeeding(job, cap)
		} else {
			// The attestation is stale. Report it, drop back to unverified and
			// let the normal resume path re-verify and repair.
			warnings = append(warnings, verr.Error())
			if err := s.markPayloadPending(job); err != nil {
				return Capture{}, err
			}
		}
	}

	// 3. Transfer.
	return s.transferAndCapture(ctx, src, job, warnings)
}

// transferAndCapture runs the torrent backend for the job's selection, records
// the verified payload, and commits the capture.
func (s *Session) transferAndCapture(ctx context.Context, src source, job *jobRecord, warnings []string) (Capture, error) {
	metaCtx, cancelMeta := context.WithTimeout(ctx, s.cfg.MetadataTimeout)
	o, err := s.open(metaCtx, src)
	cancelMeta()
	if err != nil {
		return Capture{}, err
	}

	selected, err := o.resolveSelection(job.Selection)
	if err != nil {
		return Capture{}, err
	}
	if err := checkSelectedSize(selected, s.cfg.MaxSelectedBytes); err != nil {
		return Capture{}, err
	}
	if err := s.stagingChecks(o.stagingRoot); err != nil {
		return Capture{}, err
	}
	if job.StagingRef != o.info.Name {
		job.StagingRef = o.info.Name
		if err := s.saveJob(job); err != nil {
			return Capture{}, err
		}
	}

	// Every path this job could write through must be free of symlinks before
	// storage exists: the library's file backend joins metainfo components with
	// filepath.Join and does not sanitise them, so a symlink sitting in staging
	// would redirect a write outside the root.
	if err := o.validateStagingPaths(); err != nil {
		return Capture{}, err
	}
	// Admission is complete: the layout is valid, every requested path exists,
	// the selection is within limits, and staging has no symlink on any path we
	// are about to write through. Only now is payload storage opened.
	if err := o.openPayload(ctx); err != nil {
		return Capture{}, err
	}

	selectedFiles := make(map[*at.File]struct{}, len(selected))
	for _, sf := range selected {
		selectedFiles[sf.file] = struct{}{}
	}
	for _, f := range o.files {
		if _, ok := selectedFiles[f]; ok {
			f.Download()
			continue
		}
		f.SetPriority(at.PiecePriorityNone)
	}
	o.enableTransfer()

	// Verify retained bytes before asking for more: a resumed job must not
	// re-download pieces already on disk, and corrupted retained data must be
	// detected rather than captured.
	if o.hasRetainedData() {
		if err := o.t.VerifyDataContext(ctx); err != nil {
			return Capture{}, fmt.Errorf("verify retained data: %w", err)
		}
	}

	if err := s.waitComplete(ctx, o, selected); err != nil {
		return Capture{}, err
	}

	assets, err := s.hashSelectedAssets(o, selected)
	if err != nil {
		return Capture{}, err
	}
	if err := s.markPayloadReady(job, assets); err != nil {
		return Capture{}, err
	}
	stats := o.t.Stats()
	bytesRead := stats.BytesReadUsefulData.Int64()

	// Durability boundary: verified payload recorded, capture not yet committed.
	if failAfterPayloadReady != nil {
		if herr := failAfterPayloadReady(); herr != nil {
			return Capture{}, fmt.Errorf("payload verified, capture not started: %w", herr)
		}
	}

	cap, err := s.captureStaged(job, bytesRead, warnings)
	if err != nil {
		return Capture{}, err
	}

	if s.cfg.KeepSeeding {
		// Capture is committed and returned below; seeding is a separate state
		// that must not hold the capture back.
		if err := s.markSeeding(job, SeedingActive); err != nil {
			return cap, err
		}
		<-ctx.Done()
		if err := s.markSeeding(job, SeedingStopped); err != nil {
			return cap, err
		}
		// Report the state as it is when the command returns, not as it was
		// when the capture was committed.
		cap.State = job.State
	}
	return s.clearStaleSeeding(job, cap)
}

// clearStaleSeeding records that this run is not seeding. An abruptly killed
// --seed run leaves "seeding" in the record, and a later run that adopts the
// capture without seeding must not persist a claim that is already false when
// it is written. KeepSeeding runs record their own transitions.
func (s *Session) clearStaleSeeding(job *jobRecord, cap Capture) (Capture, error) {
	if s.cfg.KeepSeeding || job.State.Seeding == SeedingStopped {
		cap.State = job.State
		return cap, nil
	}
	if err := s.markSeeding(job, SeedingStopped); err != nil {
		return cap, err
	}
	cap.State = job.State
	return cap, nil
}

// captureStaged commits a capture from the job's verified payload. It never
// touches the torrent client, so a capture retry reuses staged bytes.
func (s *Session) captureStaged(job *jobRecord, bytesRead int64, warnings []string) (Capture, error) {
	assets := job.Payload.Assets
	key := captureKey(job.JobID, assets)
	if path, ok := s.findCommittedCapture(key); ok {
		return s.adoptCapture(job, key, path, bytesRead, warnings)
	}
	manifestPath, err := s.commitCapture(job, assets, key)
	if err != nil {
		return Capture{}, fmt.Errorf("capture: %w", err)
	}
	// Durability boundary: capture committed, job state not yet updated.
	if failAfterCaptureCommit != nil {
		if herr := failAfterCaptureCommit(); herr != nil {
			return Capture{}, fmt.Errorf("capture committed, job state not updated: %w", herr)
		}
	}
	if err := s.markCaptureCommitted(job, key, manifestPath, false); err != nil {
		return Capture{}, err
	}
	return s.captureResult(job, assets, key, manifestPath, bytesRead, warnings, false), nil
}

// open adds the torrent, waits for metadata, persists the exact metainfo once,
// and validates every path in it.
func (s *Session) open(ctx context.Context, src source) (*opened, error) {
	miPath := s.metainfoPath(src.infoHash)

	var t *at.Torrent
	var err error
	if persisted, perr := loadPersistedMetainfo(miPath, src.infoHash); perr == nil {
		// Reuse the metainfo we saved on a previous run. This is what makes a
		// restart independent of metadata availability, and it means a resume
		// needs no peer at all to start verifying.
		spec := at.TorrentSpecFromMetaInfo(persisted)
		applyAdmissionOpts(spec)
		t, _, err = s.client.AddTorrentSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("add persisted torrent: %w", err)
		}
	} else {
		switch src.kind {
		case "magnet":
			spec, serr := at.TorrentSpecFromMagnetUri(src.magnet)
			if serr != nil {
				return nil, errors.New("add magnet: " + sanitizeSourceText(serr.Error(), src.magnet))
			}
			applyAdmissionOpts(spec)
			t, _, err = s.client.AddTorrentSpec(spec)
			if err != nil {
				return nil, errors.New("add magnet: " + sanitizeSourceText(err.Error(), src.magnet))
			}
		case "file":
			mi, lerr := metainfo.LoadFromFile(src.path)
			if lerr != nil {
				return nil, fmt.Errorf("load %s: %w", filepath.Base(src.path), lerr)
			}
			// Refuse a hostile or unsupported metainfo before the library
			// touches it at all: nothing is created first.
			if info, ierr := mi.UnmarshalInfo(); ierr != nil {
				return nil, fmt.Errorf("parse info of %s: %w", filepath.Base(src.path), ierr)
			} else if ierr := requireV1Identity(&info); ierr != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Base(src.path), ierr)
			} else if _, verr := validateInfoPaths(&info); verr != nil {
				return nil, verr
			}
			spec := at.TorrentSpecFromMetaInfo(mi)
			applyAdmissionOpts(spec)
			t, _, err = s.client.AddTorrentSpec(spec)
			if err != nil {
				return nil, fmt.Errorf("add torrent: %w", err)
			}
		default:
			return nil, fmt.Errorf("%w: unknown source kind %q", ErrUnsupportedSource, src.kind)
		}
	}
	// Admission gate. Until the layout and the requested selection have been
	// validated, this torrent may exchange metadata but must not move payload in
	// either direction. Together with DeferStorageOpen this means metainfo
	// arrival cannot create a directory, a part file or a completed piece.
	t.DisallowDataDownload()
	t.DisallowDataUpload()

	// Explicit peers are useful in both branches: a magnet needs them for
	// metadata, and a resumed job needs them for any missing pieces.
	if len(s.peers) > 0 {
		t.AddPeers(s.peers)
	}

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if got := t.InfoHash(); got != src.infoHash {
		return nil, fmt.Errorf("metadata infohash %s does not match source %s", got.HexString(), src.infoHash.HexString())
	}
	if err := s.persistMetainfo(t, miPath, src.infoHash); err != nil {
		return nil, err
	}

	info := t.Info()
	if info == nil {
		return nil, errors.New("torrent reported no info after metadata")
	}
	// Refresh metadata can arrive from a peer as a v2/hybrid dictionary even
	// when the magnet named a v1 infohash; identity is checked before any
	// layout is used.
	if err := requireV1Identity(info); err != nil {
		return nil, err
	}
	name := info.BestName()
	files, byPath, err := validatedFiles(t, info, name)
	if err != nil {
		return nil, err
	}
	stagingRoot, err := joinWithin(s.staging, name)
	if err != nil {
		return nil, err
	}
	list := Info{
		InfoHash:    src.infoHash.HexString(),
		Name:        name,
		TotalLength: info.TotalLength(),
		Files:       make([]File, 0, len(files)),
		Source:      src.canonical(),
	}
	for _, f := range files {
		list.Files = append(list.Files, File{Path: f.rel, Length: f.file.Length(), Offset: f.file.Offset()})
	}
	o := &opened{
		t:           t,
		src:         src,
		info:        list,
		files:       f2(files),
		byPath:      byPath,
		staging:     s.staging,
		stagingRoot: stagingRoot,
	}
	// Chunk writes cannot happen before admission enables transfer, so the
	// handler is installed before any write can be attempted.
	t.SetOnWriteChunkError(o.noteStorageError)
	return o, nil
}

type validatedFile struct {
	rel  string
	file *at.File
}

func f2(in []validatedFile) []*at.File {
	out := make([]*at.File, 0, len(in))
	for _, v := range in {
		out = append(out, v.file)
	}
	return out
}

// validateInfoPaths validates every path in a metainfo info dictionary and
// returns one staging-relative path per file, in metainfo order.
//
// It runs twice: once against the parsed .torrent before the library opens
// storage (so a hostile file is refused before any path is created), and once
// against the metadata the client actually holds, which is the only copy a
// magnet provides.
func validateInfoPaths(info *metainfo.Info) ([]string, error) {
	name := info.BestName()
	if !info.IsDir() {
		if err := safeComponent(name); err != nil {
			return nil, err
		}
		return []string{name}, nil
	}
	files := info.UpvertedFiles()
	rels := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, fi := range files {
		rel, err := relPath(name, fi.Path)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[rel]; dup {
			// Two metainfo entries collapsing onto one path is a collision
			// attack: the second write silently replaces the first.
			return nil, fmt.Errorf("%w: duplicate path %q", ErrUnsafePath, rel)
		}
		seen[rel] = struct{}{}
		rels = append(rels, rel)
	}
	return rels, nil
}

// validatedFiles pairs library file handles with validated relative paths.
// Every file in the torrent is validated, not only the selected ones: a piece
// can span files, so an unsafe neighbour is still a path we would write to.
func validatedFiles(t *at.Torrent, info *metainfo.Info, name string) ([]validatedFile, map[string]*at.File, error) {
	rels, err := validateInfoPaths(info)
	if err != nil {
		return nil, nil, err
	}
	files := t.Files()
	if len(files) != len(rels) {
		return nil, nil, fmt.Errorf("torrent file count mismatch: %d handles, %d metainfo files", len(files), len(rels))
	}
	byPath := make(map[string]*at.File, len(files))
	out := make([]validatedFile, 0, len(files))
	for i, rel := range rels {
		byPath[rel] = files[i]
		out = append(out, validatedFile{rel: rel, file: files[i]})
	}
	return out, byPath, nil
}

// resolveSelection maps requested paths onto torrent files. Selection is exact
// and case-sensitive; a mistyped name is an error, never a silent whole-torrent
// download.
func (o *opened) resolveSelection(selection []string) ([]validatedFile, error) {
	seen := make(map[string]struct{}, len(selection))
	out := make([]validatedFile, 0, len(selection))
	for _, raw := range selection {
		want := strings.TrimPrefix(strings.TrimSpace(raw), "./")
		if want == "" {
			return nil, fmt.Errorf("%w: empty selection entry", ErrSelectionNotFound)
		}
		if _, dup := seen[want]; dup {
			continue
		}
		f, ok := o.byPath[want]
		if !ok {
			return nil, fmt.Errorf("%w: %q (run list to see available paths)", ErrSelectionNotFound, want)
		}
		seen[want] = struct{}{}
		out = append(out, validatedFile{rel: want, file: f})
	}
	return out, nil
}

// checkSelectedSize rejects an oversized selection before any payload moves.
func checkSelectedSize(selected []validatedFile, limit int64) error {
	if limit <= 0 {
		return nil
	}
	var total int64
	for _, sf := range selected {
		total += sf.file.Length()
		if total > limit {
			return fmt.Errorf("%w: selection needs at least %d bytes, limit is %d", ErrSelectionTooLarge, total, limit)
		}
	}
	return nil
}

// stagingChecks rejects a staging root that already contains a symlink on the
// path we are about to write through.
func (s *Session) stagingChecks(stagingRoot string) error {
	rel, err := filepath.Rel(s.staging, stagingRoot)
	if err != nil {
		return fmt.Errorf("staging root: %w", err)
	}
	return checkNoSymlink(s.staging, filepath.ToSlash(rel))
}

// hasRetainedData reports whether anything from a previous attempt is on disk.
// Only then is a full piece re-hash worth its cost.
func (o *opened) hasRetainedData() bool {
	found := false
	_ = filepath.WalkDir(o.stagingRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(p, ".part"):
			found = true
		case d.Type().IsRegular():
			found = true
		}
		return nil
	})
	return found
}

// waitComplete blocks until every selected file is hash-verified complete and
// finalized under its final name.
func (s *Session) waitComplete(ctx context.Context, o *opened, selected []validatedFile) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var settledSince time.Time
	for {
		if err := o.storageError(); err != nil {
			o.t.DisallowDataDownload()
			return fmt.Errorf("%w: %v", ErrStorageFailure, err)
		}
		if o.allSelectedComplete(selected) {
			return nil
		}
		if o.selectedPiecesSettled(selected) {
			// Verified pieces that never reach their final names mean the
			// storage layer failed to promote a part file. The library only logs
			// that, so report it rather than wait for the transfer deadline.
			if settledSince.IsZero() {
				settledSince = time.Now()
			} else if time.Since(settledSince) > finalizeGrace {
				sf := selected[0]
				_, ferr := o.finalizedPath(sf.rel)
				return fmt.Errorf("%w: %s: pieces are complete but the file was never finalized: %v",
					ErrNotFinalized, sf.rel, ferr)
			}
		} else {
			settledSince = time.Time{}
		}
		s.report(o, selected)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrTransferTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// allSelectedComplete requires, for each selected file, that every covering
// piece is complete and idle (not hashing or being marked), that the file is
// byte-complete, and that its final name exists on disk.
//
// Pieces, not bytes, are the unit of verification: BytesCompleted can reach
// the file length before the covering piece has been hashed.
func (o *opened) allSelectedComplete(selected []validatedFile) bool {
	if !o.selectedPiecesSettled(selected) {
		return false
	}
	for _, sf := range selected {
		if _, err := o.finalizedPath(sf.rel); err != nil {
			return false
		}
	}
	return true
}

// selectedPiecesSettled reports whether every piece covering a selected file is
// complete, hashed and not being re-marked. It deliberately says nothing about
// finalization: a storage backend can hold verified pieces in part files it
// never managed to promote, and that needs a different diagnosis than "still
// downloading".
func (o *opened) selectedPiecesSettled(selected []validatedFile) bool {
	for _, sf := range selected {
		if sf.file.BytesCompleted() != sf.file.Length() {
			return false
		}
		for i := sf.file.BeginPieceIndex(); i < sf.file.EndPieceIndex(); i++ {
			st := o.t.Piece(i).State()
			if !st.Complete || st.Marking || st.Hashing || st.QueuedForHash {
				return false
			}
		}
	}
	return true
}

// finalizedPath returns the on-disk path of a completed file, rejecting a
// leftover .part or a size that disagrees with metainfo. Paths are relative to
// the staging root, so the check is independent of the torrent's own directory.
func (o *opened) finalizedPath(rel string) (string, error) {
	full, err := joinWithin(o.staging, rel)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(full + ".part"); err == nil {
		return "", fmt.Errorf("%w: %s is still a part file", ErrNotFinalized, rel)
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrNotFinalized, rel, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrNotFinalized, rel)
	}
	return full, nil
}

// report emits one throttled progress sample.
func (s *Session) report(o *opened, selected []validatedFile) {
	if s.cfg.OnUpdate == nil {
		return
	}
	var written, total int64
	for _, sf := range selected {
		written += sf.file.BytesCompleted()
		total += sf.file.Length()
	}
	s.cfg.OnUpdate(Update{Written: written, Total: total})
}

// persistMetainfo writes the exact metainfo bytes the client holds. Re-reading
// it on the next run is what lets a resume start without a peer.
func (s *Session) persistMetainfo(t *at.Torrent, path string, want metainfo.Hash) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	mi := t.Metainfo()
	if got := mi.HashInfoBytes(); got != want {
		return fmt.Errorf("library metainfo hashes to %s, expected %s", got.HexString(), want.HexString())
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return fmt.Errorf("encode metainfo: %w", err)
	}
	if err := atomicfile.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("persist metainfo: %w", err)
	}
	return nil
}

// loadPersistedMetainfo loads a previously persisted metainfo and checks that
// it still describes the requested torrent.
func loadPersistedMetainfo(path string, want metainfo.Hash) (*metainfo.MetaInfo, error) {
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	if len(mi.InfoBytes) == 0 {
		return nil, errors.New("persisted metainfo has no info dictionary")
	}
	if got := mi.HashInfoBytes(); got != want {
		return nil, fmt.Errorf("persisted metainfo hashes to %s, expected %s", got.HexString(), want.HexString())
	}
	return mi, nil
}
