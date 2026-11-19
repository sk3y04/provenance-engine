package torrentqueue

// Aggregate budget tests (BT-08, second slice). Two kinds of proof, kept apart on
// purpose:
//
//   - wiring: which limiter pointer each attempt receives (deterministic, no
//     network, says nothing about bytes);
//   - measurement: real bytes on disk over a defined window with loopback seeders,
//     compared against the token-bucket bound and against what two independent
//     per-client limiters would have allowed.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestQueueSharedLimiterWiring is acceptance 1's first half: every attempt of one
// runner gets the same limiter instances, and no limit means none at all.
func TestQueueSharedLimiterWiring(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	startAll(t, store, a.ID, b.ID)

	g := newGatedTransfer()
	var mu sync.Mutex
	seen := []Request{}
	g.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		return successOutcome(req), nil
	}
	_, err := NewRunner(store, g, Options{
		PollInterval:  2 * time.Millisecond,
		MaxActive:     2,
		DownloadLimit: 512 << 10,
		UploadLimit:   128 << 10,
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d attempts observed, want 2", len(seen))
	}
	first, second := seen[0], seen[1]
	if first.DownloadLimiter == nil || first.UploadLimiter == nil {
		t.Fatal("a configured runner handed a nil limiter to an attempt")
	}
	if first.DownloadLimiter != second.DownloadLimiter {
		t.Error("attempts received different download limiters: the budget is not shared")
	}
	if first.UploadLimiter != second.UploadLimiter {
		t.Error("attempts received different upload limiters: the budget is not shared")
	}
	if got := first.DownloadLimiter.Limit(); got != rate.Limit(512<<10) {
		t.Errorf("download limit = %v, want %v", got, rate.Limit(512<<10))
	}
	if got := first.UploadLimiter.Limit(); got != rate.Limit(128<<10) {
		t.Errorf("upload limit = %v, want %v", got, rate.Limit(128<<10))
	}
	// The burst is explicit and nonzero, and never the rate itself: the pinned
	// library panics on a zero burst.
	if burst := first.DownloadLimiter.Burst(); burst != DownloadBurstBytes {
		t.Errorf("download burst = %d, want %d", burst, DownloadBurstBytes)
	}
	if burst := first.UploadLimiter.Burst(); burst != UploadBurstBytes {
		t.Errorf("upload burst = %d, want %d", burst, UploadBurstBytes)
	}
	if DownloadBurstBytes <= 16<<10 {
		t.Error("download burst must exceed a block read")
	}
	if UploadBurstBytes < 1<<20 {
		t.Error("upload burst must cover MaxAllocPeerRequestDataPerConn or the library panics")
	}

	// Unlimited is the previous behaviour: both pointers are nil, so the library
	// default (rate.Inf) is left untouched.
	store2 := newTestStore(t)
	c := prepared(t, store2, testJobID(2))
	startAll(t, store2, c.ID)
	g2 := newGatedTransfer()
	var got Request
	g2.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		got = req
		return successOutcome(req), nil
	}
	if _, err := NewRunner(store2, g2, Options{PollInterval: 2 * time.Millisecond}).Run(context.Background()); err != nil {
		t.Fatalf("unlimited drain: %v", err)
	}
	if got.DownloadLimiter != nil || got.UploadLimiter != nil {
		t.Errorf("unlimited runner handed limiters: %v / %v", got.DownloadLimiter, got.UploadLimiter)
	}
}

// TestQueueRateValidation is acceptance 1's boundary half.
func TestQueueRateValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		ok   bool
	}{
		{"zero-is-unlimited", Options{}, true},
		{"negative-download", Options{DownloadLimit: -1}, false},
		{"negative-upload", Options{UploadLimit: -1}, false},
		{"above-max", Options{DownloadLimit: MaxRateBytes + 1}, false},
		{"overflow", Options{UploadLimit: 1 << 62}, false},
		{"at-max", Options{DownloadLimit: MaxRateBytes}, true},
		{"typical", Options{DownloadLimit: 2 << 20, UploadLimit: 512 << 10}, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.ok && err != nil {
				t.Fatalf("valid options rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
	// An invalid rate must be refused before anything is claimed.
	store := newTestStore(t)
	entry := prepared(t, store, testJobID(0))
	startAll(t, store, entry.ID)
	g := newGatedTransfer()
	if _, err := NewRunner(store, g, Options{DownloadLimit: -5}).Run(context.Background()); err == nil {
		t.Fatal("negative download rate started a drain")
	}
	if g.runCount() != 0 {
		t.Error("an attempt ran despite the invalid rate")
	}
}

// payloadBytes sums the bytes the attempt has actually written under a workspace.
// File length is useless here: the library allocates each file at its full size up
// front, so a freshly opened 2 MiB file already reports 2 MiB. Allocated blocks are
// what tracks data landing on disk.
func payloadBytes(root, jobID string) (int64, bool) {
	ws := filepath.Join(root, filepath.FromSlash(".provenance/torrent/jobs/"+jobID))
	var total int64
	supported := true
	_ = filepath.WalkDir(ws, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(ws, p)
		if rerr != nil || rel == "." {
			return nil
		}
		// Skip the adapter's own namespace, not every path that mentions it: the
		// workspace root itself lives under .provenance/torrent/jobs.
		if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); first == ".provenance" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		st, isStat := info.Sys().(*syscall.Stat_t)
		if !isStat {
			supported = false
			return filepath.SkipAll
		}
		total += st.Blocks * 512
		return nil
	})
	return total, supported
}

// TestQueueAggregateDownloadBudget is acceptance 2: two overlapping real downloads
// share one budget. The assertion is the token-bucket bound (burst + rate*window),
// not "it felt slow": it also states what two independent per-client limiters would
// have permitted, which the measurement is well under.
func TestQueueAggregateDownloadBudget(t *testing.T) {
	const (
		capPerSecond = 512 << 10
		fileSize     = 2 << 20
		burst        = DownloadBurstBytes
	)
	filesA := map[string][]byte{"a.bin": patternedBytes(fileSize, 61)}
	filesB := map[string][]byte{"a.bin": patternedBytes(fileSize, 67)}
	seedA := newLoopbackSeeder(t, "fixture", filesA, 0)
	seedB := newLoopbackSeeder(t, "fixture", filesB, 0)
	// An unthrottled seeder: the only limiter in play is the one under test, on the
	// downloading clients.
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	a := enqueue(t, store, seedA, "fixture/a.bin")
	b := enqueue(t, store, seedB, "fixture/a.bin")
	startEntry(t, store, a.ID, b.ID)

	type sample struct {
		at    time.Duration
		bytes int64
	}
	var samples []sample
	start := time.Now()
	sampling := make(chan struct{})
	// sampled closes when the sampler has returned, so the assertions below read
	// samples with a happens-before edge instead of racing the goroutine.
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampling:
				return
			case now := <-ticker.C:
				aBytes, aOK := payloadBytes(root, seedA.jobID())
				bBytes, bOK := payloadBytes(root, seedB.jobID())
				if !aOK || !bOK {
					return
				}
				samples = append(samples, sample{at: now.Sub(start), bytes: aBytes + bBytes})
			}
		}
	}()

	sum, err := NewRunner(store, realAdapter(), Options{
		MaxActive:     2,
		PollInterval:  10 * time.Millisecond,
		DownloadLimit: capPerSecond,
	}).Run(context.Background())
	close(sampling)
	<-sampled
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Completed != 2 {
		t.Fatalf("summary = %+v, want both jobs completed", sum)
	}
	total := int64(2 * fileSize)
	if len(samples) == 0 {
		t.Skip("allocated-block accounting is unavailable on this platform")
	}

	// Token-bucket bound: the clients can never deliver more than the initial burst
	// plus rate * elapsed. This is a real-traffic assertion, not an inference from
	// the fixture size.
	allowed := float64(burst) + float64(capPerSecond)*elapsed.Seconds()
	if float64(total) > allowed*1.25 {
		t.Errorf("delivered %d bytes in %s; a shared %d B/s budget allows at most %.0f (+25%% slack)", total, elapsed, capPerSecond, allowed)
	}
	// And the budget was actually used: two independent per-client limiters of the
	// same rate would have allowed about twice this, so require the run to be
	// materially slower than that.
	twoLimiters := float64(burst*2) + float64(2*capPerSecond)*elapsed.Seconds()
	if float64(total) > twoLimiters*0.85 {
		t.Errorf("delivered %d bytes in %s, which two independent per-client limiters would also allow (%.0f); the budget is not shared", total, elapsed, twoLimiters)
	}
	minDuration := time.Duration(float64(total-burst) / float64(capPerSecond) * float64(time.Second) * 0.8)
	if elapsed < minDuration {
		t.Errorf("finished in %s, faster than the shared budget should allow (%s)", elapsed, minDuration)
	}

	// Steady-state rate over a window that starts after the burst credit is spent,
	// computed from sampled payload bytes rather than from wall time alone.
	fromIdx := -1
	for i := range samples {
		if samples[i].at >= 1200*time.Millisecond {
			fromIdx = i
			break
		}
	}
	toIdx := -1
	if fromIdx >= 0 {
		for i := fromIdx + 1; i < len(samples); i++ {
			if samples[i].bytes >= total {
				break
			}
			toIdx = i
		}
	}
	if fromIdx < 0 || toIdx < 0 || samples[toIdx].at-samples[fromIdx].at < 2*time.Second {
		t.Skipf("run too short for a steady-state window (%d samples over %s)", len(samples), elapsed)
	}
	from, to := samples[fromIdx], samples[toIdx]
	windowRate := float64(to.bytes-from.bytes) / (to.at - from.at).Seconds()
	t.Logf("aggregate download: total %d bytes in %s; steady-state %.0f B/s over %.1fs window (cap %d B/s)",
		total, elapsed.Round(time.Millisecond), windowRate, (to.at - from.at).Seconds(), capPerSecond)
	if windowRate > float64(capPerSecond)*1.3 {
		t.Errorf("steady-state %.0f B/s exceeds the shared cap %d B/s", windowRate, capPerSecond)
	}
	if windowRate < float64(capPerSecond)*0.35 {
		t.Errorf("steady-state %.0f B/s is far below the shared cap %d B/s: the budget is being wasted", windowRate, capPerSecond)
	}
}

// TestQueueLowRateMakesProgress is acceptance 4's first half: a low but supported
// rate still delivers, which is where a too-small burst would show up as a stall or
// a library panic.
func TestQueueLowRateMakesProgress(t *testing.T) {
	// Deliberately larger than the 256 KiB burst, so the measurement window starts
	// after the burst credit is spent; a fixture that fits in the burst would finish
	// instantly and prove nothing about throttling.
	files := map[string][]byte{"a.bin": patternedBytes(640<<10, 71)}
	seed := newLoopbackSeeder(t, "fixture", files, 0)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := enqueue(t, store, seed, "fixture/a.bin")
	startEntry(t, store, entry.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	sum, err := NewRunner(store, realAdapter(), Options{
		PollInterval:  10 * time.Millisecond,
		DownloadLimit: 96 << 10, // 96 KiB/s against a 256 KiB burst
	}).Run(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("low-rate drain: %v", err)
	}
	if sum.Completed != 1 {
		got, _ := store.Get(entry.ID)
		t.Fatalf("summary = %+v, phase %q error %q", sum, got.Phase, got.LastError)
	}
	final, _ := store.Get(entry.ID)
	ws := filepath.Join(root, filepath.FromSlash(final.Workspace))
	payload, rerr := os.ReadFile(filepath.Join(ws, "fixture", "a.bin"))
	if rerr != nil {
		t.Fatalf("payload: %v", rerr)
	}
	if string(payload) != string(files["a.bin"]) {
		t.Error("low-rate payload does not match the fixture")
	}
	if n := runManifestCount(t, ws); n != 1 {
		t.Errorf("%d run manifests, want 1", n)
	}
	// 640 KiB at 96 KiB/s, minus the 256 KiB burst, is at least ~4s of work.
	if elapsed < 3*time.Second {
		t.Errorf("640 KiB completed in %s at a 96 KiB/s cap: the limit was not applied", elapsed)
	}
	t.Logf("low-rate download: 640 KiB in %s at a 96 KiB/s cap", elapsed.Round(time.Millisecond))
}

// TestQueuePauseWithSharedBudget is acceptance 4's second half: pausing one throttled
// attempt leaves the shared limiter intact for its sibling and frees only its own
// slot, once its client has closed.
func TestQueuePauseWithSharedBudget(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	startAll(t, store, a.ID, b.ID)

	g := newGatedTransfer()
	g.ignoreCancel = true // the throttled client is mid-close
	var mu sync.Mutex
	seen := map[string]Request{}
	g.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		mu.Lock()
		seen[req.KnownJobID] = req
		mu.Unlock()
		<-g.gate(req.KnownJobID)
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		return successOutcome(req), nil
	}
	runner := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2, DownloadLimit: 256 << 10})
	done := make(chan Summary, 1)
	go func() {
		sum, _ := runner.Run(context.Background())
		done <- sum
	}()
	waitEntered(t, g, 2, 5*time.Second)

	if _, err := store.Update(a.ID, func(e *Entry) error { e.Desired = DesiredPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, store, a.ID, PhaseStopping, 5*time.Second)
	// The sibling still holds the same limiter instance: pausing must not disable or
	// replace the shared budget.
	mu.Lock()
	limiterA, limiterB := seen[testJobID(0)].DownloadLimiter, seen[testJobID(1)].DownloadLimiter
	mu.Unlock()
	if limiterA == nil || limiterA != limiterB {
		t.Fatalf("shared limiter replaced or nil after a pause: %p / %p", limiterA, limiterB)
	}
	if limiterA.Limit() != rate.Limit(256<<10) {
		t.Errorf("limiter limit changed to %v", limiterA.Limit())
	}

	close(g.gate(testJobID(0)))
	waitPhase(t, store, a.ID, PhasePaused, 5*time.Second)
	if sibling, _ := store.Get(b.ID); sibling.Phase != PhaseRunning {
		t.Fatalf("sibling phase = %q after the pause closed", sibling.Phase)
	}
	close(g.gate(testJobID(1)))
	sum := <-done
	if sum.Paused != 1 || sum.Completed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if got, _ := store.Get(a.ID); got.Capture != nil {
		t.Error("a paused attempt recorded a capture")
	}
}
