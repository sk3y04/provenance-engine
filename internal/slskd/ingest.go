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
	"crypto/sha256"
	"encoding/hex"
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
	expected := 0
	for _, it := range job.Items {
		if !it.Rejected {
			expected++
		}
	}
	if expected == 0 {
		return i.failImport(job, errors.New("every file in this job was refused by slskd; nothing to import"))
	}
	if len(found) != expected {
		return i.failImport(job, fmt.Errorf("%d of %d downloaded file(s) are not on disk under %s: %s",
			expected-len(found), expected, dir, strings.Join(missingItems(job, found), ", ")))
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

	// Count basenames first: a job can carry the same track name from two
	// folders, and a shared capture identity would silently merge them.
	baseCounts := make(map[string]int, len(job.Items))
	for _, it := range job.Items {
		baseCounts[RemoteBaseName(it.RemoteFilename)]++
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
			ExternalID:     captureExternalID(job.ID, f.Item.RemoteFilename, baseCounts),
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
	// items alone are invisible to it. It is named after the job because
	// manifest.RunPath has one-second granularity, and two jobs finishing in the
	// same second would otherwise overwrite one capture.
	runPath := uniqueRunPath(importRoot, job.ID)
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
				// The capture item path is deterministic per job and file, and
				// uses the same identity the manifest item was written with.
				j.Items[k].CaptureItem = manifest.ItemPath(importRoot, captureExternalID(job.ID, f.Item.RemoteFilename, baseCounts))
			}
		}
	})
	if err != nil {
		return imported, err
	}

	if i.Cleanup {
		cleaned, err := i.cleanup(ctx, imported, found)
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

// stagedFile is one regular file found under the pinned destination.
type stagedFile struct {
	rel  string // slash-separated path relative to the destination
	size int64
}

// locate finds the on-disk file for each item.
//
// slskd preserves the remote folder layout under the pinned destination, but the
// exact root differs (it may strip a peer-prefixed first segment), and it renames
// on collision by appending "_{ticks}". Matching therefore walks from the most
// specific interpretation to the least, and never assigns one file to two items.
func (i *Ingestor) locate(job Job, dir string) ([]LocatedFile, error) {
	var staged []stagedFile
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
		staged = append(staged, stagedFile{rel: filepath.ToSlash(rel), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", dir, err)
	}
	// Deterministic order: two candidates can match the same preference, and a
	// map iteration would pick a different one each run.
	sort.Slice(staged, func(a, b int) bool { return staged[a].rel < staged[b].rel })

	out := make([]LocatedFile, 0, len(job.Items))
	used := make(map[string]bool, len(staged))
	for _, it := range job.Items {
		if it.Rejected {
			continue
		}
		want := strings.TrimPrefix(filepath.ToSlash(normalizeSep(it.RemoteFilename)), "/")
		match, ok := pickStaged(staged, want, used)
		if !ok {
			continue
		}
		name := RemoteBaseName(it.RemoteFilename)
		if it.Size > 0 && match.size != it.Size {
			return nil, fmt.Errorf("file %s is %d bytes on disk but slskd reported %d", name, match.size, it.Size)
		}
		if match.size == 0 {
			return nil, fmt.Errorf("file %s is empty on disk", name)
		}
		used[match.rel] = true
		out = append(out, LocatedFile{Item: it, Path: filepath.Join(dir, filepath.FromSlash(match.rel)), Size: match.size})
	}
	return out, nil
}

// pickStaged resolves one remote path against the staged files.
func pickStaged(staged []stagedFile, want string, used map[string]bool) (stagedFile, bool) {
	for _, f := range staged {
		if !used[f.rel] && f.rel == want {
			return f, true
		}
	}
	for _, f := range staged {
		if !used[f.rel] && strings.HasSuffix(f.rel, "/"+want) {
			return f, true
		}
	}
	segs := strings.Split(want, "/")
	for n := len(segs) - 1; n >= 1; n-- {
		tail := strings.Join(segs[len(segs)-n:], "/")
		for _, f := range staged {
			if used[f.rel] {
				continue
			}
			if f.rel == tail || strings.HasSuffix(f.rel, "/"+tail) {
				return f, true
			}
		}
	}
	base := segs[len(segs)-1]
	for _, f := range staged {
		if used[f.rel] {
			continue
		}
		if ticksMatch(filepath.Base(filepath.FromSlash(f.rel)), base) {
			return f, true
		}
	}
	return stagedFile{}, false
}

// ticksMatch accepts the "_{ticks}" rename slskd applies on a name collision.
func ticksMatch(candidate, want string) bool {
	if candidate == want {
		return true
	}
	ext := filepath.Ext(want)
	if filepath.Ext(candidate) != ext {
		return false
	}
	stem := strings.TrimSuffix(want, ext)
	cStem := strings.TrimSuffix(candidate, ext)
	return strings.HasPrefix(cStem, stem) && tiedSuffix.MatchString(cStem)
}

// captureExternalID is stable per job and file. A basename that appears more than
// once in the job gets a short digest of its remote path, so two albums with the
// same track name do not share one capture identity.
func captureExternalID(jobID, remote string, counts map[string]int) string {
	base := RemoteBaseName(remote)
	if counts[base] > 1 {
		sum := sha256.Sum256([]byte(normalizeSep(remote)))
		ext := filepath.Ext(base)
		base = strings.TrimSuffix(base, ext) + "-" + hex.EncodeToString(sum[:3]) + ext
	}
	return jobID + ":" + base
}

// missingItems names the items locate could not satisfy.
func missingItems(job Job, found []LocatedFile) []string {
	present := make(map[string]bool, len(found))
	for _, f := range found {
		present[f.Item.RemoteFilename] = true
	}
	var missing []string
	for _, it := range job.Items {
		if it.Rejected || present[it.RemoteFilename] {
			continue
		}
		missing = append(missing, RemoteBaseName(it.RemoteFilename))
	}
	return missing
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

// uniqueRunPath keeps the chronological run name and adds the job id.
func uniqueRunPath(root, jobID string) string {
	base := manifest.RunPath(root)
	return strings.TrimSuffix(base, ".json") + "-" + jobID + ".json"
}

// cleanup deletes the files this job imported, then the directories they emptied.
// It never removes the destination wholesale: a custom destination can be shared
// with another job, and deleting bytes this job did not import is not recoverable.
func (i *Ingestor) cleanup(ctx context.Context, job Job, found []LocatedFile) (Job, error) {
	dest, err := i.destinationDir(job)
	if err != nil {
		return job, err
	}
	dirs := make(map[string]bool, len(found)+1)
	for _, f := range found {
		if _, err := ensurePathIsInside(i.Mount, f.Path); err != nil {
			return job, err
		}
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return job, err
		}
		dirs[filepath.Dir(f.Path)] = true
	}
	dirs[dest] = true
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	// Deepest first: a parent is only considered once its children are gone.
	sort.Slice(list, func(a, b int) bool {
		return strings.Count(list[a], string(os.PathSeparator)) > strings.Count(list[b], string(os.PathSeparator))
	})
	for _, d := range list {
		if entries, rerr := os.ReadDir(d); rerr == nil && len(entries) == 0 {
			_ = os.Remove(d)
		}
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
