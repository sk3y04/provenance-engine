package slskd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults for one search.
const (
	DefaultSearchTimeoutMS   = 15000
	DefaultSearchWallClock   = 35 * time.Second
	DefaultSearchSettleGrace = 5 * time.Second
	DefaultPollInitial       = time.Second
	DefaultPollMax           = 3 * time.Second
	stoppedFetchTimeout      = 5 * time.Second
)

// SearchOptions configures one search.
type SearchOptions struct {
	Query string
	// TimeoutMS is sent to slskd verbatim. It is MILLISECONDS: both the upstream
	// Swagger and SearchRequest.cs claim seconds, and the running binary proves
	// otherwise (docs/findings-slskd-0.26-contract.md §2).
	TimeoutMS int
	// WallClock bounds the whole search. slskd resets its own timeout on every
	// peer response, so a popular query can run far past TimeoutMS; only a
	// wall-clock stop bounds it.
	WallClock                time.Duration
	FileLimit                int
	ResponseLimit            int
	MinimumPeerUploadSpeed   int
	MinimumResponseFileCount int
	// SettleGrace delays DELETE; deleting immediately after a poll can leave
	// slskd's search.db in a concurrency error that returns empty results
	// (#1819).
	SettleGrace time.Duration
	PollInitial time.Duration
	PollMax     time.Duration
	// IgnoredPeers are dropped entirely (download-side blocklist).
	IgnoredPeers []string
	// MaxResults bounds the number of ranked groups returned; 0 means all.
	MaxResults int
}

// ResultGroup is one peer's folder of matching files.
type ResultGroup struct {
	Username          string
	Directory         string
	Files             []File
	LockedFiles       int
	TotalBytes        int64
	UploadSpeed       int
	HasFreeUploadSlot bool
	QueueLength       int
	Score             float64
}

// SearchResult is the outcome of one search.
type SearchResult struct {
	SearchID        string
	Query           string
	Started         time.Time
	Duration        time.Duration
	Stopped         bool
	RawFileCount    int
	LockedFileCount int
	Groups          []ResultGroup
}

// SearchCoordinator runs searches one at a time.
//
// slskd permits exactly one concurrent search per instance and answers 429 to a
// second one (#1483). Serialising in-process is the only way to keep that from
// surfacing to a user as a throttle error, and it also makes the settle-then-
// delete bookkeeping correct.
type SearchCoordinator struct {
	client *Client
	now    func() time.Time

	// runMu serialises whole searches.
	runMu sync.Mutex
	// cleanup tracks deferred DELETE calls so a shutdown (or a test) can wait.
	cleanup sync.WaitGroup
}

// NewSearchCoordinator returns a coordinator over c.
func NewSearchCoordinator(c *Client) *SearchCoordinator {
	return &SearchCoordinator{client: c, now: func() time.Time { return time.Now().UTC() }}
}

// Client exposes the underlying client for callers that need the same profile.
func (sc *SearchCoordinator) Client() *Client { return sc.client }

// WaitCleanup blocks until every deferred search deletion has finished.
func (sc *SearchCoordinator) WaitCleanup(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		sc.cleanup.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Search runs one search to completion.
//
// A cancelled context stops the search in slskd and returns the partial result
// with Stopped set, because results already received are still useful and the
// caller asked to stop, not to fail.
func (sc *SearchCoordinator) Search(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, errors.New("search query is empty")
	}
	opts = withSearchDefaults(opts)

	sc.runMu.Lock()
	defer sc.runMu.Unlock()

	started := sc.now()
	searchID := newUUID()
	req := SearchRequest{
		ID:              searchID,
		SearchText:      query,
		SearchTimeoutMS: opts.TimeoutMS,
		FileLimit:       opts.FileLimit,
		ResponseLimit:   opts.ResponseLimit,
	}
	if opts.MinimumPeerUploadSpeed > 0 {
		req.MinimumPeerUploadSpeed = opts.MinimumPeerUploadSpeed
	}
	if opts.MinimumResponseFileCount > 0 {
		req.MinimumResponseFileCount = opts.MinimumResponseFileCount
	}
	filter := true
	req.FilterResponses = &filter

	created, err := sc.createWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	if created.ID != "" {
		searchID = created.ID
	}

	deadline := time.Now().Add(opts.WallClock)
	result := &SearchResult{SearchID: searchID, Query: query, Started: started}
	current := created
	poll := opts.PollInitial

	for !current.IsComplete {
		if time.Now().After(deadline) {
			// slskd's own timeout resets on every peer response; stop it
			// explicitly so it cannot run for minutes after we stopped caring.
			_ = sc.client.StopSearch(ctx, searchID)
			result.Stopped = true
			break
		}
		select {
		case <-ctx.Done():
			_ = sc.client.StopSearch(context.WithoutCancel(ctx), searchID)
			result.Stopped = true
		case <-time.After(poll):
		}
		if result.Stopped {
			break
		}
		next, err := sc.client.GetSearch(ctx, searchID, true)
		if err != nil {
			if ctx.Err() != nil {
				result.Stopped = true
				break
			}
			// A poll failure after the search was created: keep what we had and
			// report the failure, because the durable side effect (the search
			// record) already exists in slskd.
			return nil, fmt.Errorf("poll search %s: %w", searchID, err)
		}
		current = next
		if poll < opts.PollMax {
			poll *= 2
			if poll > opts.PollMax {
				poll = opts.PollMax
			}
		}
	}

	if result.Stopped {
		// The poll that preceded the stop cannot have seen responses that
		// arrived after it. Fetch once more with a detached context so a stopped
		// search still reports what the peers sent instead of discarding the
		// final window.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stoppedFetchTimeout)
		if final, ferr := sc.client.GetSearch(fetchCtx, searchID, true); ferr == nil && final != nil {
			current = final
		}
		cancel()
	}

	result.RawFileCount = current.FileCount
	result.LockedFileCount = current.LockedFileCount
	result.Duration = sc.now().Sub(started)
	result.Groups = rankGroups(current.Responses, opts.IgnoredPeers, opts.MaxResults)

	sc.scheduleDelete(searchID, opts.SettleGrace)
	return result, nil
}

// createWithRetry retries the single create call that slskd throttles. A 429 here
// is expected when a second process shares the instance; backing off is cheaper
// than failing a user's search.
func (sc *SearchCoordinator) createWithRetry(ctx context.Context, req SearchRequest) (*Search, error) {
	backoff := 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		s, err := sc.client.CreateSearch(ctx, req)
		if err == nil {
			if s == nil {
				return &Search{ID: req.ID}, nil
			}
			return s, nil
		}
		lastErr = err
		if !IsStatus(err, http.StatusTooManyRequests) {
			return nil, err
		}
		if attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, fmt.Errorf("slskd is throttling searches: %w", lastErr)
}

// scheduleDelete removes the search after the settle grace (#1819). The delete is
// detached from the caller's context: a search that took a user's whole session
// must still be cleaned up.
func (sc *SearchCoordinator) scheduleDelete(searchID string, grace time.Duration) {
	sc.cleanup.Add(1)
	go func() {
		defer sc.cleanup.Done()
		if grace > 0 {
			time.Sleep(grace)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sc.client.DeleteSearch(ctx, searchID)
	}()
}

func withSearchDefaults(opts SearchOptions) SearchOptions {
	if opts.TimeoutMS <= 0 {
		opts.TimeoutMS = DefaultSearchTimeoutMS
	}
	if opts.TimeoutMS < 5 {
		// slskd validates a minimum of 5; below that it rejects the request.
		opts.TimeoutMS = 5
	}
	if opts.WallClock <= 0 {
		opts.WallClock = DefaultSearchWallClock
	}
	if opts.SettleGrace == 0 {
		opts.SettleGrace = DefaultSearchSettleGrace
	}
	if opts.SettleGrace < 0 {
		opts.SettleGrace = 0
	}
	if opts.PollInitial <= 0 {
		opts.PollInitial = DefaultPollInitial
	}
	if opts.PollMax < opts.PollInitial {
		opts.PollMax = DefaultPollMax
	}
	if opts.FileLimit <= 0 {
		opts.FileLimit = 10000
	}
	if opts.ResponseLimit <= 0 {
		opts.ResponseLimit = 100
	}
	return opts
}

// rankGroups turns slskd's per-peer responses into ranked folders, dropping
// locked-only folders and ignored peers, then applies the result cap.
func rankGroups(responses []SearchResponse, ignored []string, maxResults int) []ResultGroup {
	blocked := make(map[string]bool, len(ignored))
	for _, p := range ignored {
		blocked[strings.ToLower(strings.TrimSpace(p))] = true
	}
	groups := map[string]*ResultGroup{}
	for _, resp := range responses {
		if blocked[strings.ToLower(resp.Username)] {
			continue
		}
		// Files a peer has locked cannot be downloaded without a grant; they are
		// counted but never offered.
		all := make([]File, 0, len(resp.Files)+len(resp.LockedFiles))
		all = append(all, resp.Files...)
		all = append(all, resp.LockedFiles...)
		for _, f := range all {
			if f.Size <= 0 {
				continue
			}
			dir := remoteDirectory(f.Filename)
			key := strings.ToLower(resp.Username) + "\x00" + strings.ToLower(dir)
			g, ok := groups[key]
			if !ok {
				g = &ResultGroup{
					Username:          resp.Username,
					Directory:         dir,
					UploadSpeed:       resp.UploadSpeed,
					HasFreeUploadSlot: resp.HasFreeUploadSlot,
					QueueLength:       resp.QueueLength,
				}
				groups[key] = g
			}
			if f.IsLocked {
				g.LockedFiles++
				continue
			}
			g.Files = append(g.Files, f)
			g.TotalBytes += f.Size
		}
	}
	out := make([]ResultGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.Files) == 0 {
			continue
		}
		sort.Slice(g.Files, func(i, j int) bool { return g.Files[i].Filename < g.Files[j].Filename })
		g.Score = scoreGroup(*g)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Username != out[j].Username {
			return out[i].Username < out[j].Username
		}
		return out[i].Directory < out[j].Directory
	})
	if maxResults > 0 && len(out) > maxResults {
		out = out[:maxResults]
	}
	return out
}

// scoreGroup ranks a folder by what actually affects a download: a free slot
// beats everything, then peer speed, then a shallow queue, then breadth.
func scoreGroup(g ResultGroup) float64 {
	score := 0.0
	if g.HasFreeUploadSlot {
		score += 1000
	}
	score += float64(minInt(g.UploadSpeed/1000, 500))
	score -= float64(minInt(g.QueueLength, 100))
	score += float64(minInt(len(g.Files), 20))
	return score
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// remoteDirectory returns the parent portion of a remote path, normalised to
// forward slashes for display. slskd reports Windows-style separators.
func remoteDirectory(remote string) string {
	norm := strings.ReplaceAll(remote, "\\", "/")
	idx := strings.LastIndex(norm, "/")
	if idx < 0 {
		return ""
	}
	return norm[:idx]
}

// RemoteBaseName returns the final path element of a remote path.
func RemoteBaseName(remote string) string {
	norm := strings.ReplaceAll(remote, "\\", "/")
	idx := strings.LastIndex(norm, "/")
	if idx < 0 {
		return norm
	}
	return norm[idx+1:]
}

// newUUID returns a random RFC-4122 version 4 UUID.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is unrecoverable for id generation; fall back to a
		// timestamp-derived value rather than returning a silently duplicated id.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
