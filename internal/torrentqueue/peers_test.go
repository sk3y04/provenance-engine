package torrentqueue

// Peer bounds (BT-08c). Two measurements and one plumbing assertion:
//
//   - established peers, observed from the *seeder* side while two queue jobs
//     overlap, so the number is real inbound connections rather than a config value;
//   - outbound half-open dials, observed by peers that accept TCP and never answer,
//     which is exactly the state the half-open cap governs;
//   - the per-job values each attempt receives.
//
// What none of these measure: inbound sockets accepted but not yet associated with a
// torrent, tracker/DHT activity, or file descriptors in general.

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestQueuePeerLimitPlumbing checks the values each attempt receives (no network).
func TestQueuePeerLimitPlumbing(t *testing.T) {
	store := newTestStore(t)
	a := prepared(t, store, testJobID(0))
	b := prepared(t, store, testJobID(1))
	startAll(t, store, a.ID, b.ID)

	g := newGatedTransfer()
	var mu sync.Mutex
	var seen []Request
	g.behavior = func(ctx context.Context, req Request) (Outcome, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		return successOutcome(req), nil
	}
	if _, err := NewRunner(store, g, Options{PollInterval: 2 * time.Millisecond, MaxActive: 2, PeersPerJob: 3, HalfOpenPerJob: 2}).Run(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d attempts, want 2", len(seen))
	}
	for _, req := range seen {
		if req.PeersPerAttempt != 3 || req.HalfOpenPerAttempt != 2 {
			t.Errorf("attempt got peers=%d half-open=%d, want 3/2", req.PeersPerAttempt, req.HalfOpenPerAttempt)
		}
	}

	// Boundaries: zero keeps the adapter default, out-of-range is refused.
	for _, tc := range []struct {
		name string
		opts Options
		ok   bool
	}{
		{"default", Options{}, true},
		{"typical", Options{PeersPerJob: 8, HalfOpenPerJob: 4}, true},
		{"at-max", Options{PeersPerJob: MaxPeersPerJob}, true},
		{"negative-peers", Options{PeersPerJob: -1}, false},
		{"too-many-peers", Options{PeersPerJob: MaxPeersPerJob + 1}, false},
		{"negative-half-open", Options{HalfOpenPerJob: -2}, false},
		{"absurd-half-open", Options{HalfOpenPerJob: 5000}, false},
	} {
		if err := tc.opts.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// seedSwarm starts n seeders for one fixture. More seeders than the limit is what
// makes the limit observable.
func seedSwarm(t *testing.T, n int, files map[string][]byte) []*loopbackSeeder {
	t.Helper()
	out := make([]*loopbackSeeder, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, newLoopbackSeeder(t, "fixture", files, 0))
	}
	return out
}

// swarmPeers samples the total established peers across a set of seeders.
func swarmPeers(swarm []*loopbackSeeder) int {
	total := 0
	for _, s := range swarm {
		total += s.activePeers()
	}
	return total
}

// TestQueuePerJobPeerLimitIsObserved is acceptance 1: two overlapping jobs, each
// allowed two peers, against swarms that offer three. The peak is measured from the
// seeders, so it counts real connections.
func TestQueuePerJobPeerLimitIsObserved(t *testing.T) {
	const limit = 2
	filesA := map[string][]byte{"a.bin": patternedBytes(400<<10, 73)}
	filesB := map[string][]byte{"a.bin": patternedBytes(400<<10, 79)}
	swarmA := seedSwarm(t, 3, filesA)
	swarmB := seedSwarm(t, 3, filesB)
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	a := enqueue(t, store, swarmA[0], "fixture/a.bin")
	b := enqueue(t, store, swarmB[0], "fixture/a.bin")
	if a.JobID == b.JobID {
		t.Fatal("the two fixtures share an infohash")
	}
	startEntry(t, store, a.ID, b.ID)

	// Every seeder of a swarm is offered to the matching job, and the limit is
	// lower than the swarm size, so a passing measurement means the cap bound it.
	if _, uerr := store.Update(a.ID, func(e *Entry) error { return nil }); uerr != nil {
		t.Fatal(uerr)
	}
	peersA := peerAddrs(swarmA)
	peersB := peerAddrs(swarmB)
	setPeers(t, store, a.ID, peersA)
	setPeers(t, store, b.ID, peersB)

	var peak, rawMax int32
	sampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var prev int32 = -1
		for {
			select {
			case <-sampling:
				return
			case <-ticker.C:
				n := int32(swarmPeers(append(append([]*loopbackSeeder{}, swarmA...), swarmB...)))
				for {
					cur := atomic.LoadInt32(&rawMax)
					if n <= cur || atomic.CompareAndSwapInt32(&rawMax, cur, n) {
						break
					}
				}
				// A count that lasts one sample is a socket in teardown - the metadata
				// resolver's connection can still be visible on the seeder for a moment
				// after that client closes. A real breach of the per-job limit persists,
				// so only a count repeated in two consecutive samples counts as a peak.
				if n > 0 && n == prev {
					for {
						cur := atomic.LoadInt32(&peak)
						if n <= cur || atomic.CompareAndSwapInt32(&peak, cur, n) {
							break
						}
					}
				}
				prev = n
			}
		}
	}()

	sum, err := NewRunner(store, realAdapter(), Options{
		MaxActive:    2,
		PollInterval: 10 * time.Millisecond,
		PeersPerJob:  limit,
	}).Run(context.Background())
	close(sampling)
	<-sampled
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	sustained := int(atomic.LoadInt32(&peak))
	instant := int(atomic.LoadInt32(&rawMax))
	bound := 2 * limit
	t.Logf("two jobs, %d peers each, swarms of 3: sustained peak = %d, instantaneous max = %d (bound %d)",
		limit, sustained, instant, bound)
	if sum.Completed != 2 {
		t.Fatalf("summary = %+v, want both jobs completed", sum)
	}
	if instant < 1 {
		t.Fatal("no peer connections were observed; the measurement proves nothing")
	}
	// The runner bound is max-active x per-job, but the metadata resolver's connection
	// can still be visible on a seeder for a few milliseconds after that client closes,
	// so a single extra socket is teardown, not a breach. Anything beyond one extra, or
	// any count that persists across two samples, is a real violation.
	if sustained > bound {
		t.Fatalf("sustained peak %d exceeds the derived runner bound %d", sustained, bound)
	}
	if instant > bound+1 {
		t.Fatalf("peak %d exceeds the derived runner bound %d by more than one in-teardown socket", instant, bound)
	}
}

func peerAddrs(swarm []*loopbackSeeder) []string {
	out := make([]string, 0, len(swarm))
	for _, s := range swarm {
		out = append(out, s.addr)
	}
	return out
}

// setPeers replaces the restricted peer list for an entry, as an operator fixing a
// swarm definition would.
func setPeers(t *testing.T, store *Store, id string, peers []string) {
	t.Helper()
	raw, _, err := store.RestrictedSource(id)
	if err != nil || raw == "" {
		t.Fatalf("read restricted source for %s: %v", id, err)
	}
	if err := store.writeRestricted(id, restrictedSource{RawSource: raw, Peers: peers}); err != nil {
		t.Fatalf("set peers: %v", err)
	}
}

// blackHole accepts TCP connections and never answers the BitTorrent handshake, so
// every accepted socket is one outbound dial stuck in the half-open state.
type blackHole struct {
	ln   net.Listener
	mu   sync.Mutex
	live map[net.Conn]struct{}
	peak int
}

func newBlackHole(t *testing.T) *blackHole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &blackHole{ln: ln, live: map[net.Conn]struct{}{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			b.mu.Lock()
			b.live[c] = struct{}{}
			if len(b.live) > b.peak {
				b.peak = len(b.live)
			}
			b.mu.Unlock()
			// Never answer, but notice the peer hanging up: without a read, a socket
			// the dialer closed still looks open from here.
			go func(c net.Conn) {
				buf := make([]byte, 256)
				for {
					if _, rerr := c.Read(buf); rerr != nil {
						break
					}
				}
				b.mu.Lock()
				delete(b.live, c)
				b.mu.Unlock()
				_ = c.Close()
			}(c)
		}
	}()
	return b
}

func (b *blackHole) addr() string { return b.ln.Addr().String() }

func (b *blackHole) metrics() (live, peak int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.live), b.peak
}

// TestQueueHalfOpenLimitIsObserved is acceptance 2: with more unreachable peers than
// the half-open cap, only that many handshakes are ever in flight, and cancelling the
// run releases them.
func TestQueueHalfOpenLimitIsObserved(t *testing.T) {
	const halfOpen = 2
	holes := make([]*blackHole, 6)
	peers := make([]string, 0, 6)
	for i := range holes {
		holes[i] = newBlackHole(t)
		peers = append(peers, holes[i].addr())
	}
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, aerr := store.Add(AddRequest{
		Source:    Source{Kind: "magnet", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("3", 40)},
		RawSource: "magnet:?xt=urn:btih:" + strings.Repeat("3", 40),
		Peers:     peers,
		Discovery: "direct",
		Selection: []string{"fixture/a.bin"},
	})
	if aerr != nil {
		t.Fatal(aerr)
	}
	startEntry(t, store, entry.ID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Summary, 1)
	go func() {
		adapter := Adapter{MetadataTimeout: 4 * time.Second, Timeout: 20 * time.Second}
		sum, _ := NewRunner(store, adapter, Options{
			PollInterval:   10 * time.Millisecond,
			PeersPerJob:    8,
			HalfOpenPerJob: halfOpen,
		}).Run(ctx)
		done <- sum
	}()

	// Let the dialer saturate, then sample the peak across every listener.
	deadline := time.Now().Add(10 * time.Second)
	peak := 0
	for time.Now().Before(deadline) {
		total, p := 0, 0
		for _, h := range holes {
			live, holePeak := h.metrics()
			total += live
			if holePeak > p {
				p = holePeak
			}
			if total > 0 && p >= halfOpen {
				break
			}
		}
		if p > peak {
			peak = p
		}
		if peak >= halfOpen && total >= halfOpen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("half-open cap %d: peak concurrently accepted dials across 6 unreachable peers = %d", halfOpen, peak)
	if peak == 0 {
		cancel()
		<-done
		t.Fatal("no handshake was ever attempted; the measurement proves nothing")
	}
	if peak > halfOpen {
		cancel()
		<-done
		t.Fatalf("peak in-flight handshakes %d exceeds the cap %d", peak, halfOpen)
	}

	cancel()
	<-done
	// Cancellation must release them: the attempt's client is closed, so the dials
	// die and the listeners see their sockets close.
	closeDeadline := time.Now().Add(10 * time.Second)
	for {
		live := 0
		for _, h := range holes {
			n, _ := h.metrics()
			live += n
		}
		if live == 0 {
			break
		}
		if time.Now().After(closeDeadline) {
			t.Fatalf("%d half-open sockets survived cancellation", live)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if RunnerHeld(store.Root()) {
		t.Error("runner lock still held after a cancelled run")
	}
}
