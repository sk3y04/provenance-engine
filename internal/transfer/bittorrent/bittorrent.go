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
	"strconv"
	"strings"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
)

const (
	// Backend is the transfer backend name recorded on jobs and captures.
	Backend = "bittorrent"

	// maxMetainfoBytes caps a local .torrent file we are willing to parse.
	maxMetainfoBytes = 16 << 20

	// pollInterval is how often the adapter samples transfer state.
	pollInterval = 100 * time.Millisecond

	defaultMetadataTimeout = 2 * time.Minute
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
	// BytesRead is useful payload read from peers during this run. A resumed
	// job that reuses verified bytes reads strictly less than the selection
	// size; a fully offline restart reads zero.
	BytesRead int64 `json:"bytes_read"`
	// ManifestPath is the capture manifest written through
	// internal/manifest, relative to StagingDir.
	ManifestPath string `json:"manifest_path"`
}

// Session owns one torrent client. Create one per job, call Close when done.
type Session struct {
	cfg     Config
	client  *at.Client
	peers   []at.PeerInfo
	staging string
}

// New validates cfg, prepares the staging root, and starts a client.
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
	client, err := newClient(abs, cfg)
	if err != nil {
		return nil, err
	}
	return &Session{cfg: cfg, client: client, peers: peers, staging: abs}, nil
}

// StagingDir returns the absolute staging root.
func (s *Session) StagingDir() string { return s.staging }

// Close stops the client. Safe to call once; the caller's deferred Close makes
// an interrupted transfer leave its partial data behind for the next run.
func (s *Session) Close() error {
	if s.client == nil {
		return nil
	}
	errs := s.client.Close()
	s.client = nil
	var err error
	for _, e := range errs {
		err = errors.Join(err, e)
	}
	return err
}

// newClient builds the library client. Limits are conservative; the library
// defaults assume a long-lived desktop client, not one short job.
func newClient(staging string, cfg Config) (*at.Client, error) {
	cc := at.NewDefaultClientConfig()
	// The library derives <DataDir>/<torrent name>/<path> for file storage,
	// so the staging root is the client's data directory. That also keeps the
	// piece-completion database and .part files inside the job's root.
	cc.DataDir = staging
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
type opened struct {
	t           *at.Torrent
	src         source
	info        Info
	files       []*at.File
	byPath      map[string]*at.File
	staging     string
	stagingRoot string
}

// List resolves metadata for a source and returns the file list without
// downloading payload. Run individually, it fetches nothing; a torrent whose
// peers start sending immediately may still leave a piece on disk.
func (s *Session) List(ctx context.Context, rawSource string) (Info, error) {
	src, err := parseSource(rawSource)
	if err != nil {
		return Info{}, err
	}
	metaCtx, cancel := context.WithTimeout(ctx, s.cfg.MetadataTimeout)
	defer cancel()
	o, err := s.open(metaCtx, src, true)
	if err != nil {
		return Info{}, err
	}
	return o.info, nil
}

// Fetch downloads exactly the selected files and returns verified assets.
//
// Selection entries are exact, slash-separated paths as reported by List
// ("<name>/<path>"). An empty selection is rejected: this backend never
// downloads a whole torrent by accident.
func (s *Session) Fetch(ctx context.Context, rawSource string, selection []string) (Capture, error) {
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

	metaCtx, cancelMeta := context.WithTimeout(ctx, s.cfg.MetadataTimeout)
	o, err := s.open(metaCtx, src, false)
	cancelMeta()
	if err != nil {
		return Capture{}, err
	}

	selected, err := o.resolveSelection(selection)
	if err != nil {
		return Capture{}, err
	}
	if err := checkSelectedSize(selected, s.cfg.MaxSelectedBytes); err != nil {
		return Capture{}, err
	}
	if err := s.stagingChecks(o.stagingRoot); err != nil {
		return Capture{}, err
	}
	job, err := s.recordJob(o, selection)
	if err != nil {
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

	cap, err := s.capture(o, selected, job.JobID)
	if err != nil {
		return Capture{}, err
	}
	if s.cfg.KeepSeeding {
		// Opt-in seeding: the caller asked to stay available after the
		// capture, so hold the client open until they stop us.
		<-ctx.Done()
	}
	return cap, nil
}

// open adds the torrent, waits for metadata, persists the exact metainfo once,
// and validates every path in it.
func (s *Session) open(ctx context.Context, src source, listOnly bool) (*opened, error) {
	miPath := s.metainfoPath(src.infoHash)

	var t *at.Torrent
	var err error
	if persisted, perr := loadPersistedMetainfo(miPath, src.infoHash); perr == nil {
		// Reuse the metainfo we saved on a previous run. This is what makes a
		// restart independent of metadata availability, and it means a resume
		// needs no peer at all to start verifying.
		t, err = s.client.AddTorrent(persisted)
		if err != nil {
			return nil, fmt.Errorf("add persisted torrent: %w", err)
		}
	} else {
		switch src.kind {
		case "magnet":
			t, err = s.client.AddMagnet(src.magnet)
			if err != nil {
				return nil, fmt.Errorf("add magnet: %w", err)
			}
		case "file":
			mi, lerr := metainfo.LoadFromFile(src.path)
			if lerr != nil {
				return nil, fmt.Errorf("load %s: %w", filepath.Base(src.path), lerr)
			}
			// Refuse a hostile metainfo before the library opens storage for
			// it: this way no directory or file is created first.
			if info, ierr := mi.UnmarshalInfo(); ierr != nil {
				return nil, fmt.Errorf("parse info of %s: %w", filepath.Base(src.path), ierr)
			} else if _, verr := validateInfoPaths(&info); verr != nil {
				return nil, verr
			}
			t, err = s.client.AddTorrent(mi)
			if err != nil {
				return nil, fmt.Errorf("add torrent: %w", err)
			}
		default:
			return nil, fmt.Errorf("%w: unknown source kind %q", ErrUnsupportedSource, src.kind)
		}
	}
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
	if listOnly {
		for _, f := range o.files {
			f.SetPriority(at.PiecePriorityNone)
		}
	}
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
	for {
		if o.allSelectedComplete(selected) {
			return nil
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
		if _, err := o.finalizedPath(sf.rel); err != nil {
			return false
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
