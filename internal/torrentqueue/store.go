// Package torrentqueue is the durable local queue for runner-managed torrent
// jobs, operated by a foreground runner with one active worker.
package torrentqueue

// Layout, all relative to one job root:
//
//	.provenance/torrent/queue/entries/<entryID>.json      public entry record
//	.provenance/torrent/queue/restricted/<entryID>.json   raw source + peers, 0600
//	.provenance/torrent/queue/store.lock                  store read-modify-write lock
//	.provenance/torrent/queue/runner.lock                 runner ownership (one runner)
//	.provenance/torrent/jobs/<jobID>/…                    the workspace (BT-05)
//
// The entry id is queued-<seq>-<rand> and never a torrent hash: a btmh magnet can
// resolve to a hybrid whose canonical workspace is bt-<v1>, so an entry has to
// exist before (and independently of) any torrent identity. JobID, Workspace and
// the hashes are attached only after a validated resolution.
//
// Secrets: the public record holds an identity-only magnet (xt entries, never
// trackers, web seeds or peer hints). The original argument and any explicit
// peers go to the restricted record, which is 0600 and is never printed by list
// or status.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/filelock"
)

// EntryFormat identifies the entry record so a reader can refuse a newer one.
const EntryFormat = "provenance/torrent-queue-entry+1"

const (
	queueDirName      = ".provenance/torrent/queue"
	entriesDirName    = ".provenance/torrent/queue/entries"
	restrictedDirName = ".provenance/torrent/queue/restricted"
)

// Desired is what the user asked for. It is separate from Phase, which is only
// ever what the runner last observed.
type Desired string

const (
	// DesiredPaused means the entry must not run until an explicit start.
	DesiredPaused Desired = "paused"
	// DesiredRun means the runner may pick the entry up.
	DesiredRun Desired = "run"
)

// Phase is the last observed lifecycle position, recorded - never inferred live.
type Phase string

const (
	// PhaseQueued: waiting for an explicit start, or for a runner.
	PhaseQueued Phase = "queued"
	// PhaseResolving: metadata exchange in progress (identity not yet known).
	PhaseResolving Phase = "resolving"
	// PhaseRunning: the worker holds the job.
	PhaseRunning Phase = "running"
	// PhaseStopping: a pause or shutdown was requested; the client may still be
	// closing and ownership may still be held.
	PhaseStopping Phase = "stopping"
	// PhasePaused: stopped by explicit request.
	PhasePaused Phase = "paused"
	// PhaseCompleted: a capture is published (captured from this or an earlier run).
	PhaseCompleted Phase = "completed"
	// PhaseFailed: the last attempt failed; needs retry.
	PhaseFailed Phase = "failed"
	// PhaseBlocked: a retryable condition (busy workspace, no peers yet) with a
	// bounded backoff, so the runner neither hot-loops nor gives up.
	PhaseBlocked Phase = "blocked"
)

// Source is the public description of what was enqueued.
type Source struct {
	// Kind is "magnet" or "file".
	Kind string `json:"kind"`
	// Magnet is identity-only: it never carries trackers, web seeds or peers.
	Magnet string `json:"magnet,omitempty"`
	// Path is a local .torrent path. A path is not a credential, but it stays out
	// of status output, where it would leak the operator's filesystem layout.
	Path string `json:"path,omitempty"`
}

// CaptureRef is the capture a completed entry points at.
type CaptureRef struct {
	Key         string    `json:"key"`
	Manifest    string    `json:"manifest"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
	Reconciled  bool      `json:"reconciled,omitempty"`
}

// Entry is one durable queue record.
type Entry struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	Seq       uint64    `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Desired is user intent. Phase is the last observation. They are recorded
	// separately so a restart can never mistake an interruption for a pause.
	Desired Desired `json:"desired"`
	Phase   Phase   `json:"phase"`

	Source    Source   `json:"source"`
	Discovery string   `json:"discovery"`
	Selection []string `json:"selection"`

	// JobID, hashes and Workspace are attached only after a validated resolution.
	JobID      string `json:"job_id,omitempty"`
	InfoHash   string `json:"info_hash,omitempty"`
	InfoHashV2 string `json:"info_hash_v2,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	// Unresolved records that this entry still needs metadata before it can name
	// a workspace. A resolved entry can always be resumed offline.
	Unresolved bool `json:"unresolved,omitempty"`

	Capture  *CaptureRef `json:"capture,omitempty"`
	Attempts int         `json:"attempts,omitempty"`
	// LastError is a redacted, actionable reason. It never holds a tracker URL,
	// a passkey or an absolute path.
	LastError string `json:"last_error,omitempty"`
	// NotBefore is a bounded backoff for a blocked entry.
	NotBefore time.Time `json:"not_before,omitempty"`
	// Note carries a short reconciliation remark, such as "interrupted by runner
	// shutdown" or "adopted capture published before the queue updated".
	Note string `json:"note,omitempty"`
}

// Store is the queue on disk. All methods are safe to call from several
// processes: every mutation is a read-modify-write under the store lock, and each
// record is published by atomic rename.
type Store struct {
	root string
}

// AddRequest is one submission, already prepared by the caller.
type AddRequest struct {
	// Source is the sanitized public form; RawSource and Peers are restricted.
	Source     Source
	RawSource  string
	Peers      []string
	Discovery  string
	Selection  []string
	JobID      string
	InfoHash   string
	InfoHashV2 string
	Workspace  string
	Unresolved bool
}

// OpenStore prepares the queue directories under root. It creates no entry and
// takes no lock; the root must already exist.
func OpenStore(root string) (*Store, error) {
	abs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	fi, serr := os.Stat(abs)
	if serr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("queue root %s is not a directory", abs)
	}
	for _, dir := range []string{entriesDirName, restrictedDirName} {
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o755); err != nil {
			return nil, fmt.Errorf("create queue directory: %w", err)
		}
	}
	return &Store{root: abs}, nil
}

// Root is the job root the queue belongs to.
func (s *Store) Root() string { return s.root }

func (s *Store) entriesDir() string { return filepath.Join(s.root, filepath.FromSlash(entriesDirName)) }
func (s *Store) restrictedDir() string {
	return filepath.Join(s.root, filepath.FromSlash(restrictedDirName))
}

// lock takes the store lock. It must never be held while awaiting peers,
// transferring, hashing or closing a client: callers use it only around the short
// read-modify-write of one record.
func (s *Store) lock() (*filelock.Lock, error) {
	return filelock.TryAcquire(filepath.Join(s.root, filepath.FromSlash(queueDirName), "store"))
}

// Add creates an entry, or returns the existing one when the same capture intent
// is already queued. Duplicate detection is by canonical job id when it is known
// (so a btih magnet and a .torrent of the same torrent converge), and by sanitized
// source otherwise.
func (s *Store) Add(req AddRequest) (Entry, bool, error) {
	var zero Entry
	if len(req.Selection) == 0 {
		return zero, false, errors.New("a queue entry needs at least one selected path")
	}
	if _, err := time.ParseDuration("0s"); err != nil { // keep time imported for doc clarity
		return zero, false, err
	}
	if strings.TrimSpace(req.Source.Magnet) == "" && strings.TrimSpace(req.Source.Path) == "" {
		return zero, false, errors.New("a queue entry needs a magnet or a .torrent path")
	}

	l, err := s.lock()
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return zero, false, fmt.Errorf("%w: the queue store is locked by another process; retry", ErrStoreBusy)
		}
		return zero, false, err
	}
	defer func() { _ = l.Release() }()

	entries, err := s.list()
	if err != nil {
		return zero, false, err
	}
	key := duplicateKey(req)
	for _, e := range entries {
		if duplicateKeyOf(e) == key {
			return e, true, nil
		}
	}

	now := time.Now().UTC()
	var seq uint64
	for _, e := range entries {
		if e.Seq > seq {
			seq = e.Seq
		}
	}
	entry := Entry{
		Format:     EntryFormat,
		ID:         newEntryID(seq + 1),
		Seq:        seq + 1,
		CreatedAt:  now,
		UpdatedAt:  now,
		Desired:    DesiredPaused,
		Phase:      PhaseQueued,
		Source:     req.Source,
		Discovery:  req.Discovery,
		Selection:  append([]string(nil), req.Selection...),
		JobID:      req.JobID,
		InfoHash:   req.InfoHash,
		InfoHashV2: req.InfoHashV2,
		Workspace:  req.Workspace,
		Unresolved: req.Unresolved,
	}
	if err := s.writeEntry(entry); err != nil {
		return zero, false, err
	}
	if err := s.writeRestricted(entry.ID, restrictedSource{RawSource: req.RawSource, Peers: req.Peers}); err != nil {
		return zero, false, err
	}
	return entry, false, nil
}

func duplicateKey(req AddRequest) string {
	if req.JobID != "" {
		return "job:" + req.JobID + "|" + strings.Join(sortedSelection(req.Selection), ",")
	}
	return "src:" + req.Source.Magnet + req.Source.Path + "|" + strings.Join(sortedSelection(req.Selection), ",")
}

func duplicateKeyOf(e Entry) string {
	if e.JobID != "" {
		return "job:" + e.JobID + "|" + strings.Join(sortedSelection(e.Selection), ",")
	}
	return "src:" + e.Source.Magnet + e.Source.Path + "|" + strings.Join(sortedSelection(e.Selection), ",")
}

func sortedSelection(sel []string) []string {
	out := append([]string(nil), sel...)
	sort.Strings(out)
	return out
}

func newEntryID(seq uint64) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable suffix would only weaken uniqueness, not safety; fall back
		// to the sequence alone rather than failing an enqueue on entropy.
		return fmt.Sprintf("queued-%06d", seq)
	}
	return fmt.Sprintf("queued-%06d-%s", seq, hex.EncodeToString(b[:]))
}

// List returns every entry in deterministic queue order (sequence, then id).
func (s *Store) List() ([]Entry, error) { return s.list() }

func (s *Store) list() ([]Entry, error) {
	dir := s.entriesDir()
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read queue: %w", err)
	}
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		entry, rerr := s.readEntry(filepath.Join(dir, item.Name()))
		if rerr != nil {
			return nil, rerr
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Seq != entries[j].Seq {
			return entries[i].Seq < entries[j].Seq
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

// Get returns one entry by id.
func (s *Store) Get(id string) (Entry, error) {
	entry, err := s.readEntry(s.entryPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
		}
		return Entry{}, err
	}
	return entry, nil
}

// Update applies mutate to one entry under the store lock and publishes the
// result atomically. mutate returns the entry to persist; returning an error
// aborts without writing.
func (s *Store) Update(id string, mutate func(*Entry) error) (Entry, error) {
	l, err := s.lock()
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return Entry{}, fmt.Errorf("%w: the queue store is locked by another process; retry", ErrStoreBusy)
		}
		return Entry{}, err
	}
	defer func() { _ = l.Release() }()

	entry, err := s.Get(id)
	if err != nil {
		return Entry{}, err
	}
	before := entry
	if err := mutate(&entry); err != nil {
		return Entry{}, err
	}
	// Format, id and sequence are identity, not state: never let a mutation move
	// an entry or renumber it.
	entry.Format = before.Format
	entry.ID = before.ID
	entry.Seq = before.Seq
	entry.CreatedAt = before.CreatedAt
	entry.UpdatedAt = time.Now().UTC()
	if err := s.writeEntry(entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// RestrictedSource returns the stored raw source and peers for an entry. It is
// only used by the runner (to resolve an entry) and by nothing that prints.
func (s *Store) RestrictedSource(id string) (raw string, peers []string, err error) {
	path := filepath.Join(s.restrictedDir(), id+".json")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return "", nil, fmt.Errorf("%w: %s has no restricted source record", ErrEntryNotFound, id)
		}
		return "", nil, rerr
	}
	var rec restrictedSource
	if uerr := json.Unmarshal(data, &rec); uerr != nil {
		return "", nil, fmt.Errorf("parse restricted record for %s: %w", id, uerr)
	}
	return rec.RawSource, rec.Peers, nil
}

type restrictedSource struct {
	RawSource string   `json:"raw_source"`
	Peers     []string `json:"peers,omitempty"`
}

func (s *Store) entryPath(id string) string {
	return filepath.Join(s.entriesDir(), id+".json")
}

func (s *Store) readEntry(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, fmt.Errorf("parse queue entry %s: %w", filepath.Base(path), err)
	}
	if entry.Format != EntryFormat {
		return Entry{}, fmt.Errorf("%w: %s has format %q, expected %q", ErrEntryFormat, filepath.Base(path), entry.Format, EntryFormat)
	}
	if entry.ID == "" || entry.Seq == 0 {
		return Entry{}, fmt.Errorf("%w: %s has no id or sequence", ErrEntryFormat, filepath.Base(path))
	}
	return entry, nil
}

func (s *Store) writeEntry(entry Entry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.entryPath(entry.ID), append(data, '\n'), 0o644)
}

func (s *Store) writeRestricted(id string, rec restrictedSource) error {
	if rec.RawSource == "" && len(rec.Peers) == 0 {
		return nil
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.restrictedDir(), id+".json"), append(data, '\n'), 0o600)
}

// writeFileAtomic publishes a file by rename, so a reader never sees a partial
// record and a crash leaves the previous version intact.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", filepath.Base(path), err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
