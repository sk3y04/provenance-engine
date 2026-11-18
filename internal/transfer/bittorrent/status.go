package bittorrent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// Read-only job inspection.
//
// Everything here is deliberately independent of the torrent backend: no
// Session, no staging lock, no client, no .torrent.db, no payload hashing, no
// directory creation, and no rewrite of a v1 record. A reader only opens job
// records and projects the facts they contain.
//
// A job record is published with an atomic rename (unique temp file in the same
// directory, then rename), so a reader of one file sees either the complete old
// record or the complete new one. Nothing here reads two files and presents
// them as one snapshot: the capture manifest path is reported as recorded, not
// re-checked, because the record and the manifest are separate files that a
// writer may replace at different times.

// Sentinel errors for status lookups. Callers match them with errors.Is.
var (
	// ErrJobNotFound means no record exists for that job id.
	ErrJobNotFound = errors.New("torrent job not found")
	// ErrInvalidJobID means the requested id is not a job identity. Ids are
	// validated (bt-<40 hex> or a bare 40-hex infohash) and never treated as
	// filesystem paths.
	ErrInvalidJobID = errors.New("invalid torrent job id")
)

// PayloadStatus describes the last recorded payload attestation.
type PayloadStatus struct {
	VerifiedAt time.Time `json:"verified_at"`
	// Files is len(Assets), kept for callers that only need the count.
	Files int `json:"files"`
	// Assets is the recorded digest of each selected file, as the writer
	// attested it. It is reported, not rechecked: status never reads payload.
	Assets []Asset `json:"assets,omitempty"`
}

// CaptureStatus describes the last recorded capture commit.
type CaptureStatus struct {
	CommittedAt time.Time `json:"committed_at"`
	// Manifest is the capture manifest path relative to the staging root, as
	// recorded. It is not re-resolved or verified by status.
	Manifest string `json:"manifest"`
	// CaptureKey is the idempotency key recorded for the capture.
	CaptureKey string `json:"capture_key"`
	// Reconciled is true when the recorded capture was adopted from an
	// existing manifest rather than written by that run.
	Reconciled bool `json:"reconciled,omitempty"`
}

// JobStatus is a read-only projection of one persisted job record.
//
// Every field is a *recorded* fact: what the last writer of that record knew
// when it wrote it. RecordedState is not a liveness signal — it can say
// "seeding" long after the process that was seeding died — and UpdatedAt is
// only when the record last changed.
type JobStatus struct {
	JobID         string         `json:"job_id"`
	Backend       string         `json:"backend"`
	InfoHash      string         `json:"info_hash"`
	StagingRef    string         `json:"staging_ref,omitempty"`
	Selection     []string       `json:"selection"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	RecordedState JobState       `json:"recorded_state"`
	Payload       *PayloadStatus `json:"payload,omitempty"`
	Capture       *CaptureStatus `json:"capture,omitempty"`
	RecordFormat  string         `json:"record_format"`
}

// UnreadableJob is a job record that could not be decoded or validated. A
// listing that contains one is incomplete and says so.
type UnreadableJob struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// JobListing is the result of listing a staging root's jobs.
type JobListing struct {
	// StagingRoot is the canonical absolute root the listing came from:
	// symlinks are resolved, so two spellings of one root agree.
	StagingRoot string `json:"staging_root"`
	// Complete is false when at least one record could not be read. It is the
	// in-band form of the non-zero exit code, so a script that only parses
	// stdout can tell a full listing from a partial one.
	Complete bool `json:"complete"`
	// Jobs is sorted by job id and never nil, so JSON shows [] rather than null.
	Jobs []JobStatus `json:"jobs"`
	// Unreadable is sorted by file name and omitted when empty.
	Unreadable []UnreadableJob `json:"unreadable,omitempty"`
}

// ListJobs reads every persisted job record under a staging root.
//
// It does not create, lock, or rewrite anything: an empty or
// never-used staging root returns an empty listing, and a staged root that does
// not exist is an error rather than a silent empty list.
func ListJobs(stagingDir string) (JobListing, error) {
	root, err := statusRoot(stagingDir)
	if err != nil {
		return JobListing{}, err
	}
	listing := JobListing{StagingRoot: root, Complete: true, Jobs: []JobStatus{}}

	dir := filepath.Join(root, filepath.FromSlash(torrentStateDir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing has been recorded in this root yet.
		return listing, nil
	}
	if err != nil {
		return JobListing{}, fmt.Errorf("read job directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".job.json") {
			continue
		}
		status, err := statusFromPath(root, filepath.Join(dir, entry.Name()))
		if err != nil {
			listing.Complete = false
			listing.Unreadable = append(listing.Unreadable, UnreadableJob{File: entry.Name(), Reason: err.Error()})
			continue
		}
		// The file name must agree with the identity inside the record, so a
		// planted or renamed record cannot masquerade as another job.
		if want := status.JobID + ".job.json"; entry.Name() != want {
			listing.Complete = false
			listing.Unreadable = append(listing.Unreadable, UnreadableJob{
				File:   entry.Name(),
				Reason: fmt.Sprintf("record identifies as %s but the file is named %q", status.JobID, entry.Name()),
			})
			continue
		}
		listing.Jobs = append(listing.Jobs, status)
	}

	sort.Slice(listing.Jobs, func(i, j int) bool { return listing.Jobs[i].JobID < listing.Jobs[j].JobID })
	sort.Slice(listing.Unreadable, func(i, j int) bool { return listing.Unreadable[i].File < listing.Unreadable[j].File })
	return listing, nil
}

// ReadJobStatus reads one job by validated id. The id is matched against the
// record's own identity; it is never used as a path.
func ReadJobStatus(stagingDir, rawID string) (JobStatus, error) {
	root, err := statusRoot(stagingDir)
	if err != nil {
		return JobStatus{}, err
	}
	id, ihHex, err := parseJobID(rawID)
	if err != nil {
		return JobStatus{}, err
	}
	path := filepath.Join(root, filepath.FromSlash(torrentStateDir), id+".job.json")
	job, err := readJobRecordFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return JobStatus{}, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	if err != nil {
		return JobStatus{}, err
	}
	status, err := projectJob(root, job)
	if err != nil {
		return JobStatus{}, err
	}
	if status.JobID != id || status.InfoHash != ihHex {
		return JobStatus{}, fmt.Errorf("job record %s does not describe %s", path, id)
	}
	return status, nil
}

// statusRoot resolves and checks the staging root without creating it. Status
// is read-only, so a missing root is an error the caller can act on rather than
// something to create.
func statusRoot(stagingDir string) (string, error) {
	staging := strings.TrimSpace(stagingDir)
	if staging == "" {
		return "", errors.New("bittorrent: a staging directory is required")
	}
	abs, err := filepath.Abs(staging)
	if err != nil {
		return "", fmt.Errorf("bittorrent: resolve staging dir: %w", err)
	}
	fi, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("staging root %s does not exist", abs)
	}
	if err != nil {
		return "", fmt.Errorf("inspect staging root: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("staging root %s is not a directory", abs)
	}
	// Resolve symlinks so a reader that reaches the root through a link reports
	// the same root as everyone else. This is a reporting detail only: status
	// takes no lock, so lock identity is not involved.
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return abs, nil
}

// statusFromPath reads and validates one record for a listing.
func statusFromPath(root, path string) (JobStatus, error) {
	job, err := readJobRecordFile(path)
	if err != nil {
		return JobStatus{}, err
	}
	return projectJob(root, job)
}

// readJobRecordFile decodes a record for read-only projection and refuses
// anything that is not a regular file: a symlink planted in the staging tree
// must not make status read outside the root. The writer's own reader is
// unchanged.
func readJobRecordFile(path string) (*jobRecord, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("inspect job record: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("job record %s is not a regular file", filepath.Base(path))
	}
	return readJob(path)
}

// projectJob validates a decoded record and reduces it to the status
// projection. Validation reuses the writer's rules; it only normalises the
// in-memory copy.
func projectJob(root string, job *jobRecord) (JobStatus, error) {
	job.InfoHash = strings.ToLower(job.InfoHash)
	ih, err := parseInfoHash(job.InfoHash)
	if err != nil {
		return JobStatus{}, fmt.Errorf("job record has an invalid infohash: %w", err)
	}
	if err := validateJobRecord(root, job, ih); err != nil {
		return JobStatus{}, err
	}

	status := JobStatus{
		JobID:         job.JobID,
		Backend:       job.Backend,
		InfoHash:      job.InfoHash,
		StagingRef:    job.StagingRef,
		Selection:     append([]string{}, job.Selection...),
		CreatedAt:     job.CreatedAt,
		UpdatedAt:     job.UpdatedAt,
		RecordedState: job.State,
		RecordFormat:  job.formatLabel(),
	}
	if job.StagingRef != "" {
		// The staging reference is a directory name. Validate it with the same
		// rule the writer applies, without echoing the value in the error: a
		// hand-edited record could put anything there, including a secret.
		if err := safeComponent(job.StagingRef); err != nil {
			return JobStatus{}, fmt.Errorf("%w: job record %s has an invalid staging reference", ErrUnsafePath, job.JobID)
		}
	}
	if job.Payload != nil {
		assets := make([]Asset, 0, len(job.Payload.Assets))
		assets = append(assets, job.Payload.Assets...)
		status.Payload = &PayloadStatus{
			VerifiedAt: job.Payload.VerifiedAt,
			Files:      len(assets),
			Assets:     assets,
		}
	}
	if job.Capture != nil {
		status.Capture = &CaptureStatus{
			CommittedAt: job.Capture.CommittedAt,
			Manifest:    job.Capture.Manifest,
			CaptureKey:  job.Capture.Key,
			Reconciled:  job.Capture.Reconciled,
		}
	}
	return status, nil
}

// parseJobID normalises a requested id. Accepted forms are the canonical job id
// ("bt-" plus the 40-hex v1 infohash) and the bare infohash. Anything else —
// paths, separators, traversal, partial ids — is rejected before any file is
// touched.
func parseJobID(raw string) (id, infohash string, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "bt-")
	if _, perr := parseInfoHash(s); perr != nil {
		return "", "", fmt.Errorf("%w: %q (want bt-<40 hex> or the bare infohash)", ErrInvalidJobID, raw)
	}
	return jobID(s), s, nil
}

// parseInfoHash decodes a v1 infohash without panicking on hostile input.
func parseInfoHash(s string) (metainfo.Hash, error) {
	var h metainfo.Hash
	if len(s) != metainfo.HashSize*2 {
		return h, fmt.Errorf("want %d hex characters", metainfo.HashSize*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}
