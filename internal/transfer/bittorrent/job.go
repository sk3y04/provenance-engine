package bittorrent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
	"github.com/sk3y04/provenance-engine/internal/manifest"
)

const (
	// JobFormatVersion identifies the current on-disk job record. It is
	// staging state, not archive state: the vault schema is untouched.
	JobFormatVersion = "provenance/torrent-job+2"

	// jobFormatV1 is the previous record version. It is still readable so a job
	// started by an older build resumes instead of starting over; it is
	// rewritten as v2 on the next touch.
	jobFormatV1 = "provenance/torrent-job+1"

	// torrentStateDir holds the adapter's own working files inside the job
	// root: metainfo, the job record and (through the library) the piece
	// checkpoints and part files.
	torrentStateDir = ".provenance/torrent"
)

// JobState models the three things a torrent job can be doing, separately.
//
// They are independent on purpose: files can be verified and ready while the
// capture is still pending (a capture failure leaves the payload reusable), and
// a capture can be committed while the client keeps seeding. Collapsing them
// into one status is what makes crash recovery guess.
type JobState struct {
	// Payload is PayloadPending until every selected file is hash-verified
	// complete, then PayloadReady.
	Payload string `json:"payload"`
	// Capture is CapturePending until a capture manifest is committed, then
	// CaptureCommitted.
	Capture string `json:"capture"`
	// Seeding is SeedingStopped unless the job is opted into seeding after
	// capture (Config.KeepSeeding), when it is SeedingActive.
	Seeding string `json:"seeding"`
}

// Job state values.
const (
	PayloadPending   = "pending"
	PayloadReady     = "ready"
	CapturePending   = "pending"
	CaptureCommitted = "committed"
	SeedingStopped   = "stopped"
	SeedingActive    = "seeding"
)

// resumeRef records how to reopen backend resume state. The backend owns piece
// checkpoints (.torrent.db) and part files; this adapter deliberately does not
// keep a second copy of that information, only the pointers to it.
type resumeRef struct {
	// MetainfoPath is the exact saved metainfo, relative to the job root.
	MetainfoPath string `json:"metainfo_path"`
	// DataDir is where the backend keeps payload and checkpoints, relative to
	// the job root. "." means the job root itself.
	DataDir string `json:"data_dir"`
	// PartFiles records that resume relies on the backend's part-file scheme
	// (data lands in <name>.part and is renamed on completion).
	PartFiles bool `json:"part_files"`
}

// payloadRecord is what verification produced: the selected, complete files
// with their Provenance-owned SHA-256 digests. The infohash lives on the job,
// never mixed into the artifact digest.
type payloadRecord struct {
	VerifiedAt time.Time `json:"verified_at"`
	Assets     []Asset   `json:"assets"`
}

// captureRecord is the committed capture. Key is the idempotency key, not a
// timestamp: the same job and the same staged bytes always produce the same
// key, so a restart can find the existing capture.
type captureRecord struct {
	Key         string    `json:"key"`
	Manifest    string    `json:"manifest"`
	CommittedAt time.Time `json:"committed_at"`
	// Reconciled marks a capture that was adopted from an existing manifest
	// rather than written by the run that recorded it.
	Reconciled bool `json:"reconciled,omitempty"`
}

// jobRecord is the durable description of one torrent job.
//
// It holds identity, the immutable selection, pointers into backend resume
// state and the three independently-tracked states above. It carries no
// tracker URLs, passkeys, peer addresses, or absolute local paths.
type jobRecord struct {
	Format  string `json:"format"`
	JobID   string `json:"job_id"`
	Backend string `json:"backend"`
	// InfoHash is the full v1 SHA-1 infohash, or empty for a v2-only job. Its
	// meaning is unchanged: a v1 torrent always records its v1 hash here.
	InfoHash string `json:"info_hash"`
	// InfoHashV2 is the full v2 SHA-256 infohash, recorded only when the
	// torrent has a v2 identity (hybrid or v2-only). It is a separate field so
	// nothing has to guess which 20 bytes of a 32-byte hash are "the" hash.
	InfoHashV2 string         `json:"info_hash_v2,omitempty"`
	Source     string         `json:"source"`
	StagingRef string         `json:"staging_ref"`
	Selection  []string       `json:"selection"`
	Resume     resumeRef      `json:"resume"`
	State      JobState       `json:"state"`
	Payload    *payloadRecord `json:"payload,omitempty"`
	Capture    *captureRecord `json:"capture,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`

	// fileFormat is the format string as found on disk, before any in-memory
	// upgrade. Status reports it so a v1 record is not misreported as v2. It is
	// never serialised.
	fileFormat string
}

// formatLabel is the on-disk format of the record, for read-only reporting.
func (j *jobRecord) formatLabel() string {
	if j.fileFormat != "" {
		return j.fileFormat
	}
	return j.Format
}

// enrichJobIdentity records a v2 identity that metadata revealed for a job first
// named by a narrower source (btih or btmh magnet for a hybrid). It never
// replaces a recorded hash and never changes the job id: a job created from a
// v2-only source keeps its bt2- id, and one created from btih keeps bt-<v1>.
func (s *Session) enrichJobIdentity(job *jobRecord, got Identity) error {
	current, err := identityFromJob(job)
	if err != nil {
		return err
	}
	if !got.Compare(current) {
		return fmt.Errorf("job %s records %s but the metadata resolved %s", job.JobID, current, got)
	}
	if got.HasV2 && job.InfoHashV2 == "" {
		job.InfoHashV2 = got.V2Hex()
		return s.saveJob(job)
	}
	return nil
}

// jobIdentity derives the identity a job record describes.
func jobIdentity(job *jobRecord) Identity {
	id, _ := identityFromJob(job)
	return id
}

// identityLabel is the hex hash used in external ids (capture items). v1 jobs
// keep the v1 hash; a v2-only job uses its full v2 hash, which is unambiguous by
// length.
func (j *jobRecord) identityLabel() string {
	if j.InfoHash != "" {
		return j.InfoHash
	}
	return j.InfoHashV2
}

func (s *Session) statePath(name string) string {
	return filepath.Join(s.staging, filepath.FromSlash(torrentStateDir), name)
}

func (s *Session) metainfoPath(id Identity) string {
	return s.statePath(id.MetainfoFile())
}

func (s *Session) jobPath(jobID string) string {
	return s.statePath(jobID + ".job.json")
}

// metainfoRel is the job-root-relative metainfo path recorded in a job.
func metainfoRel(name string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Join(torrentStateDir, name+".torrent")), "./")
}

// metainfoRelFor is the metainfo path a job with this identity records.
func metainfoRelFor(id Identity) string {
	return metainfoRel(strings.TrimSuffix(id.MetainfoFile(), ".torrent"))
}

// ErrIdentityConflict means another job in this staging root would share this
// torrent's payload or completion state. The library keys piece completion by a
// 20-byte infohash and stores payload under the torrent's name, so two different
// identities can point at the same bytes; the adapter refuses that instead of
// silently sharing them.
var ErrIdentityConflict = errors.New("staging root already holds a different torrent job")

// checkIdentityConflict rejects a second identity that maps onto the same
// library piece-completion key in this root.
func (s *Session) checkIdentityConflict(id Identity) error {
	entries, err := os.ReadDir(s.statePath(""))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read job directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".job.json") || name == id.JobID()+".job.json" {
			continue
		}
		other, err := readJob(s.statePath(name))
		if err != nil {
			continue
		}
		otherID, err := identityFromJob(other)
		if err != nil || otherID.SameTorrent(id) {
			continue
		}
		if otherID.StorageKey() == id.StorageKey() {
			return fmt.Errorf("%w: job %s and %s would share the library's piece-completion key; use a separate staging root",
				ErrIdentityConflict, other.JobID, id.JobID())
		}
	}
	return nil
}

// checkPayloadDirConflict rejects a second identity that would write into the
// same payload directory. It runs once the torrent name is known, because that
// name is what the library uses below its data directory.
func (s *Session) checkPayloadDirConflict(id Identity, stagingRef string) error {
	entries, err := os.ReadDir(s.statePath(""))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read job directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".job.json") || name == id.JobID()+".job.json" {
			continue
		}
		other, err := readJob(s.statePath(name))
		if err != nil {
			continue
		}
		otherID, err := identityFromJob(other)
		if err != nil || otherID.SameTorrent(id) {
			continue
		}
		if other.StagingRef != "" && other.StagingRef == stagingRef {
			return fmt.Errorf("%w: job %s already writes payload to %q; use a separate staging root",
				ErrIdentityConflict, other.JobID, stagingRef)
		}
	}
	return nil
}

// loadOrCreateJob reads the job record for a source, creating it on first use,
// and enforces that an existing job's selection cannot change. The record is
// written before any payload work, so an interrupted first run still leaves a
// durable job behind.
func (s *Session) loadOrCreateJob(id Identity, want []string) (*jobRecord, error) {
	path := s.jobPath(id.JobID())
	job, err := readJob(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if job != nil {
		if err := s.validateJob(job, id); err != nil {
			return nil, err
		}
		if !equalSelection(job.Selection, want) {
			return nil, fmt.Errorf("%w: job %s already selected %v, refusing to change it to %v; use a new staging directory",
				ErrSelectionLocked, job.JobID, job.Selection, want)
		}
		job.Format = JobFormatVersion
		job.Source = id.Source()
		return job, s.saveJob(job)
	}
	if err := s.checkIdentityConflict(id); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	job = &jobRecord{
		Format:     JobFormatVersion,
		JobID:      id.JobID(),
		Backend:    Backend,
		InfoHash:   id.V1Hex(),
		InfoHashV2: id.V2Hex(),
		Source:     id.Source(),
		Selection:  want,
		Resume: resumeRef{
			MetainfoPath: metainfoRelFor(id),
			DataDir:      ".",
			PartFiles:    true,
		},
		State: JobState{
			Payload: PayloadPending,
			Capture: CapturePending,
			Seeding: SeedingStopped,
		},
		CreatedAt: now,
	}
	return job, s.saveJob(job)
}

// validateJob refuses a record that does not describe this torrent in this
// staging root. The record's paths are treated like any other untrusted input:
// they must stay inside the job root.
func (s *Session) validateJob(job *jobRecord, want Identity) error {
	return validateJobRecord(s.staging, job, want)
}

// validateJobRecord is the Session-free form of the same validation, so a
// read-only caller (torrent status) can check a record without constructing a
// Session. It only fills defaults in memory and never touches the filesystem.
func validateJobRecord(staging string, job *jobRecord, want Identity) error {
	got, err := identityFromJob(job)
	if err != nil {
		return err
	}
	if !got.Compare(want) {
		return fmt.Errorf("job record %s describes %s, not %s", job.JobID, got, want)
	}
	if job.JobID != got.JobID() {
		return fmt.Errorf("job record id %q does not match its identity %s", job.JobID, got)
	}
	if job.Resume.MetainfoPath == "" {
		job.Resume.MetainfoPath = metainfoRelFor(got)
	}
	if _, err := joinWithin(staging, job.Resume.MetainfoPath); err != nil {
		return err
	}
	if job.Resume.MetainfoPath != metainfoRelFor(got) {
		return fmt.Errorf("%w: job record points at metainfo %q, expected %q",
			ErrUnsafePath, job.Resume.MetainfoPath, metainfoRelFor(got))
	}
	if job.Resume.DataDir == "" {
		job.Resume.DataDir = "."
	}
	if _, err := joinWithin(staging, job.Resume.DataDir); err != nil {
		return err
	}
	if job.State.Payload == "" {
		job.State.Payload = PayloadPending
	}
	if job.State.Capture == "" {
		job.State.Capture = CapturePending
	}
	if job.State.Seeding == "" {
		job.State.Seeding = SeedingStopped
	}
	return nil
}

// readJob loads a v2 record, or upgrades a v1 record in memory. A v1 record has
// no state or resume pointers, so it comes back as "nothing verified yet" and
// the normal flow re-derives both.
func readJob(path string) (*jobRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var job jobRecord
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("read job record: %w", err)
	}
	job.fileFormat = job.Format
	switch job.Format {
	case JobFormatVersion:
	case jobFormatV1, "":
		job.Format = JobFormatVersion
		job.Resume = resumeRef{MetainfoPath: metainfoRel(strings.ToLower(job.InfoHash)), DataDir: ".", PartFiles: true}
		job.State = JobState{Payload: PayloadPending, Capture: CapturePending, Seeding: SeedingStopped}
		job.Payload = nil
		job.Capture = nil
	default:
		return nil, fmt.Errorf("job record %s has unsupported format %q", path, job.Format)
	}
	if job.Selection == nil {
		job.Selection = []string{}
	}
	return &job, nil
}

func (s *Session) saveJob(job *jobRecord) error {
	job.UpdatedAt = time.Now().UTC()
	if job.Selection == nil {
		job.Selection = []string{}
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job record: %w", err)
	}
	if err := atomicfile.WriteFile(s.jobPath(job.JobID), data, 0o600); err != nil {
		return fmt.Errorf("write job record: %w", err)
	}
	return nil
}

// markPayloadReady records the verified selection. Callers pass assets sorted
// by path so the capture key is stable.
func (s *Session) markPayloadReady(job *jobRecord, assets []Asset) error {
	sorted := append([]Asset(nil), assets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	job.Payload = &payloadRecord{VerifiedAt: time.Now().UTC(), Assets: sorted}
	job.State.Payload = PayloadReady
	return s.saveJob(job)
}

// markPayloadPending drops back to unverified. Used when staged bytes no longer
// match the record, so the next run re-verifies and repairs instead of trusting
// a stale attestation.
func (s *Session) markPayloadPending(job *jobRecord) error {
	job.State.Payload = PayloadPending
	return s.saveJob(job)
}

// markCaptureCommitted records the committed capture against the job.
func (s *Session) markCaptureCommitted(job *jobRecord, key, manifestPath string, reconciled bool) error {
	job.Capture = &captureRecord{
		Key:         key,
		Manifest:    manifestPath,
		CommittedAt: time.Now().UTC(),
		Reconciled:  reconciled,
	}
	job.State.Capture = CaptureCommitted
	return s.saveJob(job)
}

// markSeeding records whether the client is still available after capture.
func (s *Session) markSeeding(job *jobRecord, state string) error {
	if job.State.Seeding == state {
		return nil
	}
	job.State.Seeding = state
	return s.saveJob(job)
}

// captureKey is the idempotency key for a job's capture: job identity plus the
// identity of the selected outputs. It deliberately excludes wall-clock time,
// the run manifest path and any freshly generated id, so a restart after a
// committed capture computes exactly the same key and can find the existing
// capture instead of recording a duplicate.
func captureKey(id string, assets []Asset) string {
	sorted := append([]Asset(nil), assets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "provenance/torrent-capture+1\njob=%s\nfiles=%d\n", id, len(sorted))
	for _, a := range sorted {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\n", a.Path, a.Length, strings.ToLower(a.SHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// findCommittedCapture looks for an existing capture manifest carrying key.
//
// The capture manifest is the commit point: it is the artifact archive import
// reads. Scanning for the key (rather than for a timestamp or a run id) is what
// makes "crashed after commit, before job update" recoverable.
func (s *Session) findCommittedCapture(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	runs, err := manifest.ListRunManifests(s.staging)
	if err != nil {
		return "", false
	}
	for _, run := range runs {
		cm, err := manifest.ReadCaptureManifest(run)
		if err != nil {
			continue
		}
		if cm.CaptureKey != key {
			continue
		}
		rel, err := filepath.Rel(s.staging, run)
		if err != nil {
			continue
		}
		return filepath.ToSlash(rel), true
	}
	return "", false
}

// adoptCapture turns an existing committed capture into a result without
// writing a new manifest, and reconciles the job record if it had not caught up.
func (s *Session) adoptCapture(job *jobRecord, key, manifestPath string, bytesRead int64, warnings []string) (Capture, error) {
	assets := []Asset{}
	if job.Payload != nil {
		assets = job.Payload.Assets
	}
	alreadyRecorded := job.State.Capture == CaptureCommitted && job.Capture != nil &&
		job.Capture.Key == key && job.Capture.Manifest == manifestPath
	if !alreadyRecorded {
		if err := s.markCaptureCommitted(job, key, manifestPath, true); err != nil {
			return Capture{}, err
		}
	}
	return s.captureResult(job, assets, key, manifestPath, bytesRead, warnings, true), nil
}

// normalizeSelection returns a sorted, de-duplicated copy so that the same
// set of files always produces the same record.
func normalizeSelection(selection []string) []string {
	seen := make(map[string]struct{}, len(selection))
	out := make([]string, 0, len(selection))
	for _, s := range selection {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func equalSelection(a, b []string) bool {
	na, nb := normalizeSelection(a), normalizeSelection(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}
