package bittorrent

// Workspace-scoped seed session (BT-08d.1).
//
// One job workspace, opened from its persisted metainfo, verified locally and then
// served. There is no metadata exchange, no download and no capture work: this is
// not the fetch path, and it is not the read-only recheck either - verifying local
// data updates the backend's piece completion state, which is exactly what makes
// serving honest and what BT-06's recheck refuses to do.
//
// Scope of this version: the workspace must hold a published capture whose manifest
// is present, and its selection must cover every file entry in the torrent. A
// selective workspace is refused rather than served, because a piece on the boundary
// between a selected and an unselected file would hand out bytes that were never
// attested for this job.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var (
	// ErrSeedIneligible means the workspace cannot be served at all: no job record,
	// no published capture, or a missing manifest.
	ErrSeedIneligible = errors.New("workspace is not eligible for seeding")
	// ErrSeedSelective means the workspace holds only part of the torrent.
	ErrSeedSelective = errors.New("selective workspace cannot be seeded")
	// ErrSeedDataIncomplete means local verification did not cover the torrent.
	// Nothing is repaired, fetched or recaptured.
	ErrSeedDataIncomplete = errors.New("local data is incomplete; serving refused")
)

// SeedInfo describes what was opened, once verification has passed.
type SeedInfo struct {
	JobID   string
	Files   int
	Bytes   int64
	Address string
	// VerifiedPieces and TotalPieces are reported so a caller can see what was
	// actually hashed rather than trusting a status flag.
	VerifiedPieces int
	TotalPieces    int
	// VerifyDuration is how long the local re-hash took.
	VerifyDuration time.Duration
}

// SeedSession is one workspace being served. The workspace lock is held from open
// until Close, across verification, handoff and serving - there is no unlock and
// reopen in between.
//
// Startup is deliberately two-phase. Verification runs in a client that cannot
// accept a connection (no listener, no DHT, no trackers, no PEX, no dialing) against
// a fresh in-memory piece completion, so it hashes every piece from the workspace it
// opened and cannot inherit a stale completed flag from the job's persisted
// completion database. Serving then runs in a second client that is handed that very
// completion state, so what it offers is what was just verified rather than what some
// earlier run recorded.
type SeedSession struct {
	// mu guards the serving client: Serve installs it from its own goroutine while a
	// caller may be asking for the address or the served-byte counter.
	mu     sync.Mutex
	own    *Session
	id     Identity
	info   *metainfo.Info
	mi     *metainfo.MetaInfo
	pieces storage.PieceCompletion
	client *at.Client
	cstore storage.ClientImplCloser
	tor    *at.Torrent
	report *SeedInfo
}

// OpenSeedSession verifies and opens one workspace for serving. cfg.StagingDir is
// the job workspace itself (the directory holding .provenance/torrent).
//
// The context bounds verification: cancellation returns the context error and
// releases the workspace, so a slow or damaged workspace can never hold a caller
// longer than it is willing to wait.
func OpenSeedSession(ctx context.Context, cfg Config, jobID string) (*SeedSession, error) {
	id, err := parseIdentityString(jobID)
	if err != nil {
		return nil, err
	}
	mode, err := ParseDiscovery(string(cfg.Discovery))
	if err != nil {
		return nil, err
	}
	cfg.Discovery = mode
	cfg.KeepSeeding = true

	ws, err := filepath.Abs(strings.TrimSpace(cfg.StagingDir))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	if fi, serr := os.Stat(ws); serr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: workspace %s does not exist", ErrSeedIneligible, ws)
	}
	job, err := readJob(filepath.Join(ws, filepath.FromSlash(torrentStateDir), id.JobID()+".job.json"))
	if err != nil {
		return nil, fmt.Errorf("%w: no job record for %s", ErrSeedIneligible, id.JobID())
	}
	if job.Capture == nil || job.Capture.Key == "" {
		return nil, fmt.Errorf("%w: no published capture for %s", ErrSeedIneligible, id.JobID())
	}
	manifest, jerr := joinWithin(ws, job.Capture.Manifest)
	if jerr != nil {
		return nil, fmt.Errorf("%w: capture manifest path is unsafe", ErrSeedIneligible)
	}
	if fi, serr := os.Stat(manifest); serr != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: capture manifest %s is missing", ErrSeedIneligible, job.Capture.Manifest)
	}
	// The metainfo path is already workspace-relative.
	mi, err := loadPersistedMetainfo(filepath.Join(ws, filepath.FromSlash(metainfoRelFor(id))), id)
	if err != nil {
		return nil, fmt.Errorf("%w: persisted metainfo: %v", ErrSeedIneligible, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, fmt.Errorf("%w: metainfo: %v", ErrSeedIneligible, err)
	}
	all := metadataPaths(&info)
	selected := append([]string(nil), job.Selection...)
	if !samePathSet(all, selected) {
		return nil, fmt.Errorf("%w: %d of %d files selected; a boundary piece would expose bytes that were never attested",
			ErrSeedSelective, len(selected), len(all))
	}

	own, err := openWorkspaceBare(ws, id, cfg)
	if err != nil {
		return nil, err
	}
	s := &SeedSession{own: own, id: id, info: &info, mi: mi}

	// A fresh, in-memory completion: verification must hash the workspace, not trust
	// the job's persisted completion database. This state is then handed to the
	// serving client, so no stale flag can reach it either.
	s.pieces = storage.NewMapPieceCompletion()
	if err := s.verify(ctx, ws, cfg); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.report = &SeedInfo{
		JobID:          id.JobID(),
		Files:          len(all),
		Bytes:          info.TotalLength(),
		Address:        "",
		VerifiedPieces: s.report.VerifiedPieces,
		TotalPieces:    s.report.TotalPieces,
		VerifyDuration: s.report.VerifyDuration,
	}
	return s, nil
}

// verify hashes the workspace in a client that cannot reach a peer in either
// direction, and records how many pieces it actually checked.
func (s *SeedSession) verify(ctx context.Context, ws string, cfg Config) error {
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   ws,
		PieceCompletion: s.pieces,
	})
	cc := clientConfig(ws, cfg)
	// Network-inert: nothing listens, nothing dials, nothing gossips.
	cc.AcceptPeerConnections = false
	cc.NoDHT = true
	cc.DisableTrackers = true
	cc.DisablePEX = true
	cc.DisableUTP = true
	cc.DisableTCP = true
	cc.DisableIPv6 = true
	cc.DisableWebseeds = true
	cc.NoDefaultPortForwarding = true
	cc.Seed = false
	cc.ListenPort = 0
	cc.DefaultStorage = st
	client, err := at.NewClient(cc)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("start verification client: %w", err)
	}
	defer func() {
		client.Close()
		_ = st.Close()
	}()
	if clientOpenHook != nil {
		clientOpenHook(client)
	}
	// The structural claim: a verification client has no reachable peer path.
	if n := len(client.Listeners()); n != 0 {
		return fmt.Errorf("%w: verification client opened %d listener(s)", ErrSeedDataIncomplete, n)
	}

	spec := at.TorrentSpecFromMetaInfo(s.mi)
	spec.DisallowDataUpload = true
	spec.Trackers = nil
	tor, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("add torrent for verification: %w", err)
	}
	start := time.Now()
	// VerifyDataContext is synchronous per piece: state afterwards is the truth.
	// Complete() is deliberately not used - it is a cached flag that lags a direct
	// verification pass and never becomes true for damaged data.
	if verr := tor.VerifyDataContext(ctx); verr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: verification failed: %v", ErrSeedDataIncomplete, verr)
	}
	verified, bad := pieceCounts(tor)
	if bad > 0 {
		return fmt.Errorf("%w: %d of %d pieces are not on disk; rerun the fetch to repair them",
			ErrSeedDataIncomplete, bad, verified+bad)
	}
	s.report = &SeedInfo{VerifiedPieces: verified, TotalPieces: verified + bad, VerifyDuration: time.Since(start)}
	return nil
}

// Serve starts the serving client and blocks until ctx is cancelled. Nothing is
// reachable before this call: startup leaves no listener behind, so a peer cannot
// receive a byte ahead of an explicit Serve.
func (s *SeedSession) Serve(ctx context.Context) error {
	s.mu.Lock()
	started := s.client != nil
	s.mu.Unlock()
	if !started {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// start builds the serving client around the state verification produced.
func (s *SeedSession) start(ctx context.Context) error {
	if s.own == nil {
		return errors.New("seed session is closed")
	}
	ws := s.own.staging
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   ws,
		PieceCompletion: s.pieces,
	})
	cc := clientConfig(ws, s.own.cfg)
	cc.DefaultStorage = st
	client, err := at.NewClient(cc)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("start serving client: %w", err)
	}
	spec := at.TorrentSpecFromMetaInfo(s.mi)
	// Download stays closed for the life of a seed; the pieces are already known
	// complete, so no hashing is needed here and nothing is ever fetched.
	spec.DisallowDataDownload = true
	applySpecDiscovery(spec, s.own.cfg.Discovery)
	tor, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		client.Close()
		_ = st.Close()
		return fmt.Errorf("add torrent for serving: %w", err)
	}
	tor.AllowDataUpload()
	s.mu.Lock()
	s.client, s.cstore, s.tor = client, st, tor
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.closeClient()
		return ctx.Err()
	default:
	}
	return nil
}

// Address is the loopback listen address of the serving client; empty before Serve.
func (s *SeedSession) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", s.client.LocalPort())
}

// Metainfo is the persisted metainfo this session verified against.
func (s *SeedSession) Metainfo() *metainfo.MetaInfo { return s.mi }

// Listening reports whether a serving client currently has a listener, so a test can
// assert that startup leaves no peer path open.
func (s *SeedSession) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil && len(s.client.Listeners()) != 0
}

// UploadedBytes is payload written to peers since the serving client started.
func (s *SeedSession) UploadedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return 0
	}
	var total int64
	for _, tor := range s.client.Torrents() {
		counter := tor.Stats().BytesWrittenData
		total += counter.Int64()
	}
	return total
}

func (s *SeedSession) closeClient() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	if s.cstore != nil {
		_ = s.cstore.Close()
		s.cstore = nil
	}
	s.tor = nil
}

// metadataPaths renders every file entry of a torrent as a staging-relative path,
// sorted, the way a job record stores its selection.
func metadataPaths(info *metainfo.Info) []string {
	name := info.BestName()
	out := make([]string, 0, 8)
	if !info.IsDir() {
		return []string{name}
	}
	for _, f := range info.UpvertedFiles() {
		parts := append([]string{name}, f.Path...)
		out = append(out, strings.Join(parts, "/"))
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func samePathSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sortStrings(as)
	sortStrings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// pieceCounts reports how many pieces are complete and how many are not.
func pieceCounts(t *at.Torrent) (complete, missing int) {
	for i := 0; i < t.NumPieces(); i++ {
		if t.Piece(i).State().Complete {
			complete++
		} else {
			missing++
		}
	}
	return complete, missing
}

// Report is what was verified and opened.
func (s *SeedSession) Report() SeedInfo {
	if s.report == nil {
		return SeedInfo{}
	}
	return *s.report
}

// JobID is the job being served.
func (s *SeedSession) JobID() string { return s.id.JobID() }

// Close stops the serving client, closes its storage and only then releases the
// workspace lock (Session.Close owns that order). Closing before Release matters:
// a released lock with a live client would let a second owner start writing while
// this one still serves.
func (s *SeedSession) Close() error {
	s.closeClient()
	if s.pieces != nil {
		_ = s.pieces.Close()
		s.pieces = nil
	}
	if s.own == nil {
		return nil
	}
	err := s.own.Close()
	s.own = nil
	return err
}
