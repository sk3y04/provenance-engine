package torrentqueue

// The real worker backend: a thin mapping onto the isolated fetch path, so the
// queue adds durability and ordering without duplicating transfer logic.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// Adapter executes entries through the BT-05 isolated workspace path.
type Adapter struct {
	// MetadataTimeout and Timeout are passed through; zero uses the adapter's
	// defaults. Both are per-attempt, because every entry gets its own run.
	MetadataTimeout time.Duration
	Timeout         time.Duration
}

// defaultPeersPerJob is the established-peer cap an unconfigured runner has always
// used.
const defaultPeersPerJob = 40

func (a Adapter) config(req Request) bittorrent.Config {
	peers := req.PeersPerAttempt
	if peers <= 0 {
		peers = defaultPeersPerJob
	}
	return bittorrent.Config{
		StagingDir:      req.Root,
		Peers:           req.Peers,
		Discovery:       bittorrent.Discovery(req.Discovery),
		MaxPeers:        peers,
		MaxHalfOpen:     req.HalfOpenPerAttempt,
		Timeout:         a.Timeout,
		MetadataTimeout: a.MetadataTimeout,
		// Seeding is deliberately out of this slice: a queue entry completes at
		// its capture and releases its slot.
		KeepSeeding: false,
		// The runner's shared budgets; nil means unlimited. Every client the
		// adapter builds (metadata resolver, payload session) inherits them.
		DownloadLimiter: req.DownloadLimiter,
		UploadLimiter:   req.UploadLimiter,
	}
}

// Run resolves (if needed) and transfers one entry, returning the validated
// identity and the published capture.
func (a Adapter) Run(ctx context.Context, req Request) (Outcome, error) {
	cfg := a.config(req)
	var cap bittorrent.Capture
	var err error
	// Resume only when there is a job to resume: a known identity with no job
	// record yet is a first attempt, which still needs one metadata exchange.
	// This is not a discovery fallback - the profile is the recorded one either way.
	if req.KnownJobID != "" && bittorrent.HasJob(req.Root, req.KnownJobID) {
		// Resolved and started before: reopen the workspace and resume from the
		// persisted metainfo. No metadata exchange, no peer needed to verify.
		cap, err = bittorrent.RunKnownWorkspace(ctx, cfg, req.KnownJobID, req.Selection)
	} else {
		cap, err = bittorrent.FetchIsolated(ctx, cfg, req.Source, req.Selection)
	}
	if err != nil {
		if errors.Is(err, bittorrent.ErrStagingBusy) {
			return Outcome{}, fmt.Errorf("%w: %s", ErrWorkspaceBusy, SanitizeError(err))
		}
		return Outcome{}, err
	}
	ws, werr := relativeTo(req.Root, cap.StagingDir)
	if werr != nil {
		return Outcome{}, werr
	}
	return Outcome{
		JobID:        cap.JobID,
		InfoHash:     cap.InfoHash,
		InfoHashV2:   cap.InfoHashV2,
		Workspace:    ws,
		CaptureKey:   cap.CaptureKey,
		ManifestPath: cap.ManifestPath,
		Reconciled:   cap.Reconciled,
	}, nil
}

// Adopt looks for a published capture without starting a transfer.
func (a Adapter) Adopt(root, jobID string, selection []string) (CaptureRef, bool) {
	ws, err := bittorrent.WorkspaceDir(root, jobID)
	if err != nil {
		return CaptureRef{}, false
	}
	ref, ok := bittorrent.AdoptCapture(ws, jobID, selection)
	if !ok {
		return CaptureRef{}, false
	}
	return CaptureRef{
		Key:         ref.Key,
		Manifest:    ref.Manifest,
		CommittedAt: ref.CommittedAt,
		Reconciled:  ref.Reconciled,
	}, true
}

func relativeTo(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("workspace %s is outside the root: %w", path, err)
	}
	return filepath.ToSlash(rel), nil
}

// OpenSeed builds a verified seed session for a completed entry. It reuses the exact
// startup path proven in BT-08d.2a (network-inert verification against fresh
// completion state, then an explicit Serve) and the same shared upload budget and
// per-job peer limit as a download - seeding never gets a second limiter.
func (a Adapter) OpenSeed(entry Entry, req Request) (SeedSession, error) {
	cfg := a.config(req)
	// The entry names its workspace by canonical id; the helper derives the path so
	// this cannot drift from the workspace layout.
	ws, werr := bittorrent.WorkspacePathForJob(req.Root, entry.JobID)
	if werr != nil {
		return nil, werr
	}
	cfg.StagingDir = ws
	cfg.Discovery = bittorrent.Discovery(req.Discovery)
	sess, err := bittorrent.OpenSeedSession(context.Background(), cfg, entry.JobID)
	if err != nil {
		return nil, err
	}
	return sess, nil
}
