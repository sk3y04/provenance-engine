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

// SeedDesired is what the user asked for. Only an explicit `queue seed` sets it.
type SeedDesired string

// SeedPhase is what the runner last observed.
type SeedPhase string

const (
	SeedOff SeedDesired = "off"
	SeedOn  SeedDesired = "on"

	SeedIdle      SeedPhase = "idle"
	SeedWaiting   SeedPhase = "waiting"
	SeedVerifying SeedPhase = "verifying"
	SeedServing   SeedPhase = "serving"
	SeedStopping  SeedPhase = "stopping"
	SeedStopped   SeedPhase = "stopped"
	SeedBlocked   SeedPhase = "blocked"
)

// SeedWanted reports whether the user asked for this entry to be seeded.
func (e Entry) SeedWanted() bool { return e.SeedDesired == SeedOn }

// SeedEligible reports whether the entry can be handed to a seed session right now:
// a completed download with a published capture, an explicit opt-in, and no seed
// already owning the record.
func (e Entry) SeedEligible(now time.Time) bool {
	if !e.SeedWanted() || e.Phase != PhaseCompleted || e.Capture == nil || e.Workspace == "" {
		return false
	}
	switch e.SeedPhase {
	case SeedServing, SeedVerifying, SeedStopping:
		return false
	}
	return !e.SeedNotBefore.After(now)
}

// SeedPending reports work a runner is still responsible for: opted-in entries that
// are idle, waiting, or actively starting/serving. A blocked seed does not keep a
// runner alive - it is waiting for a person, not for a slot.
func (e Entry) SeedPending() bool {
	if !e.SeedWanted() {
		return false
	}
	switch e.SeedPhase {
	case SeedWaiting, SeedVerifying, SeedServing, SeedStopping:
		return true
	case "":
		return e.Phase == PhaseCompleted
	}
	return false
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
	// Attempt is the token of the attempt that currently owns the record, or empty
	// when no attempt is running. A result is applied only if its token still
	// matches, so a superseded attempt cannot overwrite a newer command.
	Attempt string `json:"attempt,omitempty"`
	// LastError is a redacted, actionable reason. It never holds a tracker URL,
	// a passkey or an absolute path.
	LastError string `json:"last_error,omitempty"`
	// NotBefore is a bounded backoff for a blocked entry.
	NotBefore time.Time `json:"not_before,omitempty"`
	// SeedDesired is user intent for managed seeding, kept separate from Desired so
	// a queue drain can never start uploading by itself. Empty reads as off, which
	// is what records written before this field existed mean.
	SeedDesired SeedDesired `json:"seed_desired,omitempty"`
	// SeedPhase is the last observed seeding state, never the intent.
	SeedPhase SeedPhase `json:"seed_phase,omitempty"`
	// SeedAttempt is the token of the seed attempt that owns the record, so a late
	// result cannot overwrite a newer unseed or seed command.
	SeedAttempt string `json:"seed_attempt,omitempty"`
	SeedError   string `json:"seed_error,omitempty"`
	// SeedNotBefore is a bounded backoff after an ineligible workspace.
	SeedNotBefore time.Time `json:"seed_not_before,omitempty"`
	// Priorities raises the download preference of files that are ALREADY in
	// Selection, keyed by exact selected path. It is an override table, not a second
	// selection: a path here that is not in Selection is rejected, and absence means
	// the default the selection always had. It is not part of the workspace id, the
	// capture key or the duplicate key.
	Priorities map[string]string `json:"priorities,omitempty"`
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
//
// With more than one worker, a claim and a result write collide regularly. The
// lock is short, so a bounded retry is the right answer: failing the claim instead
// would park the coordinator on a transient condition, and spinning without a
// bound would burn CPU on a stuck owner.
func (s *Store) lock() (*filelock.Lock, error) {
	path := filepath.Join(s.root, filepath.FromSlash(queueDirName), "store")
	backoff := time.Millisecond
	for attempt := 0; attempt < 12; attempt++ {
		l, err := filelock.TryAcquire(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, err
		}
		time.Sleep(backoff)
		if backoff < 20*time.Millisecond {
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("%w: the queue store is locked by another process; retry", ErrStoreBusy)
}

// Add creates an entry, or returns the existing one when the same capture intent
// is already queued. Duplicate detection is by canonical job id when it is known
// (so a btih magnet and a .torrent of the same torrent converge), and by sanitized
// source otherwise.
func (s *Store) Add(req AddRequest) (Entry, bool, error) {
	var zero Entry
	if len(req.Selection) == 0 {
		return zero, false, fmt.Errorf("%w: a queue entry needs at least one selected path", ErrUsage)
	}
	if _, err := time.ParseDuration("0s"); err != nil { // keep time imported for doc clarity
		return zero, false, err
	}
	if strings.TrimSpace(req.Source.Magnet) == "" && strings.TrimSpace(req.Source.Path) == "" {
		return zero, false, errors.New("a queue entry needs a magnet or a .torrent path")
	}

	l, err := s.lock()
	if err != nil {
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

// newAttemptToken mints an attempt id. It is not secret; it only has to be unique
// within a queue.
func newAttemptToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("a-%d", time.Now().UnixNano())
	}
	return "a-" + hex.EncodeToString(b[:])
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

// SetSeedIntent records the user's seeding intent for one entry. It persists before
// returning, so a caller may acknowledge only after this succeeds, and it never
// touches download intent.
func (s *Store) SetSeedIntent(id string, on bool) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		if on {
			if e.Phase != PhaseCompleted || e.Capture == nil || e.Workspace == "" {
				return fmt.Errorf("%w: only a completed entry with a published capture can be seeded", ErrUsage)
			}
			e.SeedDesired = SeedOn
			e.SeedPhase = SeedWaiting
			e.SeedNotBefore = time.Time{}
			e.SeedError = ""
			return nil
		}
		e.SeedDesired = SeedOff
		switch e.SeedPhase {
		case SeedServing, SeedVerifying:
			// The runner observes the intent and reports stopping until the client
			// has closed; the phase here is not a claim that it already stopped.
			e.SeedPhase = SeedStopping
		default:
			e.SeedPhase = SeedStopped
		}
		return nil
	})
}

// SeedCandidates lists entries this runner may start serving, in queue order,
// excluding anything already in flight and anything in a workspace already owned.
func (s *Store) SeedCandidates(now time.Time, skip map[string]bool) ([]Entry, error) {
	entries, err := s.list()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if skip[e.ID] || !e.SeedEligible(now) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// SeedPending reports whether any entry still needs this runner's seed pool.
func (s *Store) SeedPending() (bool, error) {
	entries, err := s.list()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.SeedPending() {
			return true, nil
		}
	}
	return false, nil
}

// UpdateSeed applies a mutation only if the seed attempt still owns the record.
func (s *Store) UpdateSeed(id, token string, mutate func(*Entry) error) (Entry, error) {
	var stale bool
	entry, err := s.Update(id, func(e *Entry) error {
		if token != "" && e.SeedAttempt != token {
			stale = true
			return nil
		}
		return mutate(e)
	})
	if err != nil {
		return entry, err
	}
	if stale {
		return entry, errStaleAttempt
	}
	return entry, nil
}

// Removal reports exactly what a remove did, so a caller never claims a clean
// removal when credential cleanup lagged behind it.
type Removal struct {
	ID                string
	EntryRemoved      bool
	RestrictedRemoved bool
	// CleanupError is non-empty when the public entry is gone but its restricted
	// source record is still on disk. Retrying the same id cleans that record and
	// nothing else.
	CleanupError string
}

// validEntryID accepts only the ids this store allocates: queued-<6 digits>-<8 hex>.
// A prefix, a torrent hash, a filename or a path is refused here rather than being
// turned into a deletion target.
func validEntryID(id string) bool {
	if !strings.HasPrefix(id, "queued-") {
		return false
	}
	rest := id[len("queued-"):]
	if len(rest) < 6 || len(rest) > 6+1+8 {
		return false
	}
	if !isDigits(rest[:6]) {
		return false
	}
	if len(rest) == 6 {
		return true
	}
	if rest[6] != '-' {
		return false
	}
	return isHexLower(rest[7:])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Remove forgets one queue entry and its own restricted source record, and nothing
// else. A workspace, payload, resume database, capture or vault artifact is never
// touched: those belong to the job, not to the queue.
//
// The public record goes first, so a crash cannot leave a runnable entry whose source
// was already deleted. If deleting the restricted record fails afterwards the entry is
// still gone and the caller is told that cleanup is incomplete; retrying the same id
// deletes that record and no other.
func (s *Store) Remove(id string) (Removal, error) {
	res := Removal{ID: id}
	if !validEntryID(id) {
		return res, fmt.Errorf("%w: %q is not a queue entry id", ErrUsage, id)
	}
	// Ownership first, then the store lock - the same order a runner uses. Holding
	// runner ownership across the mutation is what makes "no runner is active" and
	// "the entry is gone" one atomic decision; a recorded phase proves nothing.
	owner, err := s.LockRunner()
	if err != nil {
		return res, err
	}
	defer func() { _ = owner.Release() }()

	l, err := s.lock()
	if err != nil {
		return res, err
	}
	defer func() { _ = l.Release() }()

	if _, serr := os.Stat(s.entryPath(id)); serr == nil {
		if rerr := os.Remove(s.entryPath(id)); rerr != nil {
			return res, fmt.Errorf("remove queue entry %s: %w", id, rerr)
		}
		res.EntryRemoved = true
	}
	restricted := filepath.Join(s.restrictedDir(), id+".json")
	if _, serr := os.Stat(restricted); serr == nil {
		if rerr := os.Remove(restricted); rerr != nil {
			res.CleanupError = fmt.Sprintf("queue entry %s is removed, but its restricted source record could not be deleted; retry `torrent queue remove %s`", id, id)
		} else {
			res.RestrictedRemoved = true
		}
	}
	if !res.EntryRemoved && !res.RestrictedRemoved && res.CleanupError == "" {
		return res, fmt.Errorf("%w: %s", ErrEntryNotFound, id)
	}
	return res, nil
}

// SelectedPaths returns the exact paths this entry will fetch, or nil when they are
// not known yet.
func (e Entry) SelectedPaths() []string { return append([]string(nil), e.Selection...) }

// SetPriority records a download preference for one path already in the selection.
// `normal` clears the override. It never edits the selection, the desired state, the
// queue order or seeding intent, and it refuses a completed entry: editing a priority
// must not reopen, restart or rewrite a published capture.
func (s *Store) SetPriority(id, path, level string) (Entry, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level != "normal" && level != "high" {
		return Entry{}, fmt.Errorf("%w: priority must be normal or high, got %q", ErrUsage, level)
	}
	want := strings.TrimPrefix(strings.TrimSpace(path), "./")
	if want == "" || strings.Contains(want, "..") || filepath.IsAbs(want) {
		return Entry{}, fmt.Errorf("%w: %q is not a selected relative path", ErrUsage, path)
	}
	owner, err := s.LockRunner()
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = owner.Release() }()

	var out Entry
	out, err = s.Update(id, func(e *Entry) error {
		switch {
		case e.Unresolved || len(e.Selection) == 0:
			return fmt.Errorf("%w: the exact selection for %s is not known yet; run it once so the paths are recorded", ErrUsage, id)
		case e.Phase == PhaseCompleted:
			return fmt.Errorf("%w: %s already published a capture; its priority cannot be edited", ErrUsage, id)
		}
		found := false
		for _, sel := range e.Selection {
			if strings.TrimPrefix(sel, "./") == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %q is not one of the %d selected paths", ErrSelectionNotSelected, want, len(e.Selection))
		}
		if level == "normal" {
			if e.Priorities != nil {
				delete(e.Priorities, want)
				if len(e.Priorities) == 0 {
					e.Priorities = nil
				}
			}
			return nil
		}
		if e.Priorities == nil {
			e.Priorities = map[string]string{}
		}
		e.Priorities[want] = "high"
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return out, nil
}

// LockRunner takes runner ownership without blocking. Removal uses it so a foreground
// runner cannot start between the busy check and the mutation: the answer is the lock,
// never a recorded phase or the mere existence of a sidecar file.
func (s *Store) LockRunner() (*filelock.Lock, error) {
	l, err := filelock.TryAcquire(filepath.Join(s.root, filepath.FromSlash(queueDirName), "runner"))
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("%w: a foreground runner owns this queue; stop it, wait for its clients to close, then retry", ErrRunnerActive)
		}
		return nil, err
	}
	return l, nil
}

// RestrictedSource returns the stored raw source and peers for an entry. It is about
// the restricted record, not the public one.
// RestrictedSource returns the stored raw source and peers for an entry. It is only
// used by the runner (to resolve an entry) and by nothing that prints.
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
