package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/sk3y04/provenance-engine/engine"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	ievent "github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/extractor"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/render"
)

// cliUnlimitedItems maps the CLI's unbounded default (no --limit) onto the
// facade's required positive MaxItems bound. The CLI is a trusted local tool,
// so a very large sentinel preserves its historical "no limit" behavior while
// still satisfying the facade contract that a positive bound always applies.
const cliUnlimitedItems = 1 << 30

// facadeGrabCompatible reports whether the current grab invocation can run
// through the public engine facade without changing behavior.
//
// The facade deliberately does not expose dry-run, batch files, sessions,
// filename templates, or non-standard quality values, and it rejects sources
// without an http(s) scheme or hashtag form. Invocations using any of those
// keep the legacy internal path. This is the documented transitional boundary;
// see docs/ENGINE_REFACTOR.md.
func facadeGrabCompatible(args []string) bool {
	if flagDryRun || flagBatch != "" || flagSession != "" || flagOutputTemplate != "" {
		return false
	}
	if !facadeQuality(flagQuality) {
		return false
	}
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		if !facadeURL(a) {
			return false
		}
	}
	return true
}

// facadeQuality reports whether q is one of the qualities the facade validates.
func facadeQuality(q string) bool {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "", "best", "1080", "720", "480":
		return true
	}
	return false
}

// facadeURL reports whether raw is a source the facade accepts: an http/https
// URL or a supported Twitter/X hashtag. Bare hosts, which legacy yt-dlp
// accepted, continue to use the legacy path.
func facadeURL(raw string) bool {
	if extractor.IsTwitterHashtagSource(raw) {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// engineConfigFromOptions maps the parsed CLI download config onto the public
// facade Config. OutputTemplate is intentionally absent because the facade does
// not expose it; callers with a template use the legacy path.
func engineConfigFromOptions(o dispatcher.Options) engine.Config {
	maxItems := cliUnlimitedItems
	if o.PostLimit > 0 {
		maxItems = o.PostLimit
	}
	return engine.Config{
		WorkDir:            o.OutputDir,
		CookiesFile:        o.CookiesFile,
		CookiesFromBrowser: o.CookiesFromBrowser,
		Quality:            o.Quality,
		AudioOnly:          o.AudioOnly,
		Concurrency:        o.Concurrency,
		SpeedLimit:         o.SpeedLimit,
		MaxItems:           maxItems,
		NoArchive:          o.NoArchive,
		OutputLayout:       o.OutputLayout,
		ChromePath:         o.ChromePath,
	}
}

func engineFilterFromOptions(f manifest.FilterOptions) engine.FilterOptions {
	return engine.FilterOptions{
		IncludeExt:  f.IncludeExt,
		ExcludeExt:  f.ExcludeExt,
		MinSize:     f.MinSize,
		MaxSize:     f.MaxSize,
		TitleMatch:  f.TitleMatch,
		TitleReject: f.TitleReject,
	}
}

// runGrabViaFacade executes the canonical Download operation of the public
// engine facade once per URL, renders structured events to stderr, and prints
// the same summary the legacy path produced. The first error is returned while
// remaining URLs still run, matching the legacy app.Download contract.
func runGrabViaFacade(ctx context.Context, args []string, opts dispatcher.Options) error {
	if len(args) == 0 {
		return errors.New("provide at least one URL or use --batch <file>")
	}
	eng, err := engine.New(engineConfigFromOptions(opts))
	if err != nil {
		return err
	}
	req := engine.DownloadRequest{
		Limit:           opts.PostLimit,
		Filter:          engineFilterFromOptions(opts.Filter),
		IncludePosts:    opts.IncludePosts,
		IncludeComments: opts.IncludeComments,
		CommentLimit:    opts.CommentLimit,
	}
	sink := newCLIEventSink(os.Stderr)

	var counts dispatcher.Counts
	var firstErr error
	for _, u := range args {
		req.URL = u
		res, derr := eng.Download(ctx, req, sink)
		counts.Discovered.Add(res.Counts.Discovered)
		counts.Succeeded.Add(res.Counts.Succeeded)
		counts.Failed.Add(res.Counts.Failed)
		counts.Skipped.Add(res.Counts.Skipped)
		if derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	dispatcher.PrintSummary(&counts, opts.OutputDir, "")
	return firstErr
}

// cliEventSink renders facade events to the CLI. Non-summary events use the
// shared render.Sink formatting; the summary event is suppressed so the caller
// can emit the exact legacy summary once, with the output directory, after all
// URLs complete.
type cliEventSink struct {
	inner *render.Sink
}

func newCLIEventSink(w io.Writer) *cliEventSink {
	return &cliEventSink{inner: render.NewSink(w)}
}

// Emit implements engine.EventSink.
func (s *cliEventSink) Emit(ctx context.Context, e engine.Event) {
	if e.Kind == engine.EventSummary {
		return
	}
	s.inner.Emit(ctx, engineEventToInternal(e))
}

func engineEventToInternal(e engine.Event) ievent.Event {
	out := ievent.Event{
		Kind:    ievent.Kind(e.Kind),
		Stage:   ievent.Stage(e.Stage),
		URL:     e.URL,
		ItemRef: e.ItemRef,
		Written: e.Written,
		Total:   e.Total,
		Attempt: e.Attempt,
		Reason:  e.Reason,
		Detail:  e.Detail,
		Err:     e.Err,
		At:      e.At,
	}
	if e.Summary != nil {
		out.Summary = &ievent.Counts{
			Discovered: e.Summary.Discovered,
			Succeeded:  e.Summary.Succeeded,
			Failed:     e.Summary.Failed,
			Skipped:    e.Summary.Skipped,
		}
	}
	return out
}

// exitCode maps an engine error to a process exit code. Every engine category
// currently maps to 1, preserving the historical 0-success/1-error contract.
// The switch documents the mapping and gives a single place to introduce
// distinct codes in a later phase.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	switch engine.ErrorKindOf(err) {
	case engine.ErrorValidation,
		engine.ErrorAuthRequired,
		engine.ErrorUnsupportedSource,
		engine.ErrorRateLimited,
		engine.ErrorTemporary,
		engine.ErrorCanceled,
		engine.ErrorExternalTool,
		engine.ErrorPermanent:
		return 1
	default:
		return 1
	}
}
