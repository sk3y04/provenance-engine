package bittorrent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/version"
)

// itemMetadata is the provenance-specific block stored on each capture item.
// It records identity (infohash, job, path inside the torrent) and nothing
// that could leak a tracker, passkey, or local path.
type itemMetadata struct {
	InfoHash    string `json:"info_hash"`
	JobID       string `json:"job_id"`
	TorrentPath string `json:"torrent_path"`
	// PiecesVerified records that every piece covering this file reached the
	// library's completed state before the bytes were hashed and captured.
	PiecesVerified bool `json:"pieces_verified"`
}

// hashSelectedAssets turns the selected files into capture assets: it confirms
// each one is finalized under its metainfo length and computes the
// Provenance-owned SHA-256 digest. The torrent infohash stays on the job; the
// digest here identifies the artifact bytes only.
func (s *Session) hashSelectedAssets(o *opened, selected []validatedFile) ([]Asset, error) {
	assets := make([]Asset, 0, len(selected))
	for _, sf := range selected {
		full, err := resolveCaptured(s.staging, sf.rel)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(full)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrNotFinalized, sf.rel, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular file", ErrNotFinalized, sf.rel)
		}
		if want := sf.file.Length(); fi.Size() != want {
			return nil, fmt.Errorf("%w: %s is %d bytes, metainfo says %d", ErrNotFinalized, sf.rel, fi.Size(), want)
		}
		sum, err := manifest.FileSha256(full)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", sf.rel, err)
		}
		assets = append(assets, Asset{Path: sf.rel, Length: fi.Size(), SHA256: sum})
	}
	return assets, nil
}

// verifyStagedAssets re-checks staged bytes against the attested payload of a
// job that is retrying at the capture stage. It never consults the torrent
// client: the whole point of the payload-ready state is that a capture retry
// does not touch the swarm again.
//
// Any missing file, size change or digest change means the attestation is stale,
// so the caller drops back to unverified and lets the existing re-verify-and-
// repair path run.
func (s *Session) verifyStagedAssets(want []Asset) error {
	for _, a := range want {
		full, err := resolveCaptured(s.staging, a.Path)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrStagedPayloadInvalid, a.Path, err)
		}
		fi, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("%w: %s is gone: %v", ErrStagedPayloadInvalid, a.Path, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrStagedPayloadInvalid, a.Path)
		}
		if fi.Size() != a.Length {
			return fmt.Errorf("%w: %s is %d bytes, expected %d", ErrStagedPayloadInvalid, a.Path, fi.Size(), a.Length)
		}
		sum, err := manifest.FileSha256(full)
		if err != nil {
			return fmt.Errorf("%w: hash %s: %v", ErrStagedPayloadInvalid, a.Path, err)
		}
		if !equalHex(sum, a.SHA256) {
			return fmt.Errorf("%w: %s hashes to %s, expected %s", ErrStagedPayloadInvalid, a.Path, sum, a.SHA256)
		}
	}
	return nil
}

// commitCapture writes the capture items and then the run manifest, and returns
// the manifest path relative to the job root.
//
// The run manifest is the commit point: `archive import` reads run manifests,
// and it is what findCommittedCapture scans. Items are written first because an
// item without a run manifest is not a capture — it is retried and overwritten
// under the same external id.
func (s *Session) commitCapture(job *jobRecord, assets []Asset, key string) (string, error) {
	now := time.Now().UTC()
	cm := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    job.Source,
		Site:         Backend,
		DownloadedAt: now,
		OutputDir:    s.staging,
		Tool:         "provenance",
		ToolVersion:  version.Version,
		CaptureKey:   key,
		Options: manifest.CaptureOptions{
			OutputDir: s.staging,
		},
	}

	for _, a := range assets {
		// Confinement and size are re-checked at the moment of writing; hashing
		// happened once already in this run, so the recorded digest is the one
		// that was verified.
		full, err := resolveCaptured(s.staging, a.Path)
		if err != nil {
			return "", err
		}
		fi, err := os.Stat(full)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrNotFinalized, a.Path, err)
		}
		if fi.Size() != a.Length {
			return "", fmt.Errorf("%w: %s is %d bytes, expected %d", ErrNotFinalized, a.Path, fi.Size(), a.Length)
		}

		meta, err := json.Marshal(itemMetadata{
			InfoHash:       job.InfoHash,
			JobID:          job.JobID,
			TorrentPath:    a.Path,
			PiecesVerified: true,
		})
		if err != nil {
			return "", fmt.Errorf("encode item metadata: %w", err)
		}
		item := manifest.CaptureItem{
			ExternalID:     job.InfoHash + ":" + a.Path,
			URL:            job.Source,
			CanonicalURL:   job.Source,
			Title:          filepath.Base(a.Path),
			Kind:           kindFor(a.Path),
			Extractor:      Backend,
			DownloadedPath: a.Path,
			ByteSize:       fi.Size(),
			Sha256:         a.SHA256,
			CapturedAt:     now,
			Status:         "ok",
			RawMetadata:    meta,
		}
		if err := manifest.WriteCaptureItem(s.staging, item.ExternalID, &item); err != nil {
			return "", fmt.Errorf("write capture item: %w", err)
		}
		cm.Items = append(cm.Items, item)
	}

	runPath := manifest.RunPath(s.staging)
	if err := manifest.WriteCaptureManifest(s.staging, runPath, cm); err != nil {
		return "", fmt.Errorf("write capture manifest: %w", err)
	}
	rel, err := filepath.Rel(s.staging, runPath)
	if err != nil {
		return "", fmt.Errorf("locate capture manifest: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

// captureResult assembles the value returned to callers.
func (s *Session) captureResult(job *jobRecord, assets []Asset, key, manifestPath string, bytesRead int64, warnings []string, reconciled bool) Capture {
	return Capture{
		InfoHash:     job.InfoHash,
		JobID:        job.JobID,
		StagingDir:   s.staging,
		Assets:       assets,
		BytesRead:    bytesRead,
		ManifestPath: manifestPath,
		CaptureKey:   key,
		State:        job.State,
		Reconciled:   reconciled,
		Warnings:     warnings,
	}
}

func equalHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'F' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'F' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// kindFor maps an extension onto the coarse kind the archive understands.
// Anything unrecognised stays "binary", which the archive records as a generic
// artifact rather than guessing.
func kindFor(rel string) string {
	switch manifest.ExtOf(filepath.Base(rel)) {
	case "mp4", "mkv", "webm", "mov", "avi", "ts", "m4v":
		return "video"
	case "jpg", "jpeg", "png", "gif", "webp", "avif", "bmp", "tiff":
		return "image"
	case "mp3", "flac", "ogg", "m4a", "wav", "opus", "aac":
		return "audio"
	case "md", "txt", "html", "htm":
		return "text"
	default:
		return "binary"
	}
}
