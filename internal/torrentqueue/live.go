package torrentqueue

// Live snapshots for the foreground runner (BT-10a).
//
// Read-only, local, and deliberately not an event stream: a client asks once, gets
// what the runner can observe about itself right now, and disconnects. The durable
// picture stays in the entry records; a snapshot never rewrites them.
//
// Transport is a user-private Unix socket. The socket lives in a short private
// temporary directory and the queue directory holds a small index file pointing at
// it, because a queue root can be far longer than the roughly 100-byte path limit of
// a Unix address. Neither the index file nor the socket is proof of a live runner:
// a client must connect and complete a handshake, and any failure is reported as "no
// reachable runner".
//
// No mutation is reachable over this endpoint: the only accepted request is a
// snapshot.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LiveFormat versions the snapshot contract.
const LiveFormat = "provenance/torrent-queue-live+1"

// ErrNoLiveRunner means no foreground runner answered for this root.
var ErrNoLiveRunner = errors.New("no foreground runner is reachable for this queue root")

// liveIndexName is the file a runner publishes under the queue directory. It names
// the socket; it is not itself a liveness claim.
const liveIndexName = "live.json"

// liveLimits bound a client and a reader so neither can hold a slot.
const (
	liveReadTimeout  = 2 * time.Second
	liveWriteTimeout = 2 * time.Second
	liveMaxRequest   = 4096
	liveMaxReaders   = 8
)

// LiveAttempt is one attempt the runner is running right now.
type LiveAttempt struct {
	EntryID string `json:"entry_id"`
	// Kind is "download" or "seed".
	Kind string `json:"kind"`
	// Phase is the runner's own observation: running, verifying, serving, stopping.
	Phase string `json:"phase"`
	// Stopping means the attempt has been asked to stop and still owns its slot
	// until its client closes. Kept as its own field so a reader never has to infer
	// it from a phase string.
	Stopping bool `json:"stopping"`
	// UploadedBytes is payload a seed has written. It is absent for downloads: this
	// slice has no concurrent-safe read path for their transfer counters, and a zero
	// would be a lie rather than a measurement.
	UploadedBytes *int64 `json:"uploaded_bytes,omitempty"`
	// SelectedBytes is the selected payload covered by verified pieces, and
	// SelectedTotal is the selected payload's full size. Both are absent when the
	// transfer cannot report them; absent means unknown, never zero.
	SelectedBytes *int64 `json:"selected_bytes,omitempty"`
	SelectedTotal *int64 `json:"selected_total,omitempty"`
	// ProgressRate is the change in SelectedBytes per second, in bytes/s. It is a
	// *progress* rate on the selected payload, not network throughput: it is absent
	// until two samples at least liveRateWindow apart exist, and it stays absent
	// after a counter rollback or a new attempt.
	ProgressRate *int64 `json:"progress_rate,omitempty"`
}

// LiveSlots reports used and configured slots for one pool.
type LiveSlots struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

// LiveSnapshot is one read-only observation of a runner.
type LiveSnapshot struct {
	Format    string        `json:"format"`
	Root      string        `json:"root"`
	PID       int           `json:"pid"`
	StartedAt time.Time     `json:"started_at"`
	Downloads LiveSlots     `json:"download_slots"`
	Seeds     LiveSlots     `json:"seed_slots"`
	Attempts  []LiveAttempt `json:"attempts"`
}

// liveRateWindow is the shortest baseline a progress rate is computed over.
// Samples arrive from the transfer's 100ms poll; a shorter window would turn
// scheduling noise into a spike, so a sample inside the window keeps the previous
// baseline instead of producing a rate.
const liveRateWindow = 250 * time.Millisecond

// liveSample is the baseline for one attempt's progress rate.
type liveSample struct {
	at       time.Time
	verified int64
}

// liveState is the runner's mutable view, guarded by one short mutex. Snapshots copy
// under it and serialize outside it, so a slow reader never delays a claim.
type liveState struct {
	mu        sync.Mutex
	root      string
	downloads LiveSlots
	seeds     LiveSlots
	attempts  map[string]LiveAttempt
	order     []string
	started   time.Time
	// samples holds one rate baseline per live attempt. It is reset when an attempt
	// is added or removed, so a retry can never inherit the previous attempt's rate.
	samples map[string]liveSample
	// now is the clock seam for tests; nil means time.Now (monotonic reading).
	now func() time.Time
}

func newLiveState(root string) *liveState {
	return &liveState{
		root:     root,
		attempts: map[string]LiveAttempt{},
		samples:  map[string]liveSample{},
		started:  time.Now().UTC(),
	}
}

// clock returns a monotonic reading: rate deltas must never be computed across a
// wall-clock step.
func (l *liveState) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// progress records one selected-payload sample for a live attempt. Semantics:
//   - no sample in this attempt yet -> bytes are reported, rate stays absent;
//   - verified counter went backwards -> rate absent and the baseline restarts;
//   - less than liveRateWindow since the baseline -> keep the old baseline so the
//     next rate covers the longer interval (no divide-by-tiny spike);
//   - otherwise the rate is the delta over the elapsed time, and a delta of zero is
//     recorded as a measured zero rather than omitted.
func (l *liveState) progress(id string, verified, total int64) {
	if verified < 0 || total < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[id]
	if !ok {
		// The attempt ended between the sample and this call: never resurrect it.
		return
	}
	v, t := verified, total
	a.SelectedBytes, a.SelectedTotal = &v, &t
	now := l.clock()
	prev, had := l.samples[id]
	switch {
	case !had || verified < prev.verified:
		a.ProgressRate = nil
		l.samples[id] = liveSample{at: now, verified: verified}
	case now.Sub(prev.at) >= liveRateWindow:
		rate := int64(float64(verified-prev.verified) / now.Sub(prev.at).Seconds())
		a.ProgressRate = &rate
		l.samples[id] = liveSample{at: now, verified: verified}
	}
	l.attempts[id] = a
}

func (l *liveState) add(id, kind, phase string) {
	l.mu.Lock()
	if _, ok := l.attempts[id]; !ok {
		l.order = append(l.order, id)
	}
	// A new attempt starts with a fresh rate baseline, even for the same entry.
	delete(l.samples, id)
	l.attempts[id] = LiveAttempt{EntryID: id, Kind: kind, Phase: phase}
	l.mu.Unlock()
}

func (l *liveState) phase(id, phase string) {
	l.mu.Lock()
	if a, ok := l.attempts[id]; ok {
		a.Phase = phase
		l.attempts[id] = a
	}
	l.mu.Unlock()
}

func (l *liveState) stopping(id string) {
	l.mu.Lock()
	if a, ok := l.attempts[id]; ok {
		a.Phase = "stopping"
		a.Stopping = true
		l.attempts[id] = a
	}
	l.mu.Unlock()
}

func (l *liveState) uploaded(id string, bytes int64) {
	l.mu.Lock()
	if a, ok := l.attempts[id]; ok {
		b := bytes
		a.UploadedBytes = &b
		l.attempts[id] = a
	}
	l.mu.Unlock()
}

func (l *liveState) remove(id string) {
	l.mu.Lock()
	if _, ok := l.attempts[id]; !ok {
		l.mu.Unlock()
		return
	}
	delete(l.attempts, id)
	delete(l.samples, id)
	for i, e := range l.order {
		if e == id {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
}

// snapshot copies the state. The caller serializes it.
func (l *liveState) snapshot() LiveSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Slots in use are derived from the registry itself: an attempt is registered when
	// it takes its slot and removed only after its client has closed, so a stopping
	// attempt still counts.
	var dl, sd int
	for _, a := range l.attempts {
		switch a.Kind {
		case "download":
			dl++
		case "seed":
			sd++
		}
	}
	downloads, seeds := l.downloads, l.seeds
	downloads.Used, seeds.Used = dl, sd
	out := LiveSnapshot{
		Format:    LiveFormat,
		Root:      l.root,
		PID:       os.Getpid(),
		StartedAt: l.started,
		Downloads: downloads,
		Seeds:     seeds,
		Attempts:  make([]LiveAttempt, 0, len(l.order)),
	}
	for _, id := range l.order {
		out.Attempts = append(out.Attempts, l.attempts[id])
	}
	return out
}

// liveServer owns the endpoint for one runner.
type liveServer struct {
	state    *liveState
	dir      string
	sockPath string
	ln       net.Listener
	index    string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

// startLiveServer publishes an endpoint under the queue directory. It must only be
// called by the process that holds runner ownership.
func startLiveServer(root string, state *liveState) (*liveServer, error) {
	dir, err := os.MkdirTemp("", "provenance-live-")
	if err != nil {
		return nil, fmt.Errorf("live endpoint directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	sock := filepath.Join(dir, "q.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("live endpoint: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &liveServer{
		state: state, dir: dir, sockPath: sock, ln: ln,
		index: filepath.Join(root, filepath.FromSlash(queueDirName), liveIndexName),
		conns: map[net.Conn]struct{}{},
		slots: make(chan struct{}, liveMaxReaders),
		done:  make(chan struct{}),
	}
	idx, _ := json.Marshal(map[string]any{
		"format": LiveFormat,
		"pid":    os.Getpid(),
		"socket": sock,
	})
	if err := os.WriteFile(s.index, idx, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("publish live endpoint: %w", err)
	}
	go s.serve()
	return s, nil
}

func (s *liveServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			// Too many readers already: refuse this one immediately rather than
			// letting readers accumulate.
			_, _ = conn.Write([]byte("{\"error\":\"too many live readers\"}\n"))
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func(c net.Conn) {
			defer func() {
				<-s.slots
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
				s.wg.Done()
			}()
			s.handle(c)
		}(conn)
	}
}

// handle answers exactly one request and never mutates anything.
func (s *liveServer) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(liveReadTimeout))
	_ = c.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
	reader := bufio.NewReader(io.LimitReader(c, liveMaxRequest))
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return
	}
	var req struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil || req.Op != "snapshot" {
		_, _ = c.Write([]byte("{\"error\":\"unsupported request\"}\n"))
		return
	}
	snap := s.state.snapshot()
	body, merr := json.Marshal(snap)
	if merr != nil {
		_, _ = c.Write([]byte("{\"error\":\"cannot serialize snapshot\"}\n"))
		return
	}
	_, _ = c.Write(append(body, '\n'))
}

// close stops accepting, drops open readers and removes the endpoint. It is bounded:
// a reader that never finishes cannot hold shutdown.
func (s *liveServer) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		finished := make(chan struct{})
		go func() { s.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
		if idx, err := os.ReadFile(s.index); err == nil && strings.Contains(string(idx), s.sockPath) {
			_ = os.Remove(s.index)
		}
		_ = os.RemoveAll(s.dir)
	})
}

// requestLive asks a runner for one snapshot. Any failure means "no reachable
// runner": a leftover socket, index file or lock is not evidence of one.
func requestLive(root string) (LiveSnapshot, error) {
	var zero LiveSnapshot
	idxPath := filepath.Join(root, filepath.FromSlash(queueDirName), liveIndexName)
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		return zero, fmt.Errorf("%w (index: %v)", ErrNoLiveRunner, err)
	}
	var idx struct {
		Socket string `json:"socket"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil || idx.Socket == "" {
		return zero, fmt.Errorf("%w (index decode: %v)", ErrNoLiveRunner, err)
	}
	conn, err := net.DialTimeout("unix", idx.Socket, liveReadTimeout)
	if err != nil {
		return zero, fmt.Errorf("%w (dial %s: %v)", ErrNoLiveRunner, idx.Socket, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(liveReadTimeout))
	if _, err := io.WriteString(conn, "{\"op\":\"snapshot\"}\n"); err != nil {
		return zero, fmt.Errorf("%w (write: %v)", ErrNoLiveRunner, err)
	}
	// One bounded read: the server writes exactly one line and closes, and a partial
	// read still carries the whole document because it is far below the buffer.
	buf := make([]byte, 64<<10)
	n, rerr := conn.Read(buf)
	if n == 0 {
		return zero, fmt.Errorf("%w (empty reply, err %v)", ErrNoLiveRunner, rerr)
	}
	if rerr != nil && rerr != io.EOF {
		return zero, fmt.Errorf("%w (read: %v)", ErrNoLiveRunner, rerr)
	}
	body := buf[:n]
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Error != "" {
		return zero, fmt.Errorf("runner refused the snapshot: %s", probe.Error)
	}
	var snap LiveSnapshot
	if err := json.Unmarshal(body, &snap); err != nil || snap.Format != LiveFormat {
		return zero, fmt.Errorf("%w (decode %q: %v)", ErrNoLiveRunner, body, err)
	}
	return snap, nil
}

// LiveSnapshotFor asks the runner that owns this queue root for one snapshot. Any
// failure - no index, no socket, a dead process, a timeout - is reported as
// ErrNoLiveRunner, because none of those files is proof of a live runner.
func LiveSnapshotFor(ctx context.Context, root string) (LiveSnapshot, error) {
	return liveRequestWithContext(ctx, root)
}

// liveRequestWithContext is requestLive with a caller-supplied deadline.
func liveRequestWithContext(ctx context.Context, root string) (LiveSnapshot, error) {
	type result struct {
		snap LiveSnapshot
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		snap, err := requestLive(root)
		ch <- result{snap, err}
	}()
	select {
	case r := <-ch:
		return r.snap, r.err
	case <-ctx.Done():
		var zero LiveSnapshot
		return zero, ErrNoLiveRunner
	}
}
