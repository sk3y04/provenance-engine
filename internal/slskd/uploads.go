package slskd

// Upload visibility: what peers are pulling from this instance.

import (
	"context"
	"sort"
	"strings"
)

// Uploads reads and controls outbound transfers.
type Uploads struct {
	Client *Client
}

// UploadSummary counts uploads by lifecycle class for a compact display.
type UploadSummary struct {
	Active    int
	Queued    int
	Completed int
	Failed    int
	Bytes     int64
}

// List returns active and queued uploads with slskd's duplicate "Queued,
// Locally" records collapsed (#1609: the same upload can appear more than once
// with identical fields).
func (u *Uploads) List(ctx context.Context) ([]Transfer, error) {
	if u.Client == nil {
		return nil, ErrNoClient
	}
	transfers, err := u.Client.ListUploads(ctx)
	if err != nil {
		return nil, err
	}
	return DedupeUploads(transfers), nil
}

// DedupeUploads collapses duplicate queued-locally records. Only records that
// are still only queued are candidates: two completed uploads of the same file
// to the same peer are two real events and must both survive.
func DedupeUploads(transfers []Transfer) []Transfer {
	seen := map[string]Transfer{}
	out := make([]Transfer, 0, len(transfers))
	for _, t := range transfers {
		if !isQueuedLocally(t.State) {
			out = append(out, t)
			continue
		}
		key := strings.ToLower(t.Username) + "\x00" + strings.ToLower(t.Filename)
		prev, ok := seen[key]
		if !ok {
			seen[key] = t
			out = append(out, t)
			continue
		}
		// Keep the earliest request: that is the upload that will actually run.
		if t.RequestedAt.Before(prev.RequestedAt) {
			for i := range out {
				if out[i].ID == prev.ID {
					out[i] = t
					break
				}
			}
			seen[key] = t
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RequestedAt.Before(out[j].RequestedAt) })
	return out
}

func isQueuedLocally(state string) bool {
	states := transferStates(state)
	if len(states) == 0 {
		return false
	}
	queued, locally := false, false
	for _, s := range states {
		switch strings.ToLower(s) {
		case "queued":
			queued = true
		case "locally":
			locally = true
		}
	}
	return queued && locally
}

// Summarize classifies uploads for a status line.
func Summarize(transfers []Transfer) UploadSummary {
	var sum UploadSummary
	for _, t := range transfers {
		sum.Bytes += t.Transferred
		switch {
		case isQueuedLocally(t.State):
			sum.Queued++
		case strings.Contains(strings.ToLower(t.State), "inprogress") || strings.Contains(strings.ToLower(t.State), "initializing"):
			sum.Active++
		case func() bool { terminal, ok := transferOutcome(t.State); return terminal && ok }():
			sum.Completed++
		case func() bool { terminal, _ := transferOutcome(t.State); return terminal }():
			sum.Failed++
		default:
			sum.Queued++
		}
	}
	return sum
}

// Cancel stops one upload; remove additionally drops the record.
func (u *Uploads) Cancel(ctx context.Context, username, id string, remove bool) error {
	if u.Client == nil {
		return ErrNoClient
	}
	return u.Client.CancelUpload(ctx, username, id, remove)
}

// ClearCompleted removes completed upload records from slskd.
func (u *Uploads) ClearCompleted(ctx context.Context) error {
	if u.Client == nil {
		return ErrNoClient
	}
	return u.Client.ClearCompletedUploads(ctx)
}
