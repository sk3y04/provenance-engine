package bittorrent

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// The library dials an explicit peer once, drops it when the handshake fails,
// and never retries that address. A peer that is not ready yet — a reference
// client that reports a torrent complete before it accepts peer connections, a
// peer restarting, a firewall that opens a second later — therefore used to stall
// the fetch until the deadline. waitComplete now re-offers the configured peers
// while nothing is connected.

// dropFirstProxy is a TCP forwarder that closes the first connection it accepts,
// standing in for a peer that is not ready at first contact.
type dropFirstProxy struct {
	ln     net.Listener
	target string
	// dropN is how many connections to close unanswered. The library itself
	// retries once with the other handshake flavour, so at least two are needed
	// before the peer is dropped for good.
	dropN int

	mu       sync.Mutex
	accepted int
	dropped  int
}

func newDropFirstProxy(t *testing.T, target string, dropN int) *dropFirstProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &dropFirstProxy{ln: ln, target: target, dropN: dropN}
	t.Cleanup(func() { _ = ln.Close() })
	go p.serve()
	return p
}

func (p *dropFirstProxy) addr() string { return p.ln.Addr().String() }

func (p *dropFirstProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.accepted++
		drop := p.accepted <= p.dropN
		p.mu.Unlock()
		if drop {
			p.mu.Lock()
			p.dropped++
			p.mu.Unlock()
			_ = conn.Close()
			continue
		}
		go p.forward(conn)
	}
}

func (p *dropFirstProxy) forward(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	copyDir := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyDir(upstream, client)
	go copyDir(client, upstream)
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

func (p *dropFirstProxy) stats() (accepted, dropped int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted, p.dropped
}

// TestExplicitPeerIsReofferedAfterAFailedHandshake proves the retry: the first
// connection to the peer is dropped before any handshake, and the fetch still
// completes, which can only happen if the peer was dialled a second time.
func TestExplicitPeerIsReofferedAfterAFailedHandshake(t *testing.T) {
	seed := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(30000, 53)})
	// Both of the library's own handshake attempts are dropped, which is what
	// makes it give up on the address; only the adapter's re-offer can recover.
	proxy := newDropFirstProxy(t, seed.peer(), 2)

	staging := t.TempDir()
	sess := discSession(t, Config{
		StagingDir:      staging,
		Peers:           []string{proxy.addr()},
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		Timeout:         90 * time.Second,
		MaxPeers:        4,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	cap, err := sess.Fetch(ctx, seed.magnet(), []string{"fixture/a.bin"})
	elapsed := time.Since(started)
	if err != nil {
		accepted, dropped := proxy.stats()
		t.Fatalf("fetch through a peer that dropped one connection: %v (proxy accepted=%d dropped=%d)", err, accepted, dropped)
	}
	if cap.BytesRead <= 0 {
		t.Errorf("BytesRead = %d, want bytes from the peer", cap.BytesRead)
	}
	if len(cap.Assets) != 1 || cap.Assets[0].SHA256 != sha256Hex(patterned(30000, 53)) {
		t.Errorf("capture = %+v, want the fixture digest", cap.Assets)
	}
	accepted, dropped := proxy.stats()
	if dropped != 2 {
		t.Errorf("proxy dropped %d connections, want exactly %d", dropped, 2)
	}
	if accepted < 3 {
		t.Errorf("proxy accepted %d connections, want at least 3 (two handshake flavours, then the retry)", accepted)
	}
	// The retry cannot happen sooner than the interval, so this is a lower bound
	// that proves the fetch was recovered by the re-offer and not by luck.
	if elapsed < peerRetryInterval {
		t.Errorf("fetch completed in %s, before the %s retry interval: the peer was not dropped", elapsed, peerRetryInterval)
	}
}

// TestHealthyPeerIsNotRedialled is the other half: a peer that answers the first
// time must not be re-dialled on the retry interval, so a healthy transfer does
// not churn connections.
func TestHealthyPeerIsNotRedialled(t *testing.T) {
	seed := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(24000, 59)})
	counted := newCountingProxy(t, seed.peer())

	sess := discSession(t, Config{
		StagingDir:      t.TempDir(),
		Peers:           []string{counted.addr()},
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 20 * time.Second,
		Timeout:         90 * time.Second,
		MaxPeers:        4,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := sess.Fetch(ctx, seed.magnet(), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// The transfer finishes in well under one retry interval, so this asserts the
	// retry does not fire while a peer is active rather than counting a race.
	if got := counted.count(); got != 1 {
		t.Errorf("peer was dialled %d times during a healthy fetch, want 1", got)
	}
}

// countingProxy forwards every connection and counts them.
type countingProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	total int
}

func newCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &countingProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.total++
			p.mu.Unlock()
			go p.forward(conn)
		}
	}()
	return p
}

func (p *countingProxy) addr() string { return p.ln.Addr().String() }

func (p *countingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

func (p *countingProxy) forward(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
}
