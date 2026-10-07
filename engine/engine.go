package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sk3y04/provenance-engine/internal/app"
	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
	ievent "github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/extractor"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
	"github.com/sk3y04/provenance-engine/internal/resolve"
	"github.com/sk3y04/provenance-engine/internal/worker"
)

// sinkBuffer is the bounded queue used to decouple engine emission from a
// caller's EventSink. When the queue is full events are dropped rather than
// stalling extraction.
const sinkBuffer = 256

// Engine executes Resolve and Download operations. Construct it with New and
// reuse it; it is safe for concurrent use.
type Engine struct {
	cfg     Config
	limiter *ratelimit.Manager
}

// New validates cfg, applies safe defaults, and constructs an Engine. It
// returns an Error with Kind ErrorValidation when cfg is unusable.
func New(cfg Config) (*Engine, error) {
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return nil, &Error{Kind: ErrorValidation, Op: "new", Err: errors.New("a work directory is required")}
	}
	if cfg.Quality == "" {
		cfg.Quality = DefaultQuality
	}
	switch strings.ToLower(cfg.Quality) {
	case "best", "1080", "720", "480":
	default:
		return nil, &Error{Kind: ErrorValidation, Op: "new", Err: fmt.Errorf("unsupported quality %q", cfg.Quality)}
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxItems < 1 {
		cfg.MaxItems = DefaultMaxItems
	}
	if cfg.SpeedLimit < 0 {
		return nil, &Error{Kind: ErrorValidation, Op: "new", Err: errors.New("speed limit must not be negative")}
	}
	if cfg.Timeout < 0 {
		return nil, &Error{Kind: ErrorValidation, Op: "new", Err: errors.New("timeout must not be negative")}
	}
	return &Engine{cfg: cfg, limiter: ratelimit.New()}, nil
}

// Config returns a copy of the effective configuration, including defaults.
func (e *Engine) Config() Config { return e.cfg }

// Resolve discovers a supported public source without downloading.
//
// The returned Source contains only exported facade types. A nil sink is safe.
// Inputs are validated at the boundary; failures are returned as *Error with a
// stable Kind.
func (e *Engine) Resolve(ctx context.Context, req ResolveRequest, sink EventSink) (src Source, err error) {
	defer func() {
		if r := recover(); r != nil {
			src = Source{}
			err = e.panicErr("resolve", req.URL, ctx, r)
		}
	}()

	if err = checkContext("resolve", req.URL, ctx); err != nil {
		return Source{}, err
	}
	if err = e.validateURL("resolve", req.URL); err != nil {
		return Source{}, err
	}
	if err = e.validateFilter("resolve", req.URL, req.Filter); err != nil {
		return Source{}, err
	}

	limit := e.effectiveLimit(req.Limit)
	ctx, cancel := e.withTimeout(ctx)
	defer cancel()

	events, closeSink := e.sink(sink)
	defer closeSink()

	opts := e.options(req.Filter, limit, false, false, 0)
	opts.Events = events

	sources, rerr := app.ScanResolved(ctx, []string{req.URL}, opts)
	if rerr != nil {
		return Source{}, e.wrapError("resolve", req.URL, rerr)
	}
	if len(sources) == 0 {
		return Source{}, nil
	}
	return fromSource(sources[0]), nil
}

// Download resolves a supported public source as needed and writes artifacts
// under Config.WorkDir, returning typed metadata for every artifact produced.
//
// A nil sink is safe. Inputs are validated at the boundary; failures are
// returned as *Error with a stable Kind. The Result is populated even when a
// non-nil error is returned, so partial counts, warnings, and artifacts remain
// available to the caller.
func (e *Engine) Download(ctx context.Context, req DownloadRequest, sink EventSink) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{}
			err = e.panicErr("download", req.URL, ctx, r)
		}
	}()

	if err = checkContext("download", req.URL, ctx); err != nil {
		return Result{}, err
	}
	if err = e.validateURL("download", req.URL); err != nil {
		return Result{}, err
	}
	if err = e.validateFilter("download", req.URL, req.Filter); err != nil {
		return Result{}, err
	}

	limit := e.effectiveLimit(req.Limit)
	ctx, cancel := e.withTimeout(ctx)
	defer cancel()

	warnings := &warningCollector{}
	events, closeSink := e.sink(warnings.wrap(sink))
	defer closeSink()

	counts := &countingReporter{}
	opts := e.options(req.Filter, limit, req.IncludePosts, req.IncludeComments, req.CommentLimit)
	opts.Reporter = counts
	opts.Events = events

	downloadErr := app.Download(ctx, []string{req.URL}, "", opts)
	// Drain queued events before reading warnings so trailing warnings are
	// captured. Close is idempotent, so the deferred call is safe.
	closeSink()

	res = Result{
		Source:   Source{URL: req.URL, CanonicalURL: req.URL},
		Counts:   counts.snapshot(),
		Warnings: warnings.list(),
	}
	if artifacts, aerr := collectArtifacts(e.cfg.WorkDir); aerr != nil {
		res.Warnings = append(res.Warnings, "artifact scan failed: "+aerr.Error())
	} else {
		res.Artifacts = artifacts
	}
	if downloadErr != nil {
		return res, e.wrapError("download", req.URL, downloadErr)
	}
	return res, nil
}

// options maps facade configuration and per-request fields onto the internal
// dispatcher options. Raw yt-dlp arguments and output templates are
// deliberately not exposed by the facade and remain unset.
func (e *Engine) options(f FilterOptions, limit int, includePosts, includeComments bool, commentLimit int) dispatcher.Options {
	return dispatcher.Options{
		Config: config.Config{
			OutputDir:          e.cfg.WorkDir,
			CookiesFile:        e.cfg.CookiesFile,
			CookiesFromBrowser: e.cfg.CookiesFromBrowser,
			Concurrency:        e.cfg.Concurrency,
			Quality:            e.cfg.Quality,
			AudioOnly:          e.cfg.AudioOnly,
			NoArchive:          e.cfg.NoArchive,
			Filter:             toManifestFilter(f),
			PostLimit:          limit,
			IncludePosts:       includePosts,
			IncludeComments:    includeComments,
			CommentLimit:       commentLimit,
			SpeedLimit:         e.cfg.SpeedLimit,
			ChromePath:         e.cfg.ChromePath,
			OutputLayout:       e.cfg.OutputLayout,
		},
		RateLimiter: e.limiter,
	}
}

// effectiveLimit clamps a request limit into the configured bound. A
// non-positive or oversized request limit becomes Config.MaxItems.
func (e *Engine) effectiveLimit(limit int) int {
	if limit <= 0 || limit > e.cfg.MaxItems {
		return e.cfg.MaxItems
	}
	return limit
}

// withTimeout derives the operation context, adding Config.Timeout when set.
func (e *Engine) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e.cfg.Timeout > 0 {
		return context.WithTimeout(ctx, e.cfg.Timeout)
	}
	return context.WithCancel(ctx)
}

// sink adapts a public EventSink to the internal event sink and wraps it in a
// bounded, panic-recovering notifier. A nil public sink becomes a discarding
// sink so the reusable paths never fall back to terminal output.
func (e *Engine) sink(sink EventSink) (ievent.Sink, func()) {
	if sink == nil {
		return ievent.Nop(), func() {}
	}
	adapted := ievent.Func(func(ctx context.Context, ev ievent.Event) {
		sink.Emit(ctx, fromEvent(ev))
	})
	notifier := ievent.NewNotifier(adapted, sinkBuffer)
	return notifier, notifier.Close
}

func checkContext(op, rawURL string, ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return &Error{Kind: ErrorCanceled, Op: op, URL: rawURL, Err: err}
	}
	return nil
}

func (e *Engine) validateURL(op, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return &Error{Kind: ErrorValidation, Op: op, URL: raw, Err: errors.New("url is required")}
	}
	// Bare hashtags are a supported Twitter/X source and are not URLs.
	if extractor.IsTwitterHashtagSource(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return &Error{Kind: ErrorValidation, Op: op, URL: raw, Err: fmt.Errorf("invalid url: %w", err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &Error{Kind: ErrorValidation, Op: op, URL: raw, Err: fmt.Errorf("unsupported url scheme %q", u.Scheme)}
	}
	if u.Host == "" {
		return &Error{Kind: ErrorValidation, Op: op, URL: raw, Err: errors.New("url has no host")}
	}
	return nil
}

func (e *Engine) validateFilter(op, rawURL string, f FilterOptions) error {
	if f.TitleMatch != "" {
		if _, err := regexp.Compile(f.TitleMatch); err != nil {
			return &Error{Kind: ErrorValidation, Op: op, URL: rawURL, Err: fmt.Errorf("title-match regex: %w", err)}
		}
	}
	if f.TitleReject != "" {
		if _, err := regexp.Compile(f.TitleReject); err != nil {
			return &Error{Kind: ErrorValidation, Op: op, URL: rawURL, Err: fmt.Errorf("title-exclude regex: %w", err)}
		}
	}
	if f.MinSize < 0 || f.MaxSize < 0 {
		return &Error{Kind: ErrorValidation, Op: op, URL: rawURL, Err: errors.New("size filters must not be negative")}
	}
	if f.MinSize > 0 && f.MaxSize > 0 && f.MinSize > f.MaxSize {
		return &Error{Kind: ErrorValidation, Op: op, URL: rawURL, Err: errors.New("min-size exceeds max-size")}
	}
	return nil
}

// wrapError maps an internal error onto a public categorized *Error.
// Cancellation is never re-categorized. Unknown causes map to ErrorTemporary so
// they remain retryable by default; callers should rely on the typed Kind.
func (e *Engine) wrapError(op, rawURL string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrorCanceled, Op: op, URL: rawURL, Err: err}
	}
	if worker.IsPermanent(err) {
		return &Error{Kind: ErrorPermanent, Op: op, URL: rawURL, Err: err}
	}
	if kind := engineerr.KindOf(err); kind != "" {
		return &Error{Kind: mapKind(kind), Op: op, URL: rawURL, Err: err}
	}
	return &Error{Kind: ErrorTemporary, Op: op, URL: rawURL, Err: err}
}

func mapKind(kind engineerr.Kind) ErrorKind {
	switch kind {
	case engineerr.Validation:
		return ErrorValidation
	case engineerr.AuthRequired:
		return ErrorAuthRequired
	case engineerr.UnsupportedSource:
		return ErrorUnsupportedSource
	case engineerr.RateLimited:
		return ErrorRateLimited
	case engineerr.Temporary:
		return ErrorTemporary
	case engineerr.Canceled:
		return ErrorCanceled
	case engineerr.ExternalTool:
		return ErrorExternalTool
	case engineerr.Permanent:
		return ErrorPermanent
	default:
		return ErrorTemporary
	}
}

// panicErr converts a recovered panic (for example from a third-party
// executable installer) into a categorized error so a hosted worker cannot
// crash on it. If the operation's context is done, cancellation wins.
func (e *Engine) panicErr(op, rawURL string, ctx context.Context, r any) error {
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			return &Error{Kind: ErrorCanceled, Op: op, URL: rawURL, Err: cerr}
		}
	}
	return &Error{Kind: ErrorPermanent, Op: op, URL: rawURL, Err: fmt.Errorf("engine panic: %v", r)}
}

func toManifestFilter(f FilterOptions) manifest.FilterOptions {
	return manifest.FilterOptions{
		IncludeExt:  f.IncludeExt,
		ExcludeExt:  f.ExcludeExt,
		MinSize:     f.MinSize,
		MaxSize:     f.MaxSize,
		TitleMatch:  f.TitleMatch,
		TitleReject: f.TitleReject,
	}
}

func fromSource(s resolve.Source) Source {
	out := Source{
		URL:          s.URL,
		CanonicalURL: s.CanonicalURL,
		Kind:         SourceKind(s.Kind),
		Extractor:    s.Extractor,
		Title:        s.Title,
		Author:       s.Author,
	}
	if len(s.Items) > 0 {
		out.Items = make([]Item, 0, len(s.Items))
		for _, item := range s.Items {
			out.Items = append(out.Items, fromItem(item))
		}
	}
	return out
}

func fromItem(it resolve.Item) Item {
	out := Item{
		ExternalID:  it.ExternalID,
		URL:         it.URL,
		Title:       it.Title,
		Author:      it.Author,
		PublishedAt: it.PublishedAt,
	}
	if len(it.Media) > 0 {
		out.Media = make([]MediaAsset, 0, len(it.Media))
		for _, m := range it.Media {
			out.Media = append(out.Media, fromMedia(m))
		}
	}
	if it.Text != nil {
		out.Text = &TextContent{Body: it.Text.Body, Format: TextFormat(it.Text.Format)}
	}
	return out
}

func fromMedia(m resolve.MediaAsset) MediaAsset {
	return MediaAsset{
		URL:       m.URL,
		Filename:  m.Filename,
		Extension: m.Extension,
		Size:      m.Size,
		Kind:      MediaKind(m.Kind),
	}
}

func fromEvent(ev ievent.Event) Event {
	out := Event{
		Kind:    EventKind(ev.Kind),
		Stage:   Stage(ev.Stage),
		URL:     ev.URL,
		ItemRef: ev.ItemRef,
		Written: ev.Written,
		Total:   ev.Total,
		Attempt: ev.Attempt,
		Reason:  ev.Reason,
		Detail:  ev.Detail,
		Err:     ev.Err,
		At:      ev.At,
	}
	if ev.Summary != nil {
		out.Summary = &Counts{
			Discovered: ev.Summary.Discovered,
			Succeeded:  ev.Summary.Succeeded,
			Failed:     ev.Summary.Failed,
			Skipped:    ev.Summary.Skipped,
		}
	}
	return out
}

// countingReporter implements dispatcher.Reporter to tally URL lifecycle
// outcomes for Result.Counts without emitting duplicate events.
type countingReporter struct {
	discovered atomic.Int64
	succeeded  atomic.Int64
	failed     atomic.Int64
	skipped    atomic.Int64
}

func (r *countingReporter) Queue(string, string) { r.discovered.Add(1) }
func (r *countingReporter) Start(string)         {}
func (r *countingReporter) Success(string)       { r.succeeded.Add(1) }
func (r *countingReporter) Failure(string, error) {
	r.failed.Add(1)
}
func (r *countingReporter) Skip(string, string) { r.skipped.Add(1) }

func (r *countingReporter) snapshot() Counts {
	return Counts{
		Discovered: r.discovered.Load(),
		Succeeded:  r.succeeded.Load(),
		Failed:     r.failed.Load(),
		Skipped:    r.skipped.Load(),
	}
}

// warningCollector records warning facts while forwarding events to an
// optional caller sink.
type warningCollector struct {
	mu       sync.Mutex
	warnings []string
}

func (c *warningCollector) wrap(sink EventSink) EventSink {
	return SinkFunc(func(ctx context.Context, ev Event) {
		if ev.Kind == EventWarning {
			msg := ev.Detail
			if msg == "" {
				msg = ev.Reason
			}
			if msg != "" {
				c.mu.Lock()
				c.warnings = append(c.warnings, msg)
				c.mu.Unlock()
			}
		}
		if sink != nil {
			sink.Emit(ctx, ev)
		}
	})
}

func (c *warningCollector) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.warnings...)
}

// engineBookkeepingDirs are directories the engine creates for its own use and
// never reports as artifacts.
var engineBookkeepingDirs = map[string]bool{
	"_provenance_cache": true,
	".provenance":       true,
}

// collectArtifacts walks a controlled WorkDir and reports every regular file,
// excluding engine bookkeeping directories and in-progress partial files.
func collectArtifacts(root string) ([]Artifact, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("empty work dir")
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []Artifact
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && engineBookkeepingDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = name
		}
		sum, herr := sha256File(path)
		if herr != nil {
			return herr
		}
		abs, aerr := filepath.Abs(path)
		if aerr != nil {
			abs = path
		}
		out = append(out, Artifact{
			Path:     abs,
			Filename: filepath.ToSlash(rel),
			Size:     info.Size(),
			SHA256:   sum,
			MIMEType: mimeOf(name),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func mimeOf(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
