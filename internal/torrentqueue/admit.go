package torrentqueue

// One admission path for the CLI and the TUI (BT-10c).
//
// Preparing a source, sanitising it, choosing whether the identity is already known
// and handing the result to the store is the same work in both front ends. Doing it
// twice is how the two would drift apart, so both call Admit.

import (
	"fmt"
	"path/filepath"

	"github.com/sk3y04/provenance-engine/internal/transfer/bittorrent"
)

// Admission is the outcome of admitting one source into the queue.
type Admission struct {
	Entry Entry
	// Duplicate is true when an equivalent entry already existed; Entry is that entry
	// and nothing was written.
	Duplicate bool
	// Unresolved is true when the identity needs a metadata exchange, so the entry is
	// recorded with its restricted source and a discovery profile instead of a job id.
	Unresolved bool
}

// Admit prepares a source and records it as a paused queue entry.
//
// A local .torrent is read offline; a magnet keeps its discovery profile and is
// recorded unresolved until metadata is fetched. A source this build cannot resume is
// refused here rather than accepted as a job that would never start.
func Admit(store *Store, raw string, mode bittorrent.Discovery, peers, selection []string) (Admission, error) {
	var out Admission
	if len(selection) == 0 {
		return out, fmt.Errorf("%w: an entry needs at least one selected path", ErrUsage)
	}
	prepared, err := bittorrent.PrepareSource(store.Root(), raw, mode)
	if err != nil {
		return out, err
	}
	source := Source{Kind: prepared.Kind}
	if prepared.Kind == "file" {
		if abs, aerr := filepath.Abs(raw); aerr == nil {
			source.Path = abs
		} else {
			source.Path = raw
		}
	} else {
		source.Magnet = prepared.Sanitized
	}
	entry, duplicate, aerr := store.Add(AddRequest{
		Source:     source,
		RawSource:  raw,
		Peers:      peers,
		Discovery:  string(mode),
		Selection:  selection,
		JobID:      prepared.JobID,
		InfoHash:   prepared.InfoHash,
		InfoHashV2: prepared.InfoHashV2,
		Workspace:  prepared.Workspace,
		Unresolved: prepared.Unresolved,
	})
	if aerr != nil {
		return out, aerr
	}
	out.Entry, out.Duplicate, out.Unresolved = entry, duplicate, prepared.Unresolved
	return out, nil
}
