package slskd

// Durable download jobs.
//
// Layout, relative to one job root (normally the mapped download mount):
//
//	.provenance/slskd/jobs/<externalID>.json   one job record, 0600
//	.provenance/slskd/store.lock               read-modify-write lock
//
// A job record is the authoritative history of a Soulseek download: it
// outlives slskd's transfer retention, a `flags.volatile` restart, and a
// Provenance restart. slskd keeps only what it observes now, so nothing here may
// be derived from a slskd query after the fact.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
	"github.com/sk3y04/provenance-engine/internal/filelock"
)

// JobFormat identifies the job record so a reader can refuse a newer one.
const JobFormat = "provenance/slskd-job+1"

const (
	jobsDirName = ".provenance/slskd/jobs"
	lockName    = ".provenance/slskd/store.lock"
)

// JobStatus is the durable lifecycle position of a download job.
type JobStatus string

const (
	// JobQueued: persisted, enqueue request either in flight or accepted.
	JobQueued JobStatus = "QUEUED"
	// JobInProgress: slskd reports at least one transfer still moving or queued.
	JobInProgress JobStatus = "IN_PROGRESS"
	// JobCompletedPendingImport: bytes are on disk, ingestion has not run yet.
	JobCompletedPendingImport JobStatus = "COMPLETED_PENDING_IMPORT"
	// JobImported: a verified capture exists for every item.
	JobImported JobStatus = "IMPORTED"
	// JobFailed: every item failed at the transport layer.
	JobFailed JobStatus = "FAILED"
	// JobImportFailed: bytes arrived but could not be ingested; staging is kept.
	JobImportFailed JobStatus = "IMPORT_FAILED"
	// JobInterrupted: slskd no longer knows the job and no bytes are on disk
	// (a volatile restart). It can be re-enqueued in one action.
	JobInterrupted JobStatus = "INTERRUPTED"
	// JobCancelled: cancelled by the user.
	JobCancelled JobStatus = "CANCELLED"
)

// Terminal reports whether a status can never change without a new action.
func (s JobStatus) Terminal() bool {
	switch s {
	case JobImported, JobFailed, JobImportFailed, JobCancelled:
		return true
	}
	return false
}

// Item is one file inside a job.
type Item struct {
	RemoteFilename string `json:"remoteFilename"`
	Size           int64  `json:"size"`
	// TransferID is slskd's id for this transfer, once known.
	TransferID string `json:"transferId,omitempty"`
	// State is the last observed slskd state string, verbatim.
	State         string `json:"state,omitempty"`
	LocalPath     string `json:"localPath,omitempty"`
	Sha256        string `json:"sha256,omitempty"`
	ByteSize      int64  `json:"byteSize,omitempty"`
	CaptureItem   string `json:"captureItem,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	// Rejected marks a file slskd refused at enqueue time (a 207 partial
	// batch). Such a file never gets a transfer record, so without this marker
	// the job would stay IN_PROGRESS forever waiting for one.
	Rejected bool `json:"rejected,omitempty"`
}

// Candidate is an alternative source for the same content, kept so "try another
// source" does not need a fresh search.
type Candidate struct {
	Username          string  `json:"username"`
	Directory         string  `json:"directory"`
	Files             []File  `json:"files"`
	UploadSpeed       int     `json:"uploadSpeed"`
	HasFreeUploadSlot bool    `json:"hasFreeUploadSlot"`
	QueueLength       int     `json:"queueLength"`
	Score             float64 `json:"score"`
}

// Job is one durable download job: one peer, one batch, N files.
type Job struct {
	Format string `json:"format"`
	// ID is both the Provenance job id and the slskd externalId
	// ("prov-<hex>"), so a restart can correlate without extra state.
	ID         string `json:"id"`
	BatchID    string `json:"batchId,omitempty"`
	SearchID   string `json:"searchId,omitempty"`
	SearchText string `json:"searchText,omitempty"`
	Peer       string `json:"peer"`
	// DestinationSubdir is pinned at enqueue time ("provenance/<id>").
	DestinationSubdir string      `json:"destinationSubdir"`
	Status            JobStatus   `json:"status"`
	FailureReason     string      `json:"failureReason,omitempty"`
	CreatedAt         time.Time   `json:"createdAt"`
	UpdatedAt         time.Time   `json:"updatedAt"`
	Items             []Item      `json:"items"`
	Alternates        []Candidate `json:"alternates,omitempty"`
	// ImportRoot is the output directory the capture manifest was written to.
	ImportRoot string `json:"importRoot,omitempty"`
	// RevisionID is set when a vault ingest succeeded.
	RevisionID string `json:"revisionId,omitempty"`
	// CleanedUp records that the staging copy was deleted after a verified
	// import, so a retry does not delete something that is already gone.
	CleanedUp bool `json:"cleanedUp,omitempty"`
}

// Pending reports whether the job still needs the reconciler.
func (j Job) Pending() bool { return !j.Status.Terminal() }

// ActiveItem reports whether any item is still moving or not yet observed.
func (j Job) ActiveItem() bool {
	for _, it := range j.Items {
		if it.Rejected {
			continue
		}
		if it.State == "" {
			return true
		}
		if terminal, _ := transferOutcome(it.State); !terminal {
			return true
		}
	}
	return false
}

var jobIDPattern = regexp.MustCompile(`^prov-[0-9a-f]{16,64}$`)

// ValidJobID reports whether id is a well-formed job id. The check exists to keep
// a record path inside the jobs directory.
func ValidJobID(id string) bool { return jobIDPattern.MatchString(id) }

// NewJobID returns a random job id in the "prov-" namespace.
func NewJobID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return "prov-" + hex.EncodeToString(buf), nil
}

// JobStore is a directory of job records with a single read-modify-write lock.
type JobStore struct {
	root string
	now  func() time.Time
}

// OpenJobStore opens (creating if needed) the job store under root.
func OpenJobStore(root string) (*JobStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("job store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(jobsDirName)), 0o755); err != nil {
		return nil, fmt.Errorf("create job store: %w", err)
	}
	return &JobStore{root: abs, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Root returns the absolute job store root.
func (s *JobStore) Root() string { return s.root }

func (s *JobStore) jobsDir() string { return filepath.Join(s.root, filepath.FromSlash(jobsDirName)) }

func (s *JobStore) lock() (*filelock.Lock, error) {
	lockPath := filepath.Join(s.root, filepath.FromSlash(lockName))
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	return filelock.Acquire(context.Background(), lockPath)
}

// Create stores a new job. It refuses an id that already exists, so a retry
// always creates a new record instead of overwriting history.
func (s *JobStore) Create(job Job) (Job, error) {
	if !ValidJobID(job.ID) {
		return Job{}, fmt.Errorf("invalid job id %q", job.ID)
	}
	lk, err := s.lock()
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = lk.Release() }()

	if _, err := os.Stat(s.path(job.ID)); err == nil {
		return Job{}, fmt.Errorf("job %s already exists", job.ID)
	}
	now := s.now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = JobQueued
	}
	job.Format = JobFormat
	if job.DestinationSubdir == "" {
		job.DestinationSubdir = "provenance/" + job.ID
	}
	if err := s.write(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// Get returns one job.
func (s *JobStore) Get(id string) (Job, error) {
	if !ValidJobID(id) {
		return Job{}, fmt.Errorf("invalid job id %q", id)
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Job{}, err
	}
	return decodeJob(data)
}

// List returns every job, oldest first.
func (s *JobStore) List() ([]Job, error) {
	entries, err := os.ReadDir(s.jobsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	jobs := make([]Job, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.jobsDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		job, err := decodeJob(data)
		if err != nil {
			// A record this build cannot read is reported, not skipped: silently
			// dropping a job would lose the only history of those bytes.
			return nil, fmt.Errorf("read job record %s: %w", e.Name(), err)
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs, nil
}

// Update applies mutate to a job record under the store lock.
func (s *JobStore) Update(id string, mutate func(*Job) error) (Job, error) {
	if !ValidJobID(id) {
		return Job{}, fmt.Errorf("invalid job id %q", id)
	}
	lk, err := s.lock()
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = lk.Release() }()

	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Job{}, err
	}
	job, err := decodeJob(data)
	if err != nil {
		return Job{}, err
	}
	if err := mutate(&job); err != nil {
		return Job{}, err
	}
	job.Format = JobFormat
	job.UpdatedAt = s.now()
	if err := s.write(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// FindActiveItem reports whether a non-terminal job already covers this peer's
// file, which is how a duplicate grab attaches to the existing job instead of
// double-enqueueing.
func (s *JobStore) FindActiveItem(peer, remote string) (Job, bool, error) {
	jobs, err := s.List()
	if err != nil {
		return Job{}, false, err
	}
	for _, j := range jobs {
		if j.Status.Terminal() || !strings.EqualFold(j.Peer, peer) {
			continue
		}
		for _, it := range j.Items {
			if strings.EqualFold(it.RemoteFilename, remote) {
				return j, true, nil
			}
		}
	}
	return Job{}, false, nil
}

// Pending returns every job that still needs reconciliation.
func (s *JobStore) Pending() ([]Job, error) {
	jobs, err := s.List()
	if err != nil {
		return nil, err
	}
	out := jobs[:0]
	for _, j := range jobs {
		if j.Pending() {
			out = append(out, j)
		}
	}
	return out, nil
}

func (s *JobStore) path(id string) string {
	return filepath.Join(s.jobsDir(), id+".json")
}

func (s *JobStore) write(job Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job: %w", err)
	}
	if err := atomicfile.WriteFile(s.path(job.ID), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write job %s: %w", job.ID, err)
	}
	return nil
}

func decodeJob(data []byte) (Job, error) {
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, err
	}
	if job.Format == "" {
		job.Format = JobFormat
	}
	if job.Format != JobFormat {
		return Job{}, fmt.Errorf("job record format %q is not supported (want %q)", job.Format, JobFormat)
	}
	if !ValidJobID(job.ID) {
		return Job{}, fmt.Errorf("job record has invalid id %q", job.ID)
	}
	return job, nil
}
