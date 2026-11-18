package bittorrent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
)

// JobFormatVersion identifies the on-disk job record. It is staging state, not
// archive state: the vault schema is untouched.
const JobFormatVersion = "provenance/torrent-job+1"

// torrentStateDir holds the adapter's own working files inside staging:
// metainfo, the job record, and (through the library) the piece-completion
// database. Captures reference only downloaded files, never these.
const torrentStateDir = ".provenance/torrent"

// jobRecord is the durable description of one torrent job. It is deliberately
// tiny: backend, identity, the immutable selection, and a staging reference.
// It carries no tracker URLs, passkeys, or peer addresses.
type jobRecord struct {
	Format     string    `json:"format"`
	JobID      string    `json:"job_id"`
	Backend    string    `json:"backend"`
	InfoHash   string    `json:"info_hash"`
	Source     string    `json:"source"`
	StagingRef string    `json:"staging_ref"`
	Selection  []string  `json:"selection"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// jobID derives a stable identifier from the infohash alone, so a restart of
// the same torrent is the same job rather than a new one.
func jobID(infoHash string) string { return "bt-" + infoHash }

func (s *Session) statePath(name string) string {
	return filepath.Join(s.staging, filepath.FromSlash(torrentStateDir), name)
}

func (s *Session) metainfoPath(ih metainfo.Hash) string {
	return s.statePath(ih.HexString() + ".torrent")
}

func (s *Session) jobPath(ih string) string {
	return s.statePath(jobID(ih) + ".job.json")
}

// recordJob persists the job, or reloads it and enforces that the selection of
// an existing job cannot change. Changing what a job means halfway through
// would make "resume" ambiguous, so a different selection needs a new staging
// root.
func (s *Session) recordJob(o *opened, selection []string) (*jobRecord, error) {
	path := s.jobPath(o.info.InfoHash)
	want := normalizeSelection(selection)

	if data, err := os.ReadFile(path); err == nil {
		var existing jobRecord
		if uerr := json.Unmarshal(data, &existing); uerr != nil {
			return nil, fmt.Errorf("read job record: %w", uerr)
		}
		if !equalSelection(existing.Selection, want) {
			return nil, fmt.Errorf("%w: job %s already selected %v, refusing to change it to %v; use a new staging directory",
				ErrSelectionLocked, existing.JobID, existing.Selection, want)
		}
		existing.Format = JobFormatVersion
		existing.StagingRef = o.info.Name
		existing.UpdatedAt = time.Now().UTC()
		if err := writeJob(path, &existing); err != nil {
			return nil, err
		}
		return &existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read job record: %w", err)
	}

	now := time.Now().UTC()
	job := &jobRecord{
		Format:     JobFormatVersion,
		JobID:      jobID(o.info.InfoHash),
		Backend:    Backend,
		InfoHash:   o.info.InfoHash,
		Source:     o.src.canonical(),
		StagingRef: o.info.Name,
		Selection:  want,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := writeJob(path, job); err != nil {
		return nil, err
	}
	return job, nil
}

func writeJob(path string, job *jobRecord) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job record: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write job record: %w", err)
	}
	return nil
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
