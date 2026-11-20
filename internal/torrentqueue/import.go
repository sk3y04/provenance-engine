package torrentqueue

// The optional vault handoff for a committed torrent capture.
//
// A capture is the transfer's commit point; an import is a second, independent
// step. Keeping them apart is what makes "retry the import" not "download it
// again": the queue record retains a committed Capture and only Import.State
// moves. Re-running the import is safe because the vault revision is
// content-addressed and the blob store is idempotent.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/catalog"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/version"
)

var (
	// ErrImportTargetConflict means an equivalent entry is already queued for a
	// different vault. One capture intent is handed off to one target.
	ErrImportTargetConflict = errors.New("entry is already queued for a different vault")
	// ErrImportTargetMissing means this entry has no vault handoff configured.
	ErrImportTargetMissing = errors.New("entry has no vault import target; add one with `torrent queue import --vault --collection`")
	// ErrImportNotReady means the vault handoff is only allowed after the capture
	// commit point. A payload-ready job with no run manifest is not imported.
	ErrImportNotReady = errors.New("entry has no committed capture to import")
)

// ImportCapture performs the recorded vault handoff once. It never starts a
// transfer: it reads the committed run manifest, verifies the referenced files
// are regular files inside the workspace, writes a content-addressed revision,
// and records the outcome on the entry.
//
// The entry is moved to ImportPending before any vault write, so a crash during
// the handoff is retried on the next run. A failure records ImportFailed and
// leaves Phase/Capture untouched; only an explicit retry moves it back.
func (s *Store) ImportCapture(id string) (Entry, error) {
	entry, err := s.Get(id)
	if err != nil {
		return Entry{}, err
	}
	if entry.Import == nil {
		return entry, ErrImportTargetMissing
	}
	if entry.Phase != PhaseCompleted || entry.Capture == nil || entry.Workspace == "" {
		return entry, ErrImportNotReady
	}
	if entry.Import.State == ImportImported && entry.Import.RevisionID != "" &&
		revisionExists(entry.Import.Target.VaultRoot, entry.Import.RevisionID) {
		return entry, nil
	}

	// Durable boundary: from here on the record says "pending", so a process
	// killed between the vault write and the result update retries the same
	// idempotent import rather than looking finished.
	entry, err = s.Update(id, func(e *Entry) error {
		if e.Import == nil {
			return ErrImportTargetMissing
		}
		if e.Phase != PhaseCompleted || e.Capture == nil || e.Workspace == "" {
			return ErrImportNotReady
		}
		e.Import.State = ImportPending
		e.Import.Attempts++
		e.Import.LastError = ""
		e.Import.RevisionID = ""
		e.Import.ImportedAt = time.Time{}
		return nil
	})
	if err != nil {
		return Entry{}, err
	}

	rev, ierr := importCommittedCapture(entry, s.root)
	if ierr != nil {
		return s.recordImportFailure(id, ierr)
	}

	done, uerr := s.Update(id, func(e *Entry) error {
		if e.Import == nil {
			return ErrImportTargetMissing
		}
		e.Import.State = ImportImported
		e.Import.RevisionID = rev.ID
		e.Import.ImportedAt = time.Now().UTC()
		e.Import.LastError = ""
		return nil
	})
	if uerr != nil {
		return Entry{}, uerr
	}
	return done, nil
}

// importCommittedCapture is the read-only part of the handoff plus the two
// idempotent vault writes. It returns the revision that was written.
func importCommittedCapture(entry Entry, root string) (*archive.Revision, error) {
	if entry.Import == nil {
		return nil, ErrImportTargetMissing
	}
	target := entry.Import.Target
	if strings.TrimSpace(target.VaultRoot) == "" || strings.TrimSpace(target.Collection) == "" {
		return nil, ErrImportTargetMissing
	}

	workspace, err := entryWorkspace(root, entry.Workspace)
	if err != nil {
		return nil, err
	}
	if entry.JobID != "" && filepath.Base(workspace) != entry.JobID {
		return nil, errors.New("workspace does not belong to the recorded job")
	}
	manifestPath, err := confinedJoin(workspace, entry.Capture.Manifest)
	if err != nil {
		return nil, fmt.Errorf("capture manifest: %w", err)
	}
	fi, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("capture manifest is not readable: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, errors.New("capture manifest is not a regular file")
	}
	cm, err := manifest.ReadCaptureManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if cm.CaptureKey != "" && entry.Capture.Key != "" && cm.CaptureKey != entry.Capture.Key {
		return nil, fmt.Errorf("manifest capture key %s does not match the recorded key %s", cm.CaptureKey, entry.Capture.Key)
	}

	// The manifest is ours, but the bytes are on a filesystem. Re-check every
	// referenced path against the workspace (no traversal, no symlink escape)
	// before the archive reads and hashes it.
	for _, item := range cm.Items {
		if item.DownloadedPath == "" || item.Sha256 == "" {
			continue
		}
		full, jerr := confinedJoin(workspace, item.DownloadedPath)
		if jerr != nil {
			return nil, fmt.Errorf("capture item %s: %w", item.DownloadedPath, jerr)
		}
		if verr := regularFileWithin(workspace, full); verr != nil {
			return nil, fmt.Errorf("capture item %s: %w", item.DownloadedPath, verr)
		}
	}

	// The archive must resolve item paths against the workspace, never against a
	// path claimed by the manifest.
	cm.OutputDir = workspace
	cm.Options.OutputDir = workspace

	vault := target.VaultRoot
	if err := os.MkdirAll(vault, 0o755); err != nil {
		return nil, fmt.Errorf("prepare vault: %w", err)
	}
	bs := blobstore.New(vault)
	rev, err := archive.IngestCaptureManifest(bs, archive.IngestOptions{
		VaultRoot:      vault,
		CollectionName: target.Collection,
		Source: archive.Source{
			Kind:      archive.SourceImport,
			URL:       sanitizedSource(entry),
			Reference: entry.JobID,
		},
		Tool:        "provenance-torrent",
		ToolVersion: version.Version,
	}, cm)
	if err != nil {
		return nil, fmt.Errorf("ingest capture into vault: %w", err)
	}
	if rev == nil || len(rev.Entities) == 0 {
		return nil, errors.New("vault ingest produced no entities")
	}
	if err := archive.WriteRevision(vault, rev); err != nil {
		return nil, fmt.Errorf("write vault revision: %w", err)
	}
	now := time.Now()
	if err := catalog.UpsertCollection(vault, archive.ArchiveCollection{
		Name:      target.Collection,
		VaultRoot: vault,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("record vault collection: %w", err)
	}
	return rev, nil
}

func (s *Store) recordImportFailure(id string, cause error) (Entry, error) {
	entry, err := s.Update(id, func(e *Entry) error {
		if e.Import == nil {
			return nil
		}
		e.Import.State = ImportFailed
		e.Import.LastError = SanitizeError(cause)
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return entry, fmt.Errorf("import capture: %w", cause)
}

// PendingImports lists committed captures whose handoff has not started or was
// interrupted. ImportFailed is deliberately excluded: a failed handoff needs the
// explicit retry that `queue import` and the TUI action provide.
func (s *Store) PendingImports() ([]Entry, error) {
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0)
	for _, e := range entries {
		if e.Import != nil && e.Import.State == ImportPending && e.Phase == PhaseCompleted && e.Capture != nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// entryWorkspace resolves a recorded workspace reference against the queue root
// and requires it to be one of the runner-managed job directories. It is never
// derived from a torrent name, a display label or a manifest claim.
func entryWorkspace(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("entry has no workspace")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("workspace reference is absolute")
	}
	jobsRoot := filepath.Join(root, filepath.FromSlash(".provenance/torrent/jobs"))
	ws := filepath.Join(root, filepath.FromSlash(rel))
	if !within(jobsRoot, ws) || ws == jobsRoot {
		return "", errors.New("workspace reference is outside the job workspaces")
	}
	return ws, nil
}

func sanitizedSource(e Entry) string {
	if e.Source.Magnet != "" {
		return e.Source.Magnet
	}
	return ""
}

func revisionExists(vaultRoot, id string) bool {
	if strings.TrimSpace(vaultRoot) == "" || id == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(vaultRoot, "revisions", id, "manifest.json"))
	return err == nil && fi.Mode().IsRegular()
}

// confinedJoin joins a relative path under root and rejects absolute paths and
// traversal, before any filesystem call uses the result.
func confinedJoin(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute path")
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !within(root, full) {
		return "", errors.New("path escapes the workspace")
	}
	return full, nil
}

// regularFileWithin rejects a symlink or non-regular file, then resolves the
// final path and requires it to stay inside root. The workspace lock prevents
// another Provenance writer; this check is about the bytes themselves.
func regularFileWithin(root, full string) error {
	fi, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return errors.New("is not a regular file")
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return err
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if !within(rootResolved, resolved) {
		return errors.New("resolves outside the workspace")
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
