package torrentqueue

// Durable queue operations shared by the CLI and the TUI (BT-10d).
//
// These are the same transitions `torrent queue start|pause|retry` perform, moved out
// of the command bodies so both front ends apply one rule set. They write intent and
// never touch a runner: a saved request is not an observed change, and the runner (if
// any) notices the durable change on its next poll.

import (
	"errors"
	"fmt"
	"time"
)

func zeroTime() time.Time { return time.Time{} }

// ErrEntryFailed is returned by Start for a failed entry: a failure needs an explicit
// retry, never a bulk start.
var ErrEntryFailed = errors.New("entry is failed; use retry to make the retry explicit")

// ErrEntryCompleted is returned by Retry for a completed entry.
var ErrEntryCompleted = errors.New("entry already completed a capture; nothing to retry")

// ErrEntryCancelled is returned by Start for a cancelled entry: cancellation is
// terminal until an explicit retry, just like a failure.
var ErrEntryCancelled = errors.New("entry is cancelled; use retry to make it runnable again")

// Start records desired=run. A failed or cancelled entry is refused so a bulk
// start can never silently resume work the user stopped.
func (s *Store) Start(id string) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		if e.Phase == PhaseFailed {
			return fmt.Errorf("%w: %s", ErrEntryFailed, e.ID)
		}
		if e.Phase == PhaseCancelled || e.Desired == DesiredCancelled {
			return fmt.Errorf("%w: %s", ErrEntryCancelled, e.ID)
		}
		e.Desired = DesiredRun
		if e.Phase == PhasePaused || e.Phase == PhaseStopping {
			e.Phase = PhaseQueued
		}
		return nil
	})
}

// Pause records desired=paused and acknowledges only once it is durable. A running
// attempt is not killed here: the runner observes the intent, reports stopping until
// its client closes, and only then records that it stopped.
func (s *Store) Pause(id string) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		e.Desired = DesiredPaused
		if e.Phase == PhaseQueued {
			e.Phase = PhasePaused
		}
		return nil
	})
}

// Retry clears a failure, a cancellation or a backoff and makes the entry
// runnable again. It is not the same as Start: it also clears the error, the note
// and the backoff.
func (s *Store) Retry(id string) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		if e.Phase == PhaseCompleted {
			return fmt.Errorf("%w: %s", ErrEntryCompleted, e.ID)
		}
		e.Desired = DesiredRun
		e.Phase = PhaseQueued
		e.LastError = ""
		e.Note = ""
		e.NotBefore = zeroTime()
		return nil
	})
}

// Cancel records that the user stopped this entry. A running attempt is not
// killed here: the runner observes the intent, reports stopping until the client
// has closed, then records cancelled. Payload, workspace and a committed capture
// are never removed; only an explicit retry or remove changes them.
func (s *Store) Cancel(id string) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		if e.Phase == PhaseCompleted {
			return fmt.Errorf("%w: %s already completed; cancel has nothing to stop (use remove to forget it)", ErrEntryCompleted, e.ID)
		}
		e.Desired = DesiredCancelled
		e.LastError = ""
		e.NotBefore = zeroTime()
		if e.Phase == PhaseRunning || e.Phase == PhaseStopping || e.Phase == PhaseResolving {
			// The runner still owns the client. Do not claim it stopped.
			e.Note = "cancel requested; waiting for the client to close"
			return nil
		}
		e.Phase = PhaseCancelled
		e.Note = "cancelled; payload and workspace kept"
		return nil
	})
}

// SetImportTarget attaches or replaces the vault handoff for an entry. A
// committed capture becomes pending again, so the next run or `queue import`
// retries only the import. It never changes the selection, the transfer intent or
// a published capture.
func (s *Store) SetImportTarget(id string, target ImportTarget) (Entry, error) {
	normalized, err := normalizeImportTarget(&target)
	if err != nil {
		return Entry{}, err
	}
	return s.Update(id, func(e *Entry) error {
		if e.Import != nil && e.Import.Target == *normalized &&
			(e.Import.State == ImportPending || e.Import.State == ImportImported) {
			return nil
		}
		e.Import = &ImportRef{Target: *normalized, State: ImportPending}
		return nil
	})
}
