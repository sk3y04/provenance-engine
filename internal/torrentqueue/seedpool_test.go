package torrentqueue

// BT-10a.1: the seed pool must join a dispatched attempt before it exits.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// poolSession is a seed whose Close signals and then blocks.
type poolSession struct {
	serveEntered chan struct{}
	closeEntered chan struct{}
	closeHold    chan struct{}
	serveOnce    sync.Once
	closeOnce    sync.Once
}

func newPoolSession() *poolSession {
	return &poolSession{
		serveEntered: make(chan struct{}),
		closeEntered: make(chan struct{}),
		closeHold:    make(chan struct{}),
	}
}

func (p *poolSession) Serve(ctx context.Context) error {
	p.serveOnce.Do(func() { close(p.serveEntered) })
	<-ctx.Done()
	return ctx.Err()
}

func (p *poolSession) Address() string      { return "127.0.0.1:1" }
func (p *poolSession) UploadedBytes() int64 { return 0 }

func (p *poolSession) Close() error {
	p.closeOnce.Do(func() { close(p.closeEntered) })
	<-p.closeHold
	return nil
}

func TestSeedPoolJoinsClosingAttempt(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(32<<10, 139)}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin")
	if _, uerr := store.Update(entry.ID, func(e *Entry) error {
		e.Phase = PhaseCompleted
		e.Capture = &CaptureRef{Key: "capture-fixture"}
		e.SeedDesired = SeedOn
		e.SeedPhase = SeedWaiting
		return nil
	}); uerr != nil {
		t.Fatalf("make completed: %v", uerr)
	}

	sess := newPoolSession()
	t.Cleanup(func() {
		// Always release the barrier so a failed assertion cannot strand the pool.
		select {
		case <-sess.closeHold:
		default:
			close(sess.closeHold)
		}
	})
	r := NewRunner(store, &fakeSeedOpener{Adapter: realAdapter(), next: sess},
		Options{MaxSeeds: 1, PollInterval: 10 * time.Millisecond, Log: nil})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var sum Summary
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		r.runSeeds(ctx, &mu, &sum)
	}()

	select {
	case <-sess.serveEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("the pool never dispatched the seed")
	}

	// Ask it to stop: Serve returns, Close blocks.
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-sess.closeEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("the attempt never reached Close")
	}

	assertPoolStillRunning := func(stage string) {
		t.Helper()
		select {
		case <-poolDone:
			t.Fatalf("the seed pool returned while an attempt was still in Close (%s)", stage)
		case <-time.After(300 * time.Millisecond):
		}
	}
	assertPoolStillRunning("before cancellation")
	cancel()
	assertPoolStillRunning("after cancellation")

	close(sess.closeHold)
	select {
	case <-poolDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the pool never returned after Close finished")
	}
}

// TestRunJoinsClosingSeedAttempt is the same invariant one level up: Run must not
// return while a dispatched seed attempt still owns its client.
func TestRunJoinsClosingSeedAttempt(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := newLoopbackSeeder(t, "alpha", map[string][]byte{"a.bin": patternedBytes(32<<10, 149)}, 0)
	entry := enqueue(t, store, seed, "alpha/a.bin")
	if _, uerr := store.Update(entry.ID, func(e *Entry) error {
		e.Phase = PhaseCompleted
		e.Capture = &CaptureRef{Key: "capture-fixture"}
		e.SeedDesired = SeedOn
		e.SeedPhase = SeedWaiting
		return nil
	}); uerr != nil {
		t.Fatalf("make completed: %v", uerr)
	}

	sess := newPoolSession()
	t.Cleanup(func() {
		select {
		case <-sess.closeHold:
		default:
			close(sess.closeHold)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		_, rerr := NewRunner(store, &fakeSeedOpener{Adapter: realAdapter(), next: sess},
			Options{MaxActive: 1, MaxSeeds: 1, PollInterval: 10 * time.Millisecond}).Run(ctx)
		runDone <- rerr
	}()

	select {
	case <-sess.serveEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never dispatched the seed")
	}
	if _, serr := store.SetSeedIntent(entry.ID, false); serr != nil {
		t.Fatalf("unseed: %v", serr)
	}
	select {
	case <-sess.closeEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("the attempt never reached Close")
	}

	check := func(stage string) {
		t.Helper()
		select {
		case rerr := <-runDone:
			t.Fatalf("Run returned (%v) while a seed client was still in Close (%s)", rerr, stage)
		case <-time.After(300 * time.Millisecond):
		}
	}
	check("before cancellation")
	// A competing runner must not be able to acquire ownership while an attempt is
	// still closing.
	if probe, lerr := store.LockRunner(); lerr == nil {
		_ = probe.Release()
		t.Fatal("runner ownership was released while an attempt still owned a client")
	}
}
