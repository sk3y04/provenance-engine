package torrentqueue

// BT-12: selected-payload progress and rate on the live endpoint. The rules here are
// the release gate: unknown never renders as zero, and no attempt inherits a previous
// attempt's rate.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// attemptAt returns one live attempt under the state's mutex.
func attemptAt(t *testing.T, l *liveState, id string) (LiveAttempt, bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[id]
	return a, ok
}

func TestLiveProgressRateRules(t *testing.T) {
	l := newLiveState("/root")
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	l.add("e1", "download", "running")

	// First sample: bytes are a measurement, the rate is not yet known.
	l.progress("e1", 1000, 10000)
	a, _ := attemptAt(t, l, "e1")
	if a.SelectedBytes == nil || *a.SelectedBytes != 1000 || a.SelectedTotal == nil || *a.SelectedTotal != 10000 {
		t.Fatalf("first sample = %+v", a)
	}
	if a.ProgressRate != nil {
		t.Fatalf("first sample reported a rate (%d) with no baseline", *a.ProgressRate)
	}

	// A delta over a window at least as long as liveRateWindow becomes the rate.
	now = now.Add(300 * time.Millisecond)
	l.progress("e1", 4000, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate == nil {
		t.Fatal("no rate after a complete window")
	}
	if want := int64(3000 / 0.3); *a.ProgressRate != want {
		t.Fatalf("rate = %d, want %d", *a.ProgressRate, want)
	}
	rate := *a.ProgressRate

	// A sample inside the window keeps the previous baseline: no divide-by-tiny spike.
	now = now.Add(100 * time.Millisecond)
	l.progress("e1", 4100, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate == nil || *a.ProgressRate != rate {
		t.Fatalf("rate inside the window = %v, want the previous %d", a.ProgressRate, rate)
	}

	// Once enough time has passed since that baseline the rate covers the longer
	// interval, so the 100 bytes reported inside the window are not lost.
	now = now.Add(200 * time.Millisecond)
	l.progress("e1", 4100, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate == nil || *a.ProgressRate <= 0 {
		t.Fatalf("rate over the widened window = %v, want positive", a.ProgressRate)
	}

	// Measured zero: a full window with no new bytes is 0, not absent.
	now = now.Add(300 * time.Millisecond)
	l.progress("e1", 4100, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate == nil || *a.ProgressRate != 0 {
		t.Fatalf("measured zero = %v, want 0", a.ProgressRate)
	}

	// Rollback: no negative rate, and the baseline restarts from the new value.
	now = now.Add(300 * time.Millisecond)
	l.progress("e1", 2000, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate != nil {
		t.Fatalf("rollback reported rate %d", *a.ProgressRate)
	}
	now = now.Add(300 * time.Millisecond)
	l.progress("e1", 3500, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate == nil || *a.ProgressRate < 0 {
		t.Fatalf("rate after rollback = %v", a.ProgressRate)
	}

	// A new attempt for the same entry starts with no baseline, so a retry can never
	// inherit the previous attempt's rate.
	l.remove("e1")
	l.add("e1", "download", "running")
	l.progress("e1", 9000, 10000)
	a, _ = attemptAt(t, l, "e1")
	if a.ProgressRate != nil {
		t.Fatalf("a new attempt inherited a rate: %d", *a.ProgressRate)
	}

	// A sample that arrives after the attempt ended never resurrects it.
	l.remove("e1")
	l.progress("e1", 9500, 10000)
	if _, ok := attemptAt(t, l, "e1"); ok {
		t.Fatal("a late sample resurrected a removed attempt")
	}
}

func TestLiveSnapshotProgressCompatibility(t *testing.T) {
	zero, total := int64(0), int64(4096)
	rate := int64(0)
	snap := LiveSnapshot{Attempts: []LiveAttempt{{
		EntryID: "e1", Kind: "download", Phase: "running",
		SelectedBytes: &zero, SelectedTotal: &total, ProgressRate: &rate,
	}}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Explicit zeroes must survive the round trip as zeroes, not disappear.
	var back LiveSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Attempts) != 1 || back.Attempts[0].SelectedBytes == nil || *back.Attempts[0].SelectedBytes != 0 {
		t.Fatalf("measured zero did not round-trip: %s", raw)
	}

	// An older endpoint omits the fields: they decode as absent, which is what the
	// renderers treat as unknown.
	var older LiveSnapshot
	if err := json.Unmarshal([]byte(`{"format":"provenance/torrent-queue-live+1","attempts":[{"entry_id":"e1","kind":"download","phase":"running"}]}`), &older); err != nil {
		t.Fatalf("decode older snapshot: %v", err)
	}
	if older.Attempts[0].SelectedBytes != nil || older.Attempts[0].ProgressRate != nil {
		t.Fatalf("absent fields decoded as measurements: %+v", older.Attempts[0])
	}

	// A new endpoint read by an older client: unknown fields are ignored and the
	// fields it does know still decode.
	var legacy struct {
		Attempts []struct {
			EntryID string `json:"entry_id"`
			Phase   string `json:"phase"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("old client decode: %v", err)
	}
	if len(legacy.Attempts) != 1 || legacy.Attempts[0].Phase != "running" {
		t.Fatalf("old client lost the fields it understands: %+v", legacy.Attempts)
	}
}

// TestLiveProgressDuringDownload is the end-to-end check: a real runner over a
// throttled loopback peer publishes measured progress while the transfer runs.
func TestLiveProgressDuringDownload(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("setup: open store: %v", err)
	}
	torrentPath, peer := matchedFixture(t, 4<<20, 256<<10)
	entry := addFileEntry(t, root, torrentPath, peer)
	startEntry(t, store, entry.ID)

	w := startWatch(t, store, realAdapter(), Options{MaxActive: 1, MaxSeeds: 0, PollInterval: watchPoll, Log: nil})

	snap, ok := waitSnapshot(t, root, 60*time.Second, func(s LiveSnapshot) bool {
		for _, a := range s.Attempts {
			if a.EntryID == entry.ID && a.SelectedBytes != nil && *a.SelectedBytes > 0 {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("no snapshot carried selected progress for %s (%+v)", entry.ID, mustGet(t, store, entry.ID))
	}
	var got LiveAttempt
	for _, a := range snap.Attempts {
		if a.EntryID == entry.ID {
			got = a
		}
	}
	if got.SelectedTotal == nil || *got.SelectedTotal != int64(4<<20) {
		t.Fatalf("selected total = %v, want %d", got.SelectedTotal, 4<<20)
	}
	if *got.SelectedBytes > *got.SelectedTotal {
		t.Fatalf("completed %d exceeds total %d", *got.SelectedBytes, *got.SelectedTotal)
	}
	t.Logf("LIVE progress entry=%s selected=%d/%d rate=%v", got.EntryID, *got.SelectedBytes, *got.SelectedTotal, rateString(got.ProgressRate))

	// The rate appears once a window has elapsed.
	rated, ok := waitSnapshot(t, root, 60*time.Second, func(s LiveSnapshot) bool {
		for _, a := range s.Attempts {
			if a.EntryID == entry.ID && a.ProgressRate != nil {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatal("the runner never reported a progress rate during a throttled download")
	}
	for _, a := range rated.Attempts {
		if a.EntryID == entry.ID {
			t.Logf("LIVE rate entry=%s %s", a.EntryID, rateString(a.ProgressRate))
			if *a.ProgressRate < 0 {
				t.Fatalf("negative rate %d", *a.ProgressRate)
			}
		}
	}

	if !w.stop(t, 60*time.Second) {
		t.Fatal("the session did not stop")
	}
	// Stale progress must not survive the runner.
	if _, perr := liveProbe(t, root); perr == nil {
		t.Fatal("the endpoint still answered after the session ended")
	}
}

func rateString(r *int64) string {
	if r == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d B/s", *r)
}
