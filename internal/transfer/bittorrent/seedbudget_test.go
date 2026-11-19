package bittorrent

// BT-08d.2: two verified SeedSessions serving two controlled leechers under one
// shared upload limiter, with an independent-limiter control that must fail the
// same criterion.
//
// Declared before the assertions, so the numbers are not chosen after the fact:
//
//	rate          512 KiB/s per limiter
//	burst         1 MiB (the runner's UploadBurstBytes)
//	per seed      2 MiB of deterministic payload (greater than the burst)
//	counter       tor.Stats().BytesWrittenData summed over each seed's torrents:
//	              payload bytes the client wrote to peers, which is what the upload
//	              path reserves tokens for. It is NOT tokens charged (requests are
//	              reserved before sends, sockets buffer), and NOT what the leecher
//	              read; the leecher's own counter is reported beside it.
//	window        first served byte -> last leecher verifying its final piece
//	tolerance     25% on the shared envelope, plus a separate rate comparison
//
// The discriminator is the achieved rate, not the total: both cases move the same
// bytes, so a shared budget must stretch the window to roughly half the speed.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	at "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"
)

// sessionMetainfo is the metainfo of a serving session, normalised so another
// client can add it (an empty, non-nil piece-layer table is rejected).
func sessionMetainfo(t *testing.T, s *SeedSession) *metainfo.MetaInfo {
	t.Helper()
	mi := *s.Metainfo()
	if len(mi.PieceLayers) == 0 {
		mi.PieceLayers = nil
	}
	return &mi
}

const (
	budgetRate    = 512 << 10
	budgetBurst   = 1 << 20
	budgetPerSeed = 2 << 20
	budgetToler   = 1.25
)

// startServing runs Serve and waits until the serving client has an address, so a
// leecher never dials an empty one.
func startServing(t *testing.T, s *SeedSession) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 1)
	go func() { _ = s.Serve(ctx); done <- struct{}{} }()
	ctlWait(t, "serving address", 60*time.Second, func() bool { return s.Address() != "" },
		func() string { return "no address; listening=" + strconv.FormatBool(s.Listening()) })
	return func() { cancel(); <-done }
}

// budgetRun is one measured configuration.
type budgetRun struct {
	aggregate int64
	perSeed   [2]int64
	leechRead int64
	window    time.Duration
	overlap   bool
	elapsed   time.Duration
}

// achievedRate is bytes above the burst, spread over the measurement window.
func (r budgetRun) achievedRate() float64 {
	extra := float64(r.aggregate - budgetBurst)
	if extra < 0 {
		extra = 0
	}
	if r.window <= 0 {
		return 0
	}
	return extra / r.window.Seconds()
}

// seedBudgetFixture prepares one verified workspace of budgetPerSeed bytes.
func seedBudgetFixture(t *testing.T, seedByte byte) (ws, jobID string, payload []byte) {
	t.Helper()
	payload = patterned(budgetPerSeed, seedByte)
	files := map[string][]byte{"a.bin": payload}
	seedRoot := t.TempDir()
	source := ctlMetainfo(t, seedRoot, "fixture", files)
	peer := ctlServe(t, seedRoot, "fixture", source)
	root := t.TempDir()
	magnet := "magnet:?xt=urn:btih:" + source.HashInfoBytes().HexString()
	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, peer.addr), magnet, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("fetch fixture: %v", err)
	}
	return cap.StagingDir, cap.JobID, payload
}

// runBudgetCase serves two workspaces with either one shared limiter or one each.
func runBudgetCase(t *testing.T, shared bool) budgetRun {
	t.Helper()
	wsA, jobA, payloadA := seedBudgetFixture(t, 71)
	wsB, jobB, payloadB := seedBudgetFixture(t, 73)

	limA := rate.NewLimiter(rate.Limit(budgetRate), budgetBurst)
	limB := limA
	if !shared {
		limB = rate.NewLimiter(rate.Limit(budgetRate), budgetBurst)
	}
	cfgA := isolatedConfig(wsA)
	cfgA.UploadLimiter = limA
	sessA, err := OpenSeedSession(context.Background(), cfgA, jobA)
	if err != nil {
		t.Fatalf("open seed A: %v", err)
	}
	t.Cleanup(func() { _ = sessA.Close() })
	cfgB := isolatedConfig(wsB)
	cfgB.UploadLimiter = limB
	sessB, err := OpenSeedSession(context.Background(), cfgB, jobB)
	if err != nil {
		t.Fatalf("open seed B: %v", err)
	}
	t.Cleanup(func() { _ = sessB.Close() })

	stopA := startServing(t, sessA)
	stopB := startServing(t, sessB)
	defer func() { stopA(); stopB() }()

	start := time.Now()
	// Measurement starts before either leecher exists, so nothing is missed.
	baseA, baseB := sessA.UploadedBytes(), sessB.UploadedBytes()
	leechA := ctlLeech(t, t.TempDir(), sessionMetainfo(t, sessA), sessA.Address())
	leechB := ctlLeech(t, t.TempDir(), sessionMetainfo(t, sessB), sessB.Address())

	var firstByteAt time.Time
	var overlap bool
	deadline := time.Now().Add(180 * time.Second)
	prevA, prevB := baseA, baseB
	for {
		aNow, bNow := sessA.UploadedBytes(), sessB.UploadedBytes()
		dA, dB := aNow-prevA, bNow-prevB
		if dA > 0 && dB > 0 {
			overlap = true
		}
		prevA, prevB = aNow, bNow
		if firstByteAt.IsZero() && (aNow > baseA || bNow > baseB) {
			firstByteAt = time.Now()
		}
		if leechA.allPiecesComplete() && leechB.allPiecesComplete() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leechers did not finish: seedA=%d seedB=%d %s | %s",
				aNow-baseA, bNow-baseB, leechA.describe(), leechB.describe())
		}
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	window := elapsed
	if !firstByteAt.IsZero() {
		window = time.Since(firstByteAt)
	}

	run := budgetRun{
		aggregate: (sessA.UploadedBytes() - baseA) + (sessB.UploadedBytes() - baseB),
		perSeed:   [2]int64{sessA.UploadedBytes() - baseA, sessB.UploadedBytes() - baseB},
		leechRead: leecherTotal(leechA) + leecherTotal(leechB),
		window:    window,
		overlap:   overlap,
		elapsed:   elapsed,
	}
	// Both sides delivered the right bytes.
	for i, tc := range []struct {
		root    string
		payload []byte
	}{{leechA.root, payloadA}, {leechB.root, payloadB}} {
		got, rerr := os.ReadFile(filepath.Join(tc.root, "fixture", "a.bin"))
		if rerr != nil {
			t.Fatalf("leecher %d read: %v", i, rerr)
		}
		if string(got) != string(tc.payload) {
			t.Fatalf("leecher %d payload differs (got %d bytes)", i, len(got))
		}
	}
	return run
}

// TestSeedAggregateUploadBudget is the gate: one shared limiter across two serving
// seeds stays inside burst + rate x window, and an independent-limiter control of
// the same workload clearly does not.
func TestSeedAggregateUploadBudget(t *testing.T) {
	shared := runBudgetCase(t, true)
	t.Logf("shared limiter: aggregate=%d (A=%d B=%d) window=%s elapsed=%s overlap=%v leecherRead=%d achieved=%.0f B/s",
		shared.aggregate, shared.perSeed[0], shared.perSeed[1], shared.window.Round(time.Millisecond),
		shared.elapsed.Round(time.Millisecond), shared.overlap, shared.leechRead, shared.achievedRate())

	if shared.aggregate < 2*budgetPerSeed {
		t.Fatalf("aggregate %d is below the %d bytes both seeds hold", shared.aggregate, 2*budgetPerSeed)
	}
	if !shared.overlap {
		t.Fatalf("no interval had both seeds serving: the run was serialised, not aggregated")
	}
	allowance := float64(budgetBurst) + float64(budgetRate)*shared.window.Seconds()
	if float64(shared.aggregate) > allowance*budgetToler {
		t.Errorf("aggregate %d exceeds the shared allowance %.0f (+%d%%)", shared.aggregate, allowance, 25)
	}
	if got := shared.achievedRate(); got > float64(budgetRate)*1.3 {
		t.Errorf("achieved %.0f B/s exceeds one budget of %d B/s", got, budgetRate)
	}

	indep := runBudgetCase(t, false)
	t.Logf("independent limiters: aggregate=%d (A=%d B=%d) window=%s overlap=%v leecherRead=%d achieved=%.0f B/s",
		indep.aggregate, indep.perSeed[0], indep.perSeed[1], indep.window.Round(time.Millisecond),
		indep.overlap, indep.leechRead, indep.achievedRate())

	// The control has to be distinguishable, or the shared verdict is meaningless.
	sharedAllowance := float64(budgetBurst) + float64(budgetRate)*indep.window.Seconds()
	if float64(indep.aggregate) <= sharedAllowance*budgetToler {
		t.Fatalf("inconclusive: the independent-limiter run (%d bytes in %s) also fits the shared envelope (%.0f)",
			indep.aggregate, indep.window.Round(time.Millisecond), sharedAllowance)
	}
	if indep.achievedRate() < shared.achievedRate()*1.35 {
		t.Fatalf("inconclusive: independent rate %.0f B/s is not clearly above the shared rate %.0f B/s",
			indep.achievedRate(), shared.achievedRate())
	}
}

func leecherTotal(l *ctlPeer) int64 {
	counter := l.client.Stats().BytesReadUsefulData
	return counter.Int64()
}

// TestSeedStopOneKeepsSibling: stopping one throttled seed releases its workspace and
// leaves the sibling serving on the same limiter instance.
func TestSeedStopOneKeepsSibling(t *testing.T) {
	wsA, jobA, _ := seedBudgetFixture(t, 79)
	wsB, jobB, _ := seedBudgetFixture(t, 81)
	limiter := rate.NewLimiter(rate.Limit(budgetRate), budgetBurst)
	cfgA := isolatedConfig(wsA)
	cfgA.UploadLimiter = limiter
	sessA, err := OpenSeedSession(context.Background(), cfgA, jobA)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	cfgB := isolatedConfig(wsB)
	cfgB.UploadLimiter = limiter
	sessB, err := OpenSeedSession(context.Background(), cfgB, jobB)
	if err != nil {
		_ = sessA.Close()
		t.Fatalf("open B: %v", err)
	}
	defer func() { _ = sessB.Close() }()

	stopA := startServing(t, sessA)
	stopB := startServing(t, sessB)

	leechB := ctlLeech(t, t.TempDir(), sessionMetainfo(t, sessB), sessB.Address())
	ctlWait(t, "sibling to start serving", 60*time.Second, func() bool {
		return sessB.UploadedBytes() > 0
	}, func() string { return leechB.describe() })
	before := sessB.UploadedBytes()

	stopA()
	if err := sessA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	assertWorkspaceFree(t, wsA)

	// The sibling is still serving, on the same limiter instance.
	ctlWait(t, "sibling to keep serving after the stop", 60*time.Second, func() bool {
		return sessB.UploadedBytes() > before
	}, func() string { return leechB.describe() })
	if limiter.Burst() != budgetBurst || limiter.Limit() != rate.Limit(budgetRate) {
		t.Fatalf("shared limiter was replaced: burst=%d limit=%v", limiter.Burst(), limiter.Limit())
	}
	stopB()
	if err := sessB.Close(); err != nil {
		t.Fatalf("close B: %v", err)
	}
	assertWorkspaceFree(t, wsB)
}

// TestSeedVerificationDoesNotFetchOrServe is the startup gate. During verification a
// client exists but has no listener and dials nothing, so there is no peer path to
// abuse - proven here structurally (listener count, port, hook observation) rather
// than by a sleep and a zero byte count.
func TestSeedVerificationDoesNotFetchOrServe(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(12<<20, 83)}
	seedRoot := t.TempDir()
	source := ctlMetainfo(t, seedRoot, "fixture", files)
	peer := ctlServe(t, seedRoot, "fixture", source)
	root := t.TempDir()
	magnet := "magnet:?xt=urn:btih:" + source.HashInfoBytes().HexString()
	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, peer.addr), magnet, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Observe every client created during startup, and try to reach it the way a peer
	// would: dial its port. A verification client must have none.
	type observation struct {
		listeners int
		port      int
		dialErr   error
	}
	var seen []observation
	prev := clientOpenHook
	clientOpenHook = func(client *at.Client) {
		o := observation{listeners: len(client.Listeners()), port: client.LocalPort()}
		conn, derr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(o.port), 300*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
		}
		o.dialErr = derr
		seen = append(seen, o)
	}
	t.Cleanup(func() { clientOpenHook = prev })

	sess, err := OpenSeedSession(context.Background(), isolatedConfig(cap.StagingDir), cap.JobID)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no client was observed during startup; the check proves nothing")
	}
	for i, o := range seen {
		if o.listeners != 0 {
			t.Fatalf("startup client %d opened %d listener(s): a peer could reach it before Serve", i, o.listeners)
		}
		if o.port != 0 {
			t.Fatalf("startup client %d bound port %d", i, o.port)
		}
		if o.dialErr == nil {
			t.Fatalf("startup client %d accepted a connection before Serve", i)
		}
	}
	// Nothing is reachable, nothing was served, nothing was fetched.
	if sess.Listening() {
		t.Fatal("the session reports a listener before Serve")
	}
	if addr := sess.Address(); addr != "" {
		t.Fatalf("the session advertises %q before Serve", addr)
	}
	if served := sess.UploadedBytes(); served != 0 {
		t.Fatalf("the session served %d bytes before Serve", served)
	}
	if fetched := seedFetchedBytes(sess); fetched != 0 {
		t.Fatalf("the seed fetched %d payload bytes during verification", fetched)
	}

	// After Serve the same verified data is available to a peer.
	serveCtx, stopServing := context.WithCancel(context.Background())
	served := make(chan struct{}, 1)
	go func() { _ = sess.Serve(serveCtx); served <- struct{}{} }()
	ctlWait(t, "serving listener and address", 30*time.Second, func() bool {
		return sess.Listening() && sess.Address() != ""
	}, func() string { return "listening=false address=" + sess.Address() })
	leech := ctlLeech(t, t.TempDir(), sessionMetainfo(t, sess), sess.Address())
	ctlWait(t, "post-Serve transfer", 120*time.Second, func() bool {
		return sess.UploadedBytes() > 0 && leech.allPiecesComplete()
	}, func() string { return leech.describe() })
	got, rerr := os.ReadFile(filepath.Join(leech.root, "fixture", "a.bin"))
	if rerr != nil {
		t.Fatalf("leecher read: %v", rerr)
	}
	if string(got) != string(files["a.bin"]) {
		t.Fatalf("leecher payload differs: %d bytes", len(got))
	}
	stopServing()
	<-served
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertWorkspaceFree(t, cap.StagingDir)
}

// seedFetchedBytes is payload this seed's client read from peers.
func seedFetchedBytes(s *SeedSession) int64 {
	if s.client == nil {
		return 0
	}
	var total int64
	for _, tor := range s.client.Torrents() {
		counter := tor.Stats().BytesReadUsefulData
		total += counter.Int64()
	}
	return total
}

// TestSeedCancelBetweenVerificationAndServe: a verified session that is closed without
// ever serving leaves no listener, no client and no lock.
func TestSeedCancelBetweenVerificationAndServe(t *testing.T) {
	ws, jobID, _ := seedBudgetFixture(t, 87)
	sess, err := OpenSeedSession(context.Background(), isolatedConfig(ws), jobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sess.Listening() {
		t.Fatal("listening before Serve")
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertWorkspaceFree(t, ws)
}

// TestSeedFailedHandoffLeavesNothingServing: Serve with an already-cancelled context
// builds the serving client and then aborts the handoff. Nothing may be left listening
// or holding the workspace.
func TestSeedFailedHandoffLeavesNothingServing(t *testing.T) {
	ws, jobID, _ := seedBudgetFixture(t, 89)
	sess, err := OpenSeedSession(context.Background(), isolatedConfig(ws), jobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sess.Serve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve on a cancelled context returned %v", err)
	}
	if sess.Listening() {
		t.Fatal("a failed handoff left a listener behind")
	}
	if sess.UploadedBytes() != 0 {
		t.Fatalf("a failed handoff served %d bytes", sess.UploadedBytes())
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertWorkspaceFree(t, ws)
}
