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

// capture verifies the selected files and writes a capture manifest through
// the shared manifest package, so `manifest verify` and `archive import` work
// on the result without changes.
func (s *Session) capture(o *opened, selected []validatedFile, jobID string) (Capture, error) {
	cm := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    o.src.canonical(),
		Site:         Backend,
		DownloadedAt: time.Now().UTC(),
		OutputDir:    s.staging,
		Tool:         "provenance",
		ToolVersion:  version.Version,
		Options: manifest.CaptureOptions{
			OutputDir: s.staging,
		},
	}
	stats := o.t.Stats()
	cap := Capture{
		InfoHash:   o.info.InfoHash,
		JobID:      jobID,
		StagingDir: s.staging,
		BytesRead:  stats.BytesReadUsefulData.Int64(),
	}

	for _, sf := range selected {
		// Final path check: the path is relative, but resolution (symlinked
		// parents) must still land inside staging before we read bytes.
		full, err := resolveCaptured(s.staging, sf.rel)
		if err != nil {
			return Capture{}, err
		}
		fi, err := os.Stat(full)
		if err != nil {
			return Capture{}, fmt.Errorf("%w: %s: %v", ErrNotFinalized, sf.rel, err)
		}
		if !fi.Mode().IsRegular() {
			return Capture{}, fmt.Errorf("%w: %s is not a regular file", ErrNotFinalized, sf.rel)
		}
		if want := sf.file.Length(); fi.Size() != want {
			return Capture{}, fmt.Errorf("%w: %s is %d bytes, metainfo says %d", ErrNotFinalized, sf.rel, fi.Size(), want)
		}
		sum, err := manifest.FileSha256(full)
		if err != nil {
			return Capture{}, fmt.Errorf("hash %s: %w", sf.rel, err)
		}

		meta, err := json.Marshal(itemMetadata{
			InfoHash:       o.info.InfoHash,
			JobID:          jobID,
			TorrentPath:    sf.rel,
			PiecesVerified: true,
		})
		if err != nil {
			return Capture{}, fmt.Errorf("encode item metadata: %w", err)
		}
		item := manifest.CaptureItem{
			ExternalID:     o.info.InfoHash + ":" + sf.rel,
			URL:            o.src.canonical(),
			CanonicalURL:   o.src.canonical(),
			Title:          filepath.Base(sf.rel),
			Kind:           kindFor(sf.rel),
			Extractor:      Backend,
			DownloadedPath: sf.rel,
			ByteSize:       fi.Size(),
			Sha256:         sum,
			CapturedAt:     time.Now().UTC(),
			Status:         "ok",
			RawMetadata:    meta,
		}
		if err := manifest.WriteCaptureItem(s.staging, item.ExternalID, &item); err != nil {
			return Capture{}, fmt.Errorf("write capture item: %w", err)
		}
		cm.Items = append(cm.Items, item)
		cap.Assets = append(cap.Assets, Asset{Path: sf.rel, Length: fi.Size(), SHA256: sum})
	}

	runPath := manifest.RunPath(s.staging)
	if err := manifest.WriteCaptureManifest(s.staging, runPath, cm); err != nil {
		return Capture{}, fmt.Errorf("write capture manifest: %w", err)
	}
	if rel, err := filepath.Rel(s.staging, runPath); err == nil {
		cap.ManifestPath = filepath.ToSlash(rel)
	}
	return cap, nil
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
