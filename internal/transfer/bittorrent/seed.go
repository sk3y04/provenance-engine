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
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
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
// until Close.
type SeedSession struct {
	sess   *Session
	id     Identity
	info   *metainfo.Info
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

	sess, err := openWorkspaceAt(ws, id, cfg)
	if err != nil {
		return nil, err
	}
	s := &SeedSession{sess: sess, id: id, info: &info}

	spec := at.TorrentSpecFromMetaInfo(mi)
	// No upload until local verification has established what can honestly be served.
	spec.DisallowDataUpload = true
	// Download is disallowed only after verification: the piece-hashing loop runs off
	// the torrent's active state, and a torrent with data download disallowed never
	// hashes, so VerifyDataContext would wait on a signal that cannot arrive.
	// Nothing is fetched in the meantime because no file has a download priority.
	// Deliberately not admitSpec: admission defers storage open and disallows data
	// download, and a torrent in that state never hashes - VerifyDataContext would
	// wait on a signal that cannot arrive. Discovery policy is still applied.
	applySpecDiscovery(spec, cfg.Discovery)
	tor, _, err := sess.client.AddTorrentSpec(spec)
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("add torrent for seeding: %w", err)
	}
	s.tor = tor

	start := time.Now()
	// VerifyDataContext is synchronous: it hashes and marks every piece, so the
	// piece states afterwards are the truth. Complete() is deliberately not used - it
	// is a cached flag that lags a direct verification pass, and for damaged data it
	// never becomes true at all.
	if verr := tor.VerifyDataContext(ctx); verr != nil {
		_ = s.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: verification failed: %v", ErrSeedDataIncomplete, verr)
	}
	verified, bad := pieceCounts(tor)
	if bad > 0 {
		_ = s.Close()
		return nil, fmt.Errorf("%w: %d of %d pieces are not on disk; rerun the fetch to repair them",
			ErrSeedDataIncomplete, bad, verified+bad)
	}
	// Verified. Download stays closed for good; upload stays closed until Serve is
	// called, so a peer that connected during verification - or during a slot wait -
	// gets nothing from a session that has not been told to serve.
	tor.DisallowDataDownload()

	s.report = &SeedInfo{
		JobID:          id.JobID(),
		Files:          len(all),
		Bytes:          info.TotalLength(),
		Address:        fmt.Sprintf("127.0.0.1:%d", sess.client.LocalPort()),
		VerifiedPieces: verified,
		TotalPieces:    verified,
		VerifyDuration: time.Since(start),
	}
	return s, nil
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
func (s *SeedSession) Report() SeedInfo { return *s.report }

// Address is the loopback listen address of the serving client.
func (s *SeedSession) Address() string { return s.report.Address }

// JobID is the job being served.
func (s *SeedSession) JobID() string { return s.id.JobID() }

// Serve enables upload for a verified session and blocks until ctx is cancelled.
// Upload is enabled here rather than at open so that nothing is served before the
// caller decides to serve - which is what keeps "verified" and "serving" separate.
// Closing is the caller's job.
func (s *SeedSession) Serve(ctx context.Context) error {
	if s.tor != nil {
		s.tor.AllowDataUpload()
	}
	<-ctx.Done()
	return ctx.Err()
}

// Close stops the client, closes its completion database and only then releases the
// workspace lock (Session.Close owns that order).
func (s *SeedSession) Close() error {
	if s.sess == nil {
		return nil
	}
	err := s.sess.Close()
	s.sess = nil
	return err
}

// UploadedBytes reports payload bytes written to peers, from the client's counters.
func (s *SeedSession) UploadedBytes() int64 {
	if s.sess == nil || s.sess.client == nil {
		return 0
	}
	var total int64
	for _, tor := range s.sess.client.Torrents() {
		counter := tor.Stats().BytesWrittenData
		total += counter.Int64()
	}
	return total
}
