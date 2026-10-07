// Package app provides the top-level download and scan coordination used by
// every transport - CLI, TUI, or any future interface.
package app

import (
	"context"
	"fmt"
	"os"

	"github.com/sk3y04/provenance-engine/internal/diagnose"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/engineerr"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/resolve"
)

// Download dispatches one or more URLs for downloading. When batchPath is
// non-empty the file (one URL per line) is dispatched via parallel worker
// pools; individual urls are dispatched sequentially. The first encountered
// error is returned but remaining work continues.
//
// When opts.Events is set, progress, item, and summary facts are emitted as
// structured events and nothing is written to the terminal. When it is nil the
// legacy CLI/TUI output is preserved unchanged.
func Download(ctx context.Context, urls []string, batchPath string, opts dispatcher.Options) error {
	if batchPath == "" && len(urls) == 0 {
		return engineerr.Newf(engineerr.Validation, "download", "", "provide at least one URL or use --batch <file>")
	}
	var firstErr error
	cr := dispatcher.NewCountsReporter(opts.Reporter)
	opts.Reporter = cr
	if batchPath != "" {
		if err := dispatcher.BatchDispatch(ctx, batchPath, opts); err != nil {
			firstErr = err
		}
	}
	for _, u := range urls {
		if err := dispatcher.Dispatch(ctx, u, opts); err != nil {
			if opts.Events != nil {
				event.Emit(ctx, opts.Events, event.Event{
					Kind: event.KindItemDone, URL: u, Err: err,
				})
			} else {
				fmt.Fprintf(os.Stderr, "[provenance] %s: %v\n", u, err)
				if hint := diagnose.Hint(err); hint != "" {
					fmt.Fprintf(os.Stderr, "[provenance] hint: %s\n", hint)
				}
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if opts.Events != nil {
		event.Emit(ctx, opts.Events, event.Event{
			Kind: event.KindSummary,
			Summary: &event.Counts{
				Discovered: cr.Counts.Discovered.Load(),
				Succeeded:  cr.Counts.Succeeded.Load(),
				Failed:     cr.Counts.Failed.Load(),
				Skipped:    cr.Counts.Skipped.Load(),
			},
		})
	} else {
		dispatcher.PrintSummary(&cr.Counts, opts.OutputDir, "")
	}
	return firstErr
}

// Scan discovers downloadable items for the given URLs without downloading.
func Scan(ctx context.Context, urls []string, opts dispatcher.Options) ([]manifest.Manifest, error) {
	var all []manifest.Manifest
	for _, u := range urls {
		m, err := dispatcher.Scan(ctx, u, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, nil
}

// ScanResolved discovers downloadable items using the shared resolve model,
// preserving raw platform metadata for downstream collection and archive use.
func ScanResolved(ctx context.Context, urls []string, opts dispatcher.Options) ([]resolve.Source, error) {
	var all []resolve.Source
	for _, u := range urls {
		s, err := dispatcher.ScanResolved(ctx, u, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, nil
}
