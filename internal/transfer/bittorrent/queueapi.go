package bittorrent

// Entry points used by the local queue (BT-07).
//
// The queue needs three things the fetch path does not expose: a way to learn a
// source's canonical identity *without any network*, a way to reopen a known
// workspace without re-resolving the source, and a way to see an already
// published capture without starting a transfer. They are here, in this package,
// because identity rules, workspace layout and capture reconciliation live here.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// PreparedSource is what enqueue-time validation learns about a source without
// touching the network.
type PreparedSource struct {
	// Sanitized is the identity-only source safe for a public queue record:
	// a magnet carrying xt entries and nothing else. Trackers, web seeds and peer
	// hints stay out of it.
	Sanitized string
	// JobID is the canonical job id, empty when the identity is not yet known.
	JobID string
	// InfoHash / InfoHashV2 are the full hashes, empty when unknown.
	InfoHash   string
	InfoHashV2 string
	// Workspace is the job-root-relative workspace directory, empty when the
	// identity is not yet known. It is never derived from a display name or a
	// truncated key.
	Workspace string
	// Unresolved reports that the source needs a metadata exchange before a
	// canonical identity - and therefore a workspace - exists.
	Unresolved bool
	// Kind is "magnet" or "file".
	Kind string
}

// PrepareSource validates a source at enqueue time. It reads a local .torrent,
// but never contacts a peer, so a magnet whose identity cannot be settled
// offline is reported as Unresolved instead of being rejected.
//
// A source that cannot be resumed at all is refused here rather than accepted as
// a job that will never start: an unreadable .torrent file, an unsupported scheme,
// a v2-only magnet (no piece layers over BEP 9), or an identity already held by a
// legacy job at the root.
func PrepareSource(root, rawSource string, discovery Discovery) (PreparedSource, error) {
	var out PreparedSource
	if _, err := ParseDiscovery(string(discovery)); err != nil {
		return out, err
	}
	absRoot, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return out, fmt.Errorf("resolve root: %w", err)
	}
	src, err := parseSource(rawSource)
	if err != nil {
		return out, err
	}
	out.Kind = src.kind
	switch src.kind {
	case "magnet":
		// A magnet is identity-only until metadata arrives. The btih form pins the
		// canonical job id already (v1 and hybrid are both bt-<v1>), but btmh-only
		// and dual forms still need metadata to know whether the torrent is hybrid,
		// so no workspace is attached before that.
		if !src.id.HasV1 {
			out.Sanitized = sanitizedMagnet(src.id)
			out.Unresolved = true
			out.InfoHashV2 = src.id.V2Hex()
			return out, nil
		}
		out.InfoHash = src.id.V1Hex()
		out.InfoHashV2 = src.id.V2Hex()
		out.Sanitized = sanitizedMagnet(src.id)
		out.JobID = src.id.JobID()
		ws, werr := WorkspacePath(absRoot, src.id)
		if werr != nil {
			return out, werr
		}
		out.Workspace = relativeToRoot(absRoot, ws)
		if cerr := checkLegacyConflict(absRoot, src.id); cerr != nil {
			return out, cerr
		}
		return out, nil
	case "file":
		mi, lerr := metainfo.LoadFromFile(src.path)
		if lerr != nil {
			return out, fmt.Errorf("%w: %v", ErrUnsupportedSource, sanitizeSourceText(lerr.Error(), src.path))
		}
		info, ierr := mi.UnmarshalInfo()
		if ierr != nil {
			return out, fmt.Errorf("parse %s: %w", filepath.Base(src.path), ierr)
		}
		id, ierr := requireIdentity(mi, &info)
		if ierr != nil {
			return out, ierr
		}
		if perr := checkPrivatePolicy(&info, discovery); perr != nil {
			return out, perr
		}
		if _, verr := validateInfoPaths(&info); verr != nil {
			return out, verr
		}
		out.Sanitized = sanitizedMagnet(id)
		out.JobID = id.JobID()
		out.InfoHash = id.V1Hex()
		out.InfoHashV2 = id.V2Hex()
		ws, werr := WorkspacePath(absRoot, id)
		if werr != nil {
			return out, werr
		}
		out.Workspace = relativeToRoot(absRoot, ws)
		if cerr := checkLegacyConflict(absRoot, id); cerr != nil {
			return out, cerr
		}
		return out, nil
	}
	return out, fmt.Errorf("%w: unsupported source", ErrUnsupportedSource)
}

// sanitizedMagnet renders the identity as a magnet with xt entries only.
func sanitizedMagnet(id Identity) string {
	switch {
	case id.HasV1 && id.HasV2:
		return "magnet:?xt=urn:btih:" + id.V1Hex() + "&xt=urn:btmh:1220" + id.V2Hex()
	case id.HasV1:
		return "magnet:?xt=urn:btih:" + id.V1Hex()
	case id.HasV2:
		return "magnet:?xt=urn:btmh:1220" + id.V2Hex()
	}
	return ""
}

func relativeToRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// manifestCovers reports whether a run manifest was written for jobID and covers
// exactly the requested selection: same count, same paths, same job.
func manifestCovers(path, jobID string, want map[string]bool) bool {
	cm, err := manifest.ReadCaptureManifest(path)
	if err != nil || cm.CaptureKey == "" || len(cm.Items) != len(want) {
		return false
	}
	matched := map[string]bool{}
	for _, item := range cm.Items {
		var meta struct {
			JobID string `json:"job_id"`
		}
		if len(item.RawMetadata) > 0 {
			if jerr := json.Unmarshal(item.RawMetadata, &meta); jerr != nil {
				return false
			}
		}
		rel := filepath.ToSlash(item.DownloadedPath)
		if meta.JobID != jobID || !want[rel] || matched[rel] {
			return false
		}
		matched[rel] = true
	}
	return len(matched) == len(want)
}

// WorkspaceDir returns the absolute workspace directory for a canonical job id.
func WorkspaceDir(root, jobID string) (string, error) {
	id, err := parseIdentityString(jobID)
	if err != nil {
		return "", err
	}
	return WorkspacePath(root, id)
}

// HasJob reports whether a job record exists for this id under root. The queue uses
// it to tell "resolved, resumable offline" from "identity known, never started".
func HasJob(root, jobID string) bool {
	if jobID == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(torrentStateDir), jobID+".job.json"))
	return err == nil
}

// OpenWorkspaceJob reopens an existing isolated workspace for a known canonical
// job id. It performs no metadata exchange and no peer contact: the first line of
// open() is the persisted metainfo, so a restart of a resolved job is independent
// of the swarm being reachable. It still takes workspace ownership, so a live
// owner answers ErrStagingBusy.
func OpenWorkspaceJob(cfg Config, jobID string) (*IsolatedJob, error) {
	mode, err := ParseDiscovery(string(cfg.Discovery))
	if err != nil {
		return nil, err
	}
	cfg.Discovery = mode
	id, err := parseIdentityString(jobID)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(strings.TrimSpace(cfg.StagingDir))
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	if fi, serr := os.Stat(root); serr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("root %s is not a directory", root)
	}
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(torrentStateDir), id.JobID()+".job.json")); serr != nil {
		return nil, fmt.Errorf("%w: no job record for %s at %s", ErrJobNotFound, id.JobID(), root)
	}
	sess, err := openWorkspace(root, id, cfg)
	if err != nil {
		return nil, err
	}
	return &IsolatedJob{sess: sess, id: id, workspace: sess.staging, src: resumeSource(id)}, nil
}

// resumeSource is a source that must be served from the workspace's persisted
// metainfo. open() prefers that file, so this only reaches a network path if the
// metainfo is gone, which resume() then reports instead of dialling.
func resumeSource(id Identity) source {
	return source{kind: "resume", id: id, infoHash: id.StorageKey()}
}

// CaptureRef identifies a published capture without loading it.
type CaptureRef struct {
	Key         string
	Manifest    string
	CommittedAt time.Time
	Reconciled  bool
}

// AdoptCapture reports whether dir already holds a published capture for jobID
// covering exactly selection. It reads records and manifests only: no client, no
// hashing, no payload access. This is the startup reconciliation path, and the
// path a runner takes after a crash between "capture committed" and "queue
// updated".
func AdoptCapture(dir, jobID string, selection []string) (CaptureRef, bool) {
	var ref CaptureRef
	if jobID == "" || len(selection) == 0 {
		return ref, false
	}
	// The selection set is part of the capture intent: a record pointing at a
	// capture of a *different* selection must not be adopted as if it covered this
	// one. Coverage is checked against the manifest itself, because the record's
	// pointer says only "a capture exists".
	want := map[string]bool{}
	for _, rel := range selection {
		want[filepath.ToSlash(rel)] = true
	}
	recordPath := filepath.Join(dir, filepath.FromSlash(torrentStateDir), jobID+".job.json")
	if job, err := readJob(recordPath); err == nil && job.Capture != nil && job.Capture.Key != "" {
		if full, jerr := joinWithin(dir, job.Capture.Manifest); jerr == nil {
			if fi, serr := os.Stat(full); serr == nil && fi.Mode().IsRegular() && manifestCovers(full, jobID, want) {
				return CaptureRef{
					Key:         job.Capture.Key,
					Manifest:    job.Capture.Manifest,
					CommittedAt: job.Capture.CommittedAt,
					Reconciled:  job.Capture.Reconciled,
				}, true
			}
		}
	}
	// The record is stale (or the manifest was moved): find the commit itself. The
	// run manifest is the commit point, so a manifest that covers this job and
	// exactly this selection is a published capture even if the record never
	// caught up.
	runs, err := manifest.ListRunManifests(dir)
	if err != nil {
		return ref, false
	}
	for _, run := range runs {
		if !manifestCovers(run, jobID, want) {
			continue
		}
		cm, cerr := manifest.ReadCaptureManifest(run)
		if cerr != nil || cm.CaptureKey == "" {
			continue
		}
		rel, rerr := filepath.Rel(dir, run)
		if rerr != nil {
			continue
		}
		return CaptureRef{
			Key:         cm.CaptureKey,
			Manifest:    filepath.ToSlash(rel),
			CommittedAt: cm.DownloadedAt,
		}, true
	}
	return ref, false
}

// RunKnownWorkspace runs the fetch pipeline for an already resolved job, without
// any metadata exchange, and returns its capture. It is the resume half of a
// queue restart.
func RunKnownWorkspace(ctx context.Context, cfg Config, jobID string, selection []string) (Capture, error) {
	job, err := OpenWorkspaceJob(cfg, jobID)
	if err != nil {
		return Capture{}, err
	}
	defer func() { _ = job.Close() }()
	return job.Fetch(ctx, selection)
}
