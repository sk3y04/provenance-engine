package bittorrent

// Offline recheck (BT-06).
//
// Recheck answers one question for one job, with no client and no network: do the
// job's selected working files still hash to the digests that were recorded when
// the capture was published?
//
// It is deliberately weaker than a client force-recheck and deliberately
// narrower than a vault check:
//
//	what it does        hash each selected, finalized, regular file with SHA-256
//	                    and compare it with the per-file capture item
//	what it does not    re-hash pieces against the torrent's piece layers, read
//	                    .part data, open the completion database, prove the
//	                    vault blob exists, or prove anything about liveness
//
// Why the library's own verifier is not used: `Torrent.VerifyDataContext` needs a
// client and an added torrent, hashes through the storage backend (which opens
// the piece-completion database for writing, and on a fresh file set can mark
// pieces and rename `.part` data into place), and updates both in-memory and
// persisted completion state. Calling that here and describing the result as
// read-only would be a false claim about this command's side effects.
//
// Ownership: a lock-free read of the job record does not make concurrent hashing
// safe, so recheck takes the job's existing ownership lock - the same flock a
// fetch takes - without building a session and without touching resume state. A
// live owner answers ErrStagingBusy instead of producing a report that could
// certify bytes mid-write. Sidecar effect, stated plainly: the lock file
// `<root>/.provenance/torrent/staging.lock` is opened (created if somehow absent,
// zero bytes) and locked for the duration.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sk3y04/provenance-engine/internal/filelock"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// RecheckFormat identifies the JSON report, so a reader can refuse a newer one.
const RecheckFormat = "provenance-torrent-recheck-1"

// RecheckStatus is the outcome for one selected file.
type RecheckStatus string

const (
	// RecheckMatch: the finalized bytes hash to the attested digest.
	RecheckMatch RecheckStatus = "match"
	// RecheckMismatch: the file is there and readable, the digest differs.
	RecheckMismatch RecheckStatus = "mismatch"
	// RecheckMissing: no finalized file at the selected path.
	RecheckMissing RecheckStatus = "missing"
	// RecheckPartial: no finalized file, but a `.part` exists, so the job looks
	// interrupted. Nothing was hashed.
	RecheckPartial RecheckStatus = "partial"
	// RecheckUnverifiable: there is no recorded digest to compare against (no
	// published capture, or no capture item for this path).
	RecheckUnverifiable RecheckStatus = "unverifiable"
	// RecheckError: the path could not be checked: it escapes the job root, is
	// not a regular file, or could not be read.
	RecheckError RecheckStatus = "error"
)

// RecheckFile is one deterministic per-file result.
type RecheckFile struct {
	Path string `json:"path"`
	// Status is the outcome above. Anything other than match means the job is not
	// verified; there is no partial credit.
	Status RecheckStatus `json:"status"`
	// AttestedSHA256 is the digest recorded at capture time, when there was one.
	AttestedSHA256 string `json:"attested_sha256,omitempty"`
	// SHA256 is what the bytes hash to now, when they could be hashed.
	SHA256 string `json:"sha256,omitempty"`
	// Bytes is the size on disk, when it could be read.
	Bytes int64 `json:"bytes,omitempty"`
	// AttestedBytes is the size recorded next to the digest.
	AttestedBytes int64 `json:"attested_bytes,omitempty"`
	// Detail is a short, path-relative explanation. It never carries credentials:
	// the record and the item hold only the identity, and paths here are the
	// selection entries, not absolute paths.
	Detail string `json:"detail,omitempty"`
}

// RecheckCapture is the historical capture the job points at. It is reported
// separately from the current outcome: it says a capture was published, not that
// the working copy still matches it.
type RecheckCapture struct {
	Key         string    `json:"key"`
	Manifest    string    `json:"manifest"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
	Reconciled  bool      `json:"reconciled,omitempty"`
	// Present reports whether the manifest file is still there. Recheck never
	// rewrites or repairs it.
	Present bool `json:"present"`
}

// RecheckSummary counts the outcomes. Selected is len(Selection) so a job whose
// selection is empty cannot read as verified.
type RecheckSummary struct {
	Selected     int `json:"selected"`
	Match        int `json:"match"`
	Mismatch     int `json:"mismatch"`
	Missing      int `json:"missing"`
	Partial      int `json:"partial"`
	Unverifiable int `json:"unverifiable"`
	Error        int `json:"error"`
}

// RecheckReport is the whole answer for one job.
type RecheckReport struct {
	Format   string `json:"format"`
	JobID    string `json:"job_id"`
	InfoHash string `json:"info_hash,omitempty"`
	// InfoHashV2 is reported for a hybrid as well, so the report names the full
	// identity rather than the canonical id alone.
	InfoHashV2 string          `json:"info_hash_v2,omitempty"`
	Selection  []string        `json:"selection"`
	Capture    *RecheckCapture `json:"capture,omitempty"`
	Files      []RecheckFile   `json:"files"`
	Summary    RecheckSummary  `json:"summary"`
	// Verified is true only when every selected file matched its attestation.
	// Missing, partial, unverifiable and errored files all mean false.
	Verified bool `json:"verified"`
	// Detail explains a report that checked nothing (unattested job, empty
	// selection).
	Detail string `json:"detail,omitempty"`
	// Error carries a run failure (cancelled, lock lost, context deadline) so the
	// JSON stays parseable. It is never written to stdout as free text.
	Error string `json:"error,omitempty"`
}

// recheckHashChunkHook is a deterministic test seam: it is called after every
// chunk hashed, so a test can cancel part-way through a specific file instead of
// racing a timer. It is nil in production.
var recheckHashChunkHook func()

// recheckChunkSize is the hashing read size. Larger than io.Copy's default so a
// cancellation is checked often enough on a slow disk without a syscall per KiB.
const recheckChunkSize = 1 << 20

// Recheck verifies one job's selected working files against the digests recorded
// at capture time. It reads the job record, the per-file capture items and the
// payload; it starts no client, takes no network action and writes no record,
// item, manifest, metainfo or payload byte.
//
// dir is either a legacy staging root or a directly supplied isolated workspace:
// both are complete job roots, and nothing here walks a tree looking for jobs.
func Recheck(ctx context.Context, dir, rawID string) (RecheckReport, error) {
	report := RecheckReport{Format: RecheckFormat}
	root, err := statusRoot(dir)
	if err != nil {
		return report, err
	}
	req, err := parseIdentityString(rawID)
	if err != nil {
		return report, err
	}
	job, _, err := lookupJob(root, req)
	if err != nil {
		return report, err
	}
	if err := validateJobRecord(root, job, req); err != nil {
		return report, fmt.Errorf("job record for %s: %w", req.JobID(), err)
	}

	report.JobID = job.JobID
	report.InfoHash = job.InfoHash
	report.InfoHashV2 = job.InfoHashV2
	report.Selection = append([]string(nil), job.Selection...)
	report.Capture = captureReference(root, job)

	// Ownership before any hashing: a live writer may be replacing these bytes.
	lock, err := filelock.TryAcquire(jobLockPath(root))
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return report, fmt.Errorf("%w: %s has a live owner, so recheck refused to certify bytes that may still be changing; stop the transfer and retry", ErrStagingBusy, root)
		}
		return report, fmt.Errorf("recheck: take job ownership: %w", err)
	}
	defer func() { _ = lock.Release() }()

	if len(job.Selection) == 0 {
		report.Detail = "the job records an empty selection, so there is nothing to check"
		return report, nil
	}
	for _, rel := range job.Selection {
		if cerr := ctx.Err(); cerr != nil {
			report.Error = "cancelled: " + cerr.Error()
			return report, fmt.Errorf("recheck: %w", cerr)
		}
		report.Files = append(report.Files, recheckFile(ctx, root, job, rel))
	}
	// A cancellation inside the last file ends the loop without a next iteration
	// to notice it, so the report must not be finalised as if it completed.
	if cerr := ctx.Err(); cerr != nil {
		report.Summary = summarise(report.Files, len(job.Selection))
		report.Error = "cancelled: " + cerr.Error()
		return report, fmt.Errorf("recheck: %w", cerr)
	}
	report.Summary = summarise(report.Files, len(job.Selection))
	report.Verified = report.Summary.Match == report.Summary.Selected && report.Summary.Selected > 0
	if !report.Verified && report.Capture == nil {
		report.Detail = "no capture has been published for this job, so no selected-file digest was recorded; only a completed fetch can attest these bytes"
	}
	return report, nil
}

// captureReference reports the recorded capture without inspecting it.
func captureReference(root string, job *jobRecord) *RecheckCapture {
	if job.Capture == nil || job.Capture.Key == "" && job.Capture.Manifest == "" {
		return nil
	}
	ref := &RecheckCapture{
		Key:         job.Capture.Key,
		Manifest:    job.Capture.Manifest,
		CommittedAt: job.Capture.CommittedAt,
		Reconciled:  job.Capture.Reconciled,
	}
	if job.Capture.Manifest != "" {
		if full, err := joinWithin(root, job.Capture.Manifest); err == nil {
			if fi, serr := os.Stat(full); serr == nil && fi.Mode().IsRegular() {
				ref.Present = true
			}
		}
	}
	return ref
}

// recheckFile is the whole check for one selected path.
func recheckFile(ctx context.Context, root string, job *jobRecord, rel string) RecheckFile {
	res := RecheckFile{Path: rel}

	// The attestation first: without one, the file cannot be verified no matter
	// what the bytes say.
	attested, detail := readAttestation(root, job, rel)
	if attested == nil {
		res.Status = RecheckUnverifiable
		res.Detail = detail
	}

	// Lexical containment, then a non-following stat. A recorded path is an
	// attestation of relative location, never a licence to read anywhere.
	full, err := joinWithin(root, rel)
	if err != nil {
		res.Status = RecheckError
		res.Detail = "recorded path does not resolve inside the job root"
		return res
	}
	fi, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		if _, perr := os.Lstat(full + ".part"); perr == nil {
			res.Status = RecheckPartial
			res.Detail = "no finalized file; an inspected .part is present, so the job looks interrupted and nothing was hashed"
			return res
		}
		res.Status = RecheckMissing
		res.Detail = "no finalized file and no .part at this path"
		return res
	}
	if err != nil {
		res.Status = RecheckError
		res.Detail = "stat failed: " + err.Error()
		return res
	}
	if !fi.Mode().IsRegular() {
		// A symlink or device at a selected path is refused, not followed.
		res.Status = RecheckError
		res.Detail = fmt.Sprintf("not a regular file (%s)", fi.Mode().Type())
		return res
	}
	res.Bytes = fi.Size()

	// Now that it exists and is regular, re-resolve with symlink evaluation so the
	// bytes hashed are the bytes inside the root.
	real, err := resolveCaptured(root, rel)
	if err != nil {
		res.Status = RecheckError
		res.Detail = "path resolves outside the job root"
		return res
	}
	sum, size, err := hashFileContext(ctx, real)
	if err != nil {
		if ctx.Err() != nil {
			res.Status = RecheckError
			res.Detail = "hashing cancelled"
			return res
		}
		res.Status = RecheckError
		res.Detail = "read failed: " + err.Error()
		return res
	}
	res.Bytes = size
	res.SHA256 = sum

	if attested == nil {
		return res
	}
	res.AttestedSHA256 = attested.Sha256
	res.AttestedBytes = attested.ByteSize
	if equalHex(sum, attested.Sha256) {
		res.Status = RecheckMatch
		return res
	}
	res.Status = RecheckMismatch
	if attested.ByteSize != 0 && attested.ByteSize != size {
		res.Detail = fmt.Sprintf("digest differs and the length changed (%d bytes now, %d attested)", size, attested.ByteSize)
	} else {
		res.Detail = "digest differs at the same length"
	}
	return res
}

// readAttestation finds the capture item for one selected path. Items are keyed by
// `<identity>:<staging-relative path>` and hold the digest recorded at capture
// time, which is the only per-file attestation the adapter keeps.
func readAttestation(root string, job *jobRecord, rel string) (*manifest.CaptureItem, string) {
	externalID := job.identityLabel() + ":" + rel
	path := manifest.ItemPath(root, externalID)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "no capture item records a digest for this path"
	}
	if err != nil {
		return nil, "capture item could not be read"
	}
	var item manifest.CaptureItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, "capture item is not valid JSON"
	}
	if item.Sha256 == "" {
		return nil, "capture item carries no digest"
	}
	if item.DownloadedPath != "" && item.DownloadedPath != rel {
		return nil, "capture item describes a different path"
	}
	if item.ExternalID != "" && item.ExternalID != externalID {
		return nil, "capture item belongs to a different identity"
	}
	return &item, ""
}

// hashFileContext streams one file through SHA-256, observing ctx between chunks.
// It replaces a plain io.Copy so a cancellation interrupts a long hash, and it
// re-checks after open that the thing opened is still a regular file.
func hashFileContext(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	h := sha256.New()
	buf := make([]byte, recheckChunkSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			if recheckHashChunkHook != nil {
				recheckHashChunkHook()
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", total, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func summarise(files []RecheckFile, selected int) RecheckSummary {
	sum := RecheckSummary{Selected: selected}
	for _, f := range files {
		switch f.Status {
		case RecheckMatch:
			sum.Match++
		case RecheckMismatch:
			sum.Mismatch++
		case RecheckMissing:
			sum.Missing++
		case RecheckPartial:
			sum.Partial++
		case RecheckUnverifiable:
			sum.Unverifiable++
		case RecheckError:
			sum.Error++
		}
	}
	return sum
}

// UnverifiedProblem renders the first reason a report is not verified, for a
// one-line diagnostic. It returns "" when the report is verified.
func (r RecheckReport) UnverifiedProblem() string {
	s := r.Summary
	switch {
	case s.Selected == 0:
		return "the job records no selected files"
	case s.Mismatch > 0:
		return fmt.Sprintf("%d of %d selected files no longer match their recorded digests", s.Mismatch, s.Selected)
	case s.Missing > 0:
		return fmt.Sprintf("%d of %d selected files are missing", s.Missing, s.Selected)
	case s.Partial > 0:
		return fmt.Sprintf("%d of %d selected files exist only as a partial", s.Partial, s.Selected)
	case s.Unverifiable > 0:
		return fmt.Sprintf("%d of %d selected files have no recorded digest to verify against", s.Unverifiable, s.Selected)
	case s.Error > 0:
		return fmt.Sprintf("%d of %d selected files could not be checked", s.Error, s.Selected)
	}
	return ""
}
