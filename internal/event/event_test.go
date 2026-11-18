package event

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNopAndNilSink(t *testing.T) {
	Nop().Emit(context.Background(), Event{Kind: KindProgress})
	Emit(context.Background(), nil, Event{Kind: KindProgress})
}

func TestFuncAdapter(t *testing.T) {
	var got []Event
	s := Func(func(_ context.Context, e Event) { got = append(got, e) })
	Emit(context.Background(), s, Event{Kind: KindWarning, Reason: "x"})
	if len(got) != 1 || got[0].Reason != "x" {
		t.Fatalf("got %+v", got)
	}
	if _, ok := Func(nil).(nopSink); !ok {
		t.Fatal("Func(nil) should be Nop")
	}
}

func TestEmitSetsTimestamp(t *testing.T) {
	var got Event
	Emit(context.Background(), Func(func(_ context.Context, e Event) { got = e }), Event{})
	if got.At.IsZero() {
		t.Fatal("Emit did not set At")
	}
}

func TestEmitDropsWhenContextDone(t *testing.T) {
	called := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Emit(ctx, Func(func(context.Context, Event) { called = true }), Event{})
	if called {
		t.Fatal("event emitted despite canceled context")
	}
}

func TestNotifierPreservesOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	n := NewNotifier(Func(func(_ context.Context, e Event) {
		mu.Lock()
		got = append(got, e.ItemRef)
		mu.Unlock()
	}), 64)
	for i := 0; i < 10; i++ {
		n.Emit(context.Background(), Event{Kind: KindItemDone, ItemRef: string(rune('a' + i))})
	}
	n.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 10 {
		t.Fatalf("got %d events, want 10", len(got))
	}
	for i, ref := range got {
		if want := string(rune('a' + i)); ref != want {
			t.Fatalf("event %d = %q, want %q (order not preserved)", i, ref, want)
		}
	}
}

func TestNotifierRecoversPanics(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	n := NewNotifier(Func(func(_ context.Context, e Event) {
		if e.Reason == "panic" {
			panic("boom")
		}
		mu.Lock()
		seen++
		mu.Unlock()
	}), 8)
	n.Emit(context.Background(), Event{Reason: "panic"})
	n.Emit(context.Background(), Event{Reason: "ok"})
	n.Close()
	mu.Lock()
	defer mu.Unlock()
	if seen != 1 {
		t.Fatalf("seen = %d, want 1 (panic must not stop delivery)", seen)
	}
}

func TestNotifierNeverBlocksOnSlowSink(t *testing.T) {
	block := make(chan struct{})
	n := NewNotifier(Func(func(context.Context, Event) { <-block }), 1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			n.Emit(context.Background(), Event{Kind: KindProgress})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a slow sink")
	}
	close(block)
	n.Close()
}

func TestNotifierCloseIdempotentAndDropsAfterClose(t *testing.T) {
	n := NewNotifier(Nop(), 4)
	n.Close()
	n.Close()
	n.Emit(context.Background(), Event{Kind: KindProgress})
}

// TestNotifierCloseTwiceWithBlockedSinkLeavesNoWaiters covers the shutdown
// path engine.Download uses (an explicit Close plus a deferred one) against a
// sink that violates the best-effort contract and blocks. Both Close calls
// must return within the grace bound, and while the sink stays blocked only
// the single delivery goroutine may remain — Close must not leave waiter
// goroutines behind that accumulate across operations.
func TestNotifierCloseTwiceWithBlockedSinkLeavesNoWaiters(t *testing.T) {
	base := runtime.NumGoroutine()
	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	n := NewNotifier(Func(func(context.Context, Event) {
		entered <- struct{}{}
		<-block
	}), 4)
	n.Emit(context.Background(), Event{Kind: KindProgress})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery never started")
	}

	for i := 0; i < 2; i++ {
		done := make(chan struct{})
		go func() {
			n.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeGrace + 2*time.Second):
			t.Fatalf("Close %d stalled on a blocked sink", i)
		}
	}

	// The sink is still blocked: only the delivery goroutine may remain.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= base+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Close left waiter goroutines behind: have %d, want <= %d", got, base+1)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Once the sink unblocks, delivery finishes and every goroutine exits.
	close(block)
	deadline = time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked after unblock: have %d, want <= %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
