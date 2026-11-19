// Package bittorrent is a selective BitTorrent adapter.
package bittorrent

// Isolated workspaces (BT-05).
//
// A workspace is one runner-managed job's own provenance root: payload,
// completion database, exact metainfo, job record, lock, capture items and
// published runs all live under
//
//	<root>/.provenance/torrent/jobs/<jobID>/
//
// and nothing mutable is shared with the top level or with another job. The
// session is rooted there (Session.staging == the workspace), so every path the
// adapter already derives — state files, lock, capture namespace, containment
// checks — becomes job-scoped without a second path scheme:
//
//	<ws>/<torrent name>/<path>                  payload (library DataDir == ws)
//	<ws>/.provenance/completion/.torrent.db     piece completion (library-owned)
//	<ws>/.provenance/torrent/<jobID>.job.json   job record
//	<ws>/.provenance/torrent/<hash>.torrent     exact metainfo, 0600
//	<ws>/.provenance/torrent/staging.lock       workspace ownership
//	<ws>/.provenance/items/<id>.json            capture items
//	<ws>/.provenance/runs/<ts>.json             published run (the commit point)
//
// Legacy jobs are untouched: they keep <root>/<name>/…, <root>/.torrent.db and
// <root>/.provenance/… . There is no migration, no fallback and no relayout of
// existing state; the two layouts are addressable independently and a legacy job
// with the same canonical identity is a conflict rather than a second job.
//
// Lock order: an isolated session takes exactly one lock, its workspace lock.
// It does not take the top-level root's lock, because it never writes a
// root-level file — it only reads <root>/.provenance/torrent/<jobID>.job.json to
// detect a legacy conflict, and that file is published by atomic rename. Nothing
// takes both locks, so there is no order to document beyond "none".

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	// workspaceJobsDir is where workspaces live, relative to the top-level root.
	workspaceJobsDir = ".provenance/torrent/jobs"

	// workspaceSystemDir is the adapter's own namespace inside a workspace. A
	// torrent whose top-level name is this cannot be laid out inside a
	// workspace, because its payload directory would merge with the job's
	// records.
	workspaceSystemDir = ".provenance"

	// workspaceCompletionDir holds the job's piece-completion database. It is
	// inside the adapter namespace rather than the payload root so a torrent
	// named like the database cannot collide with it.
	workspaceCompletionDir = ".provenance/completion"
)

var (
	// ErrLegacyJobConflict means a job with the same canonical identity already
	// exists at the top level in the legacy layout. The isolated entry point
	// refuses instead of starting a second job over the same capture intent, and
	// it never adopts or migrates the legacy record.
	ErrLegacyJobConflict = errors.New("legacy job already owns this identity")

	// ErrInvalidWorkspaceID means the job id handed to WorkspacePath is not a
	// canonical identity form. Workspace paths are derived from validated
	// identities only, never from a magnet string or a truncated key.
	ErrInvalidWorkspaceID = errors.New("invalid torrent job id for a workspace")
)

// WorkspacePath returns the absolute workspace directory for an identity. The
// directory is derived from the canonical job id — bt-<full v1 hash> for v1 and
// hybrid, bt2-<full v2 hash> for v2-only — so two identities that would share
// the library's 20-byte storage key still get distinct workspaces.
func WorkspacePath(root string, id Identity) (string, error) {
	jobID := id.JobID()
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	if !id.HasV1 && !id.HasV2 {
		return "", fmt.Errorf("%w: identity carries no hash", ErrInvalidWorkspaceID)
	}
	return filepath.Join(root, filepath.FromSlash(workspaceJobsDir), jobID), nil
}

// validateJobID accepts only the two canonical forms, so a caller cannot pass a
// bare hash, a display name or a truncated key and have it become a path.
func validateJobID(jobID string) error {
	switch {
	case strings.HasPrefix(jobID, "bt-") && isHex(jobID[3:], 40):
		return nil
	case strings.HasPrefix(jobID, "bt2-") && isHex(jobID[4:], 64):
		return nil
	}
	return fmt.Errorf("%w: %q is not bt-<40 hex> or bt2-<64 hex>", ErrInvalidWorkspaceID, jobID)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// IsolatedJob is one open workspace: a Session rooted at the job's directory,
// holding its exclusive lock and its own client and completion database.
type IsolatedJob struct {
	sess      *Session
	id        Identity
	workspace string
	src       source
}

// JobID is the canonical job id the workspace is keyed by.
func (j *IsolatedJob) JobID() string { return j.id.JobID() }

// Workspace is the absolute job directory.
func (j *IsolatedJob) Workspace() string { return j.workspace }

// Identity is the full canonical identity, including both hashes for a hybrid.
func (j *IsolatedJob) Identity() Identity { return j.id }

// Fetch runs the ordinary transfer and capture pipeline inside the workspace.
// Selection, admission, verification, capture keys and reconciliation are the
// same code as a foreground fetch; only the root differs.
func (j *IsolatedJob) Fetch(ctx context.Context, selection []string) (Capture, error) {
	return j.sess.fetchSource(ctx, j.src, selection)
}

// Close stops the client, closes the workspace's completion database and
// releases the workspace lock.
func (j *IsolatedJob) Close() error { return j.sess.Close() }

// OpenIsolated resolves a source to its canonical identity and opens that
// identity's workspace. A magnet is resolved metadata-only first, so a btmh-only
// magnet naming a hybrid lands in its canonical bt-<v1> workspace instead of
// creating a provisional bt2- job.
func OpenIsolated(ctx context.Context, cfg Config, rawSource string) (*IsolatedJob, error) {
	mode, err := ParseDiscovery(string(cfg.Discovery))
	if err != nil {
		return nil, err
	}
	cfg.Discovery = mode
	root, err := filepath.Abs(strings.TrimSpace(cfg.StagingDir))
	if err != nil {
		return nil, fmt.Errorf("bittorrent: resolve root: %w", err)
	}
	if fi, serr := os.Stat(root); serr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("bittorrent: root %s is not a directory", root)
	}

	src, err := parseSource(rawSource)
	if err != nil {
		return nil, err
	}
	id := src.id
	if src.kind == "magnet" {
		mi, resolved, rerr := resolveMetainfo(ctx, cfg, src)
		if rerr != nil {
			return nil, rerr
		}
		src = metainfoSource(mi, resolved)
		id = resolved
	}
	if err := checkLegacyConflict(root, id); err != nil {
		return nil, err
	}
	sess, err := openWorkspace(root, id, cfg)
	if err != nil {
		return nil, err
	}
	return &IsolatedJob{sess: sess, id: id, workspace: sess.staging, src: src}, nil
}

// FetchIsolated is the one-shot form: open the workspace, fetch, close.
func FetchIsolated(ctx context.Context, cfg Config, rawSource string, selection []string) (Capture, error) {
	job, err := OpenIsolated(ctx, cfg, rawSource)
	if err != nil {
		return Capture{}, err
	}
	defer func() { _ = job.Close() }()
	return job.Fetch(ctx, selection)
}

// checkLegacyConflict refuses to start an isolated job over an identity the
// top-level root already tracks in the legacy layout. The read is a plain file
// read of an atomically published record, so it needs no root lock.
func checkLegacyConflict(root string, id Identity) error {
	path := filepath.Join(root, filepath.FromSlash(torrentStateDir), id.JobID()+".job.json")
	job, err := readJob(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check legacy job %s: %w", id.JobID(), err)
	}
	if job != nil {
		return fmt.Errorf("%w: %s is already a legacy job at %s; an isolated workspace would be a second job over the same capture intent. Nothing was moved or adopted",
			ErrLegacyJobConflict, id.JobID(), filepath.Dir(path))
	}
	return nil
}

// openWorkspace creates (or opens) the workspace for an identity, takes its
// lock, and starts a client whose payload and completion database live inside
// it. The layout is validated against symlinks before anything is created.
func openWorkspace(root string, id Identity, cfg Config) (*Session, error) {
	ws, err := WorkspacePath(root, id)
	if err != nil {
		return nil, err
	}
	// The top-level layout must not be redirected either: a symlinked jobs
	// directory would put every workspace somewhere else.
	for _, rel := range []string{
		filepath.ToSlash(filepath.Join(".provenance", "torrent", "jobs")),
		filepath.ToSlash(filepath.Join(workspaceJobsDir, id.JobID())),
	} {
		if err := checkNoSymlink(root, rel); err != nil {
			return nil, err
		}
	}
	for _, dir := range []string{
		ws,
		filepath.Join(ws, filepath.FromSlash(torrentStateDir)),
		filepath.Join(ws, filepath.FromSlash(workspaceCompletionDir)),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create workspace %s: %w", dir, err)
		}
	}
	return openWorkspaceAt(ws, id, cfg)
}

// openWorkspaceAt opens a workspace whose directory is already known, taking its
// lock and validating any existing record. Seeds use it for an existing workspace;
// everything else goes through openWorkspace, which derives the path first.
// openWorkspaceBare takes ownership of a workspace and validates its job record,
// without opening storage or starting a client. The caller holds the lock and must
// release it through Session.Close, which tolerates a session that never had a client.
func openWorkspaceBare(ws string, id Identity, cfg Config) (*Session, error) {
	peers, err := parsePeers(cfg.Peers)
	if err != nil {
		return nil, err
	}
	s := &Session{cfg: cfg, peers: peers, staging: ws, isolated: true, workspaceID: id}
	if err := s.acquireOwnership(); err != nil {
		return nil, err
	}
	// An existing workspace must describe the same full identity before anything
	// in it is trusted. This is the adoption check: a record filed under this id
	// that describes a different torrent is a hard error, never a rename.
	if job, jerr := readJob(s.jobPath(id.JobID())); jerr != nil && !errors.Is(jerr, os.ErrNotExist) {
		_ = s.releaseOwnership()
		return nil, jerr
	} else if job != nil {
		if verr := validateJobRecord(ws, job, id); verr != nil {
			_ = s.releaseOwnership()
			return nil, fmt.Errorf("workspace %s: %w", ws, verr)
		}
	}
	return s, nil
}

func openWorkspaceAt(ws string, id Identity, cfg Config) (*Session, error) {
	s, err := openWorkspaceBare(ws, id, cfg)
	if err != nil {
		return nil, err
	}
	completion, err := storage.NewDefaultPieceCompletionForDir(filepath.Join(ws, filepath.FromSlash(workspaceCompletionDir)))
	if err != nil {
		_ = s.releaseOwnership()
		return nil, fmt.Errorf("open workspace completion database: %w", err)
	}
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   ws,
		PieceCompletion: completion,
	})
	cc := clientConfig(ws, cfg)
	// The workspace owns its storage: one completion database per job, not the
	// root's shared one. Set after clientConfig so an injected test storage
	// cannot silently reintroduce a shared database.
	cc.DefaultStorage = st
	client, err := at.NewClient(cc)
	if err != nil {
		_ = st.Close()
		_ = s.releaseOwnership()
		return nil, fmt.Errorf("start workspace client: %w", err)
	}
	if clientLifecycleHook != nil {
		clientLifecycleHook("open:payload")
	}
	if clientOpenHook != nil {
		clientOpenHook(client)
	}
	s.client = client
	s.ownStorage = st
	return s, nil
}

// resolveMetainfo fetches only the info dictionary for a magnet, under the same
// discovery policy the job will run with, and returns the exact metainfo plus
// the canonical identity it resolves to. No payload storage is opened and no
// workspace exists yet.
func resolveMetainfo(ctx context.Context, cfg Config, src source) (*metainfo.MetaInfo, Identity, error) {
	timeout := cfg.MetadataTimeout
	if timeout <= 0 {
		timeout = defaultMetadataTimeout
	}
	tmp, err := os.MkdirTemp("", "provenance-isolated-resolve-")
	if err != nil {
		return nil, Identity{}, fmt.Errorf("resolve metadata: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	cc := clientConfig(tmp, cfg)
	// Metadata only: an in-memory completion and no durable payload anywhere.
	cc.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   tmp,
		PieceCompletion: storage.NewMapPieceCompletion(),
	})
	spec, err := at.TorrentSpecFromMagnetUri(src.magnet)
	if err != nil {
		return nil, Identity{}, fmt.Errorf("%w: %s", ErrUnsupportedSource, sanitizeSourceText(err.Error(), src.magnet))
	}
	spec.DeferStorageOpen = true
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
	applySpecDiscovery(spec, cfg.Discovery)

	client, err := at.NewClient(cc)
	if err != nil {
		return nil, Identity{}, fmt.Errorf("resolve metadata: start client: %w", err)
	}
	if clientLifecycleHook != nil {
		clientLifecycleHook("open:resolver")
	}
	defer func() {
		_ = client.Close()
		if clientLifecycleHook != nil {
			clientLifecycleHook("close:resolver")
		}
	}()
	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return nil, Identity{}, errors.New("resolve metadata: " + sanitizeSourceText(err.Error(), src.magnet))
	}
	t.DisallowDataDownload()
	t.DisallowDataUpload()
	if len(cfg.Peers) > 0 {
		peers, perr := parsePeers(cfg.Peers)
		if perr != nil {
			return nil, Identity{}, perr
		}
		t.AddPeers(peers)
	}
	metaCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-t.GotInfo():
	case <-metaCtx.Done():
		return nil, Identity{}, fmt.Errorf("%w: %w", ErrMetadataTimeout, metaCtx.Err())
	}
	info := t.Info()
	if info == nil {
		return nil, Identity{}, errors.New("torrent reported no info after metadata")
	}
	resolved, err := IdentityFromInfo(info, t.Metainfo().InfoBytes)
	if err != nil {
		return nil, Identity{}, err
	}
	if !resolved.Compare(src.id) {
		return nil, Identity{}, fmt.Errorf("metadata identity %s does not match source %s", resolved, src.id)
	}
	if err := checkPrivatePolicy(info, cfg.Discovery); err != nil {
		return nil, Identity{}, fmt.Errorf("%w (discovery already ran while the metadata was being fetched)", err)
	}
	mi := t.Metainfo()
	return &mi, resolved, nil
}

// metainfoSource is an in-memory source whose identity is already known. It
// exists so the isolated entry point can hand the resolved metainfo to the same
// admission and capture pipeline a local .torrent uses, without writing a
// temporary file outside the workspace.
func metainfoSource(mi *metainfo.MetaInfo, id Identity) source {
	// The library serialises an empty piece-layer table as a non-nil empty map,
	// while a decoded .torrent leaves it nil. A non-nil table makes the library
	// require a piece root for every multi-piece file, which a v1 torrent does
	// not have — so normalise it here, where the in-memory metainfo enters.
	if mi != nil && len(mi.PieceLayers) == 0 {
		cp := *mi
		cp.PieceLayers = nil
		mi = &cp
	}
	return source{kind: "metainfo", id: id, infoHash: id.StorageKey(), mi: mi}
}
