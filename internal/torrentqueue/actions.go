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

// Start records desired=run. A failed entry is refused so a bulk start can never
// silently retry a failure.
func (s *Store) Start(id string) (Entry, error) {
	return s.Update(id, func(e *Entry) error {
		if e.Phase == PhaseFailed {
			return fmt.Errorf("%w: %s", ErrEntryFailed, e.ID)
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

// Retry clears a failure or a backoff and makes the entry runnable again. It is not
// the same as Start: it also clears the error, the note and the backoff.
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
