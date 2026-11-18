package extractor

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/sk3y04/provenance-engine/internal/capability"
	"github.com/sk3y04/provenance-engine/internal/downloader"
)

// Deps are the engine-owned dependencies an operation uses. A long-lived agent or
// worker runs many operations in one process, so mutable state must not be
// process-global: every mutable value an operation can change lives here.
// Immutable constants stay package-level, and the package-level default client is
// a concurrency-safe shared service used only when no engine-owned Deps is
// supplied (the CLI path).
type Deps struct {
	HTTPClient *http.Client
	QueryIDs   *QueryIDCache

	// mu guards Tools and the resolved tool paths: Capabilities may re-probe
	// while operations read the report. Once a Deps is shared across
	// goroutines, read the report through ToolAvailable and FfmpegLocation,
	// never through the fields directly. AutoInstall is set before the Deps
	// is published and is immutable afterwards.
	mu          sync.RWMutex
	Tools       capability.Set
	FfmpegPath  string
	FFprobePath string
	ChromePath  string
	// AutoInstall permits a tool to be downloaded into the local cache. The
	// engine leaves it false so a hosted operation cannot mutate the filesystem;
	// the CLI keeps its historical auto-install behavior.
	AutoInstall bool
}

// NewDeps builds engine-owned dependencies. The transport is the shared,
// concurrency-safe service; the query-ID cache and tool paths are per-engine.
func NewDeps() *Deps {
	d := &Deps{
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: Transport(), CheckRedirect: downloader.SafeRedirect},
		QueryIDs:   &QueryIDCache{},
	}
	d.Probe("")
	return d
}

// Probe refreshes the tool report for this engine and copies the resolved paths
// onto the Deps. It never installs a tool. It is safe to call concurrently with
// operations reading the report through ToolAvailable and FfmpegLocation.
func (d *Deps) Probe(chromePath string) capability.Set {
	set := capability.Probe(context.Background(), chromePath)
	ffmpeg := set.Path(capability.ToolFfmpeg)
	ffprobe := set.Path(capability.ToolFFprobe)
	chrome := chromePath
	if chrome == "" {
		chrome = set.Path(capability.ToolChrome)
	}
	d.mu.Lock()
	d.Tools = set
	d.FfmpegPath = ffmpeg
	d.FFprobePath = ffprobe
	d.ChromePath = chrome
	d.mu.Unlock()
	return set
}

// ToolAvailable reports whether the named tool was reachable at the last
// probe. It is safe to call concurrently with Probe.
func (d *Deps) ToolAvailable(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.Tools.Available(name)
}

// FfmpegLocation returns the ffmpeg path resolved by the last probe, or ""
// when ffmpeg is not reachable. It is safe to call concurrently with Probe.
func (d *Deps) FfmpegLocation() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.FfmpegPath
}

// Default returns the package-level shared dependencies used by the CLI path.
// The engine always supplies its own Deps, so this default exists only for
// callers that have no engine.
func Default() *Deps { return defaultDeps }

var defaultDeps = NewDeps()

// depsFor returns d when an engine supplied one, otherwise the shared default.
func depsFor(d *Deps) *Deps {
	if d == nil {
		return defaultDeps
	}
	return d
}

// Transport returns the shared, immutable, concurrency-safe transport used by
// the extractors.
func Transport() *http.Transport {
	return sharedTransport
}

var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           downloader.NewDialer().DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

// QueryIDCache is the per-engine cache of X/Twitter GraphQL query IDs and the
// public guest bearer discovered from the web client bundle. It is concurrency
// safe and no longer process-global.
type QueryIDCache struct {
	mu               sync.RWMutex
	userByScreenName string
	userMedia        string
	userTweets       string
	searchTimeline   string
	guestBearer      string
}

func (c *QueryIDCache) get(field string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch field {
	case "UserByScreenName":
		return c.userByScreenName
	case "UserMedia":
		return c.userMedia
	case "UserTweets":
		return c.userTweets
	case "SearchTimeline":
		return c.searchTimeline
	}
	return ""
}

func (c *QueryIDCache) set(field, value string) {
	if c == nil || value == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch field {
	case "UserByScreenName":
		c.userByScreenName = value
	case "UserMedia":
		c.userMedia = value
	case "UserTweets":
		c.userTweets = value
	case "SearchTimeline":
		c.searchTimeline = value
	}
}

func (c *QueryIDCache) bearer() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.guestBearer
}

func (c *QueryIDCache) setBearer(value string) {
	if c == nil || value == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guestBearer = value
}

// Invalidate clears discovered query IDs after a stale-query failure.
func (c *QueryIDCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userByScreenName = ""
	c.userMedia = ""
	c.userTweets = ""
	c.searchTimeline = ""
}
