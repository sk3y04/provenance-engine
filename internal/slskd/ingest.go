package slskd

// Ingestion: turning a completed slskd download into a provenance capture.
//
// slskd has no byte-stream download API, so the only way to read a finished file
// is the shared volume mount. That means the ingest path is: map the remote
// destination onto the local mount, enumerate it defensively, verify size and
// hash, then hand the bytes to the existing capture pipeline
// (manifest.CaptureItem + manifest.RunPath) and, when a vault is configured,
// into archive.IngestCaptureManifest. Nothing here invents a second capture
// format.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// Ingestor imports completed downloads.
type Ingestor struct {
	// SlskdDownloadsRoot is the download root as slskd sees it (used only for
	// diagnostics in errors).
	SlskdDownloadsRoot string
	// Mount is that same root as this process sees it. Required.
	Mount string
	// ImportRoot is where capture manifests are written. Defaults to Mount.
	ImportRoot string
	// VaultRoot, when set, additionally ingests each capture into the vault.
	VaultRoot string
	// Collection names the vault collection for new revisions.
	Collection string
	// Cleanup deletes the staging copy after a verified import.
	Cleanup bool
	// Client, when set, also clears the slskd transfer record after cleanup.
	Client *Client
	Now    func() time.Time

	// store is optional: with it the ingestor records the outcome durably.
	store *JobStore
}

// LocatedFile is one file discovered on disk for a job item.
type LocatedFile struct {
	Item Item
	Path string
	Size int64
}

// Ingest imports every item of a job that slskd reports as succeeded.
//
// On any failure the job becomes IMPORT_FAILED and the staging files are left
// exactly as they are: an import that is not verified must never cost the bytes.
func (i *Ingestor) Ingest(ctx context.Context, job Job) (Job, error) {
	now := i.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if strings.TrimSpace(i.Mount) == "" {
		return job, errors.New("provenance download mount is not configured")
	}
	if job.DestinationSubdir == "" {
		return job, errors.New("job has no destination subdirectory")
	}

	dir, err := i.destinationDir(job)
	if err != nil {
		return i.failImport(job, err)
	}
	found, err := i.locate(job, dir)
	if err != nil {
		return i.failImport(job, err)
	}
	if len(found) == 0 {
		return i.failImport(job, fmt.Errorf("no downloaded files found under %s", dir))
	}

	importRoot := i.ImportRoot
	if strings.TrimSpace(importRoot) == "" {
		importRoot = i.Mount
	}
	capture := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    soulseekURL(job),
		Site:         "soulseek",
		DownloadedAt: now(),
		OutputDir:    importRoot,
		Tool:         "slskd",
		ToolVersion:  "0.26.x",
		CaptureKey:   job.ID,
		Options:      manifest.CaptureOptions{OutputDir: importRoot},
	}

	items := make([]manifest.CaptureItem, 0, len(found))
	hashes := make(map[string]string, len(found))
	for _, f := range found {
		sum, err := manifest.FileSha256(f.Path)
		if err != nil {
			return i.failImport(job, fmt.Errorf("hash %s: %w", f.Path, err))
		}
		rel, err := filepath.Rel(importRoot, f.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return i.failImport(job, fmt.Errorf("file %s is outside the import root %s", f.Path, importRoot))
		}
		it := manifest.CaptureItem{
			ExternalID:     job.ID + ":" + RemoteBaseName(f.Item.RemoteFilename),
			URL:            soulseekFileURL(job, f.Item.RemoteFilename),
			Title:          RemoteBaseName(f.Item.RemoteFilename),
			Author:         job.Peer,
			Kind:           kindForExtension(RemoteBaseName(f.Item.RemoteFilename)),
			Extractor:      "slskd",
			DownloadedPath: filepath.ToSlash(rel),
			ByteSize:       f.Size,
			Sha256:         sum,
			CapturedAt:     now(),
			RawMetadata:    rawMetadata(job, f.Item),
		}
		hashes[f.Item.RemoteFilename] = sum
		items = append(items, it)
		if err := manifest.WriteCaptureItem(importRoot, it.ExternalID, &it); err != nil {
			return i.failImport(job, fmt.Errorf("write capture item: %w", err))
		}
	}
	capture.Items = items

	// The run manifest is the commit point: archive import reads runs/*.json, and
	// items alone are invisible to it.
	runPath := manifest.RunPath(importRoot)
	if err := manifest.WriteCaptureManifest(importRoot, runPath, capture); err != nil {
		return i.failImport(job, fmt.Errorf("write capture manifest: %w", err))
	}

	revisionID := ""
	if strings.TrimSpace(i.VaultRoot) != "" {
		bs := blobstore.New(i.VaultRoot)
		rev, err := archive.IngestCaptureManifest(bs, archive.IngestOptions{
			VaultRoot:      i.VaultRoot,
			CollectionName: i.Collection,
			Source:         archive.Source{Kind: archive.SourceURL, URL: capture.SourceURL, Reference: job.ID},
			Tool:           "slskd",
		}, capture)
		if err != nil {
			return i.failImport(job, fmt.Errorf("ingest capture into vault: %w", err))
		}
		if rev == nil || len(rev.Entities) == 0 {
			return i.failImport(job, errors.New("vault ingest produced no entities"))
		}
		if err := archive.WriteRevision(i.VaultRoot, rev); err != nil {
			return i.failImport(job, fmt.Errorf("write revision: %w", err))
		}
		revisionID = rev.ID
	}

	imported, err := i.storeUpdate(job, func(j *Job) {
		j.Status = JobImported
		j.FailureReason = ""
		j.ImportRoot = importRoot
		j.RevisionID = revisionID
		j.UpdatedAt = now()
		for _, f := range found {
			for k := range j.Items {
				if j.Items[k].RemoteFilename != f.Item.RemoteFilename {
					continue
				}
				j.Items[k].LocalPath = f.Path
				j.Items[k].ByteSize = f.Size
				j.Items[k].Sha256 = hashes[f.Item.RemoteFilename]
				j.Items[k].FailureReason = ""
				// The capture item path is deterministic per job and file.
				j.Items[k].CaptureItem = manifest.ItemPath(importRoot, job.ID+":"+RemoteBaseName(f.Item.RemoteFilename))
			}
		}
	})
	if err != nil {
		return imported, err
	}

	if i.Cleanup {
		cleaned, err := i.cleanup(ctx, imported)
		if err != nil {
			// A failed cleanup must not downgrade a verified import; it is
			// reported as a warning on the job instead.
			warned, _ := i.storeUpdate(imported, func(j *Job) {
				j.FailureReason = "import succeeded but staging cleanup failed: " + err.Error()
			})
			return warned, nil
		}
		imported = cleaned
	}
	return imported, nil
}

// IngestPending imports every job that is waiting at the import boundary.
func (i *Ingestor) IngestPending(ctx context.Context, store *JobStore) ([]Job, error) {
	jobs, err := store.List()
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, job := range jobs {
		if job.Status != JobCompletedPendingImport {
			continue
		}
		next, err := i.Ingest(ctx, job)
		out = append(out, next)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// destinationDir maps the pinned destination onto the local mount with a strict
// containment check.
func (i *Ingestor) destinationDir(job Job) (string, error) {
	abs, err := ensurePathIsInside(i.Mount, filepath.Join(i.Mount, filepath.FromSlash(job.DestinationSubdir)))
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("download destination %s is not present: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("download destination %s is not a directory", abs)
	}
	return abs, nil
}

// locate finds the on-disk file for each item. slskd renames on collision by
// appending "_{ticks}" before the extension, so a strict basename match is not
// enough.
func (i *Ingestor) locate(job Job, dir string) ([]LocatedFile, error) {
	// A staged file may sit in a nested directory: slskd preserves the remote
	// folder layout under the pinned destination. Match on the basename but keep
	// the relative path, or a nested file is read from the wrong location.
	type staged struct {
		rel  string
		size int64
	}
	files := map[string]staged{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// A symlink inside the staging area could point anywhere on the host.
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		abs, err := ensurePathIsInside(dir, path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, abs)
		if err != nil {
			return err
		}
		files[d.Name()] = staged{rel: rel, size: info.Size()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", dir, err)
	}

	out := make([]LocatedFile, 0, len(job.Items))
	for _, it := range job.Items {
		name := RemoteBaseName(it.RemoteFilename)
		match, ok := matchStagedFile(files, name)
		if !ok {
			continue
		}
		if it.Size > 0 && match.size != it.Size {
			return nil, fmt.Errorf("file %s is %d bytes on disk but slskd reported %d", name, match.size, it.Size)
		}
		if match.size == 0 {
			return nil, fmt.Errorf("file %s is empty on disk", name)
		}
		out = append(out, LocatedFile{Item: it, Path: filepath.Join(dir, match.rel), Size: match.size})
	}
	return out, nil
}

// matchStagedFile finds the staged file for an expected basename, accepting the
// "_{ticks}" collision suffix slskd's FileService.MoveFile adds.
func matchStagedFile[T any](files map[string]T, name string) (T, bool) {
	var zero T
	if v, ok := files[name]; ok {
		return v, true
	}
	ext := strings.ToLower(filepath.Ext(name))
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for candidate, v := range files {
		cExt := strings.ToLower(filepath.Ext(candidate))
		cStem := strings.TrimSuffix(candidate, filepath.Ext(candidate))
		if cExt != ext {
			continue
		}
		if tiedSuffix.MatchString(cStem) && strings.HasPrefix(cStem, stem) {
			return v, true
		}
		if tiedSuffix.MatchString(candidate) && strings.HasPrefix(candidate, name) {
			return v, true
		}
	}
	return zero, false
}

// tiedSuffix matches the "_{ticks}" rename slskd applies on a collision.
var tiedSuffix = regexp.MustCompile(`_\d{6,}$`)

func (i *Ingestor) failImport(job Job, cause error) (Job, error) {
	failed, err := i.storeUpdate(job, func(j *Job) {
		j.Status = JobImportFailed
		j.FailureReason = cause.Error()
	})
	if err != nil {
		return job, errors.Join(cause, err)
	}
	return failed, cause
}

// storeUpdate applies a mutation through the store when the job is known to it,
// and returns the mutated job unchanged otherwise (which is what makes the
// ingestor testable without a store).
func (i *Ingestor) storeUpdate(job Job, mutate func(*Job)) (Job, error) {
	if i.store == nil {
		mutate(&job)
		return job, nil
	}
	return i.store.Update(job.ID, func(j *Job) error {
		mutate(j)
		return nil
	})
}

// AttachStore wires the ingestor to a job store.
func (i *Ingestor) AttachStore(s *JobStore) { i.store = s }

// cleanup deletes the staging copy of a verified import.
func (i *Ingestor) cleanup(ctx context.Context, job Job) (Job, error) {
	dir, err := i.destinationDir(job)
	if err != nil {
		return job, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return job, err
	}
	if i.Client != nil {
		for _, it := range job.Items {
			if it.TransferID == "" {
				continue
			}
			if err := i.Client.CancelDownload(ctx, job.Peer, it.TransferID, true); err != nil && !IsStatus(err, 404) {
				return job, err
			}
		}
	}
	return i.storeUpdate(job, func(j *Job) { j.CleanedUp = true })
}

// soulseekURL is the capture's source URL: one Soulseek peer, one job.
func soulseekURL(job Job) string {
	u := url.URL{Scheme: "soulseek", Host: job.Peer, Path: "/" + job.ID}
	return u.String()
}

// soulseekFileURL identifies one file on that peer. The remote path is escaped,
// so a filename with spaces or '#' stays a valid URL.
func soulseekFileURL(job Job, remote string) string {
	u := url.URL{Scheme: "soulseek", Host: job.Peer, Path: "/" + strings.TrimPrefix(normalizeSep(remote), "/")}
	return u.String()
}

// rawMetadata records the Soulseek provenance of one file.
func rawMetadata(job Job, it Item) json.RawMessage {
	payload := map[string]any{
		"source":            "soulseek",
		"peer_username":     job.Peer,
		"remote_filename":   it.RemoteFilename,
		"search_query":      job.SearchText,
		"search_id":         job.SearchID,
		"slskd_external_id": job.ID,
		"slskd_transfer_id": it.TransferID,
		"job_batch_id":      job.BatchID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return data
}

// kindForExtension maps a file extension onto the capture kind vocabulary that
// archive's artifact classifier understands.
func kindForExtension(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".flac", ".mp3", ".m4a", ".ogg", ".opus", ".wav", ".aiff", ".ape", ".wv", ".dsf":
		return "audio"
	case ".mp4", ".mkv", ".avi", ".mov", ".m4v", ".webm", ".flv", ".ts":
		return "video"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tiff":
		return "image"
	case ".pdf", ".epub", ".mobi", ".txt", ".md":
		return "text"
	default:
		return "binary"
	}
}

// SortedNames is a small helper used by the CLI to render a job's files in a
// stable order.
func SortedNames(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, RemoteBaseName(it.RemoteFilename))
	}
	sort.Strings(out)
	return out
}
