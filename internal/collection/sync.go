package collection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sk3y04/provenance-engine/internal/app"
	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
	"github.com/sk3y04/provenance-engine/internal/session"
	"github.com/sk3y04/provenance-engine/internal/version"
)

type SyncOptions struct {
	DryRun      bool
	Record      bool
	RateLimiter *ratelimit.Manager
}

// Seams for hermetic tests; production always uses the real paths.
var (
	scanFn     = dispatcher.Scan
	downloadFn = app.Download
)

func Sync(ctx context.Context, name string, opts SyncOptions) (*SyncResult, error) {
	c, err := Get(name)
	if err != nil {
		return nil, err
	}
	return syncCollection(ctx, &c, opts)
}

func SyncAll(ctx context.Context, opts SyncOptions) error {
	all, err := List()
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range all {
		if cerr := ctx.Err(); cerr != nil {
			if firstErr == nil {
				firstErr = cerr
			}
			break
		}
		result, err := syncCollection(ctx, &c, opts)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		_ = result
	}
	return firstErr
}

func syncCollection(ctx context.Context, c *Collection, opts SyncOptions) (*SyncResult, error) {
	dOpts := dispatcher.Options{
		Config:      c.Options,
		RateLimiter: opts.RateLimiter,
	}

	site, err := dispatcher.Classify(c.URL)
	if err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}

	m, err := scanFn(ctx, c.URL, dOpts)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	c.Site = site.String()

	seen := c.SeenIDs
	if seen == nil {
		seen = make(map[string]bool)
	}
	newURLs, _, urlToItem, total, skipped := partitionNewItems(m.Items, seen)

	result := &SyncResult{
		Total:   total,
		Skipped: skipped,
		New:     len(newURLs),
	}

	if opts.DryRun {
		printDryRun(c.Name, result, newURLs)
		return result, nil
	}

	if len(newURLs) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "[provenance] collection %q: nothing to sync (%d total, %d skipped)\n", c.Name, total, skipped)
		return result, nil
	}

	sessName := fmt.Sprintf("collect-%s-%s", c.Name, time.Now().Format("20060102-150405"))
	sess, err := session.OpenOrCreate(sessName, c.Options)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[provenance] collection %q: %d new, %d skipped (%d total) — downloading\n",
		c.Name, result.New, result.Skipped, result.Total)

	dOpts.Reporter = sess
	downloadErr := downloadFn(ctx, newURLs, "", dOpts)
	counts := sess.Counts()
	result.Failed = counts.Failed
	result.New = counts.Succeeded
	result.Skipped += counts.Skipped
	result.SessionName = sessName

	// Mark an item seen only when every one of its URLs succeeded, and do so
	// even on partial failure: succeeded posts must not re-download, while
	// failed ones stay retryable on the next sync.
	succeeded := make(map[string]bool)
	for _, e := range sess.EntriesByStatus(session.StatusSucceeded) {
		succeeded[e.URL] = true
	}
	if ids := fullySucceededIDs(urlToItem, succeeded); len(ids) > 0 {
		if err := AddSeen(c.Name, ids...); err != nil {
			return result, fmt.Errorf("save seen ids: %w", err)
		}
	}

	if err := RecordSync(c.Name, *result); err != nil {
		return result, fmt.Errorf("record sync: %w", err)
	}
	if downloadErr != nil {
		return result, fmt.Errorf("sync download: %w", downloadErr)
	}

	if opts.Record {
		_ = recordCapture(*c, sessName, urlToItem)
	}

	_, _ = fmt.Fprintf(os.Stderr, "[provenance] collection %q: synced — %d downloaded, %d skipped, %d failed\n",
		c.Name, result.New, result.Skipped, result.Failed)

	return result, nil
}

func itemID(item manifest.Item) string {
	if item.PostID != "" {
		return item.PostID
	}
	if item.ID != "" {
		return item.ID
	}
	return ""
}

// fullySucceededIDs returns the item identities whose every new URL succeeded.
// A post carrying several media files is only marked seen when all of them
// downloaded, so a partial failure never drops the missing files from future
// syncs. The result is sorted for deterministic persistence.
func fullySucceededIDs(urlToItem map[string]manifest.Item, succeeded map[string]bool) []string {
	needed := make(map[string]int)
	done := make(map[string]int)
	for url, item := range urlToItem {
		id := itemID(item)
		if id == "" {
			id = url
		}
		needed[id]++
		if succeeded[url] {
			done[id]++
		}
	}
	ids := make([]string, 0, len(needed))
	for id, n := range needed {
		if done[id] == n {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// partitionNewItems splits scanned items into not-yet-seen URLs/IDs and the
// skipped remainder. Item identity prefers PostID (so every media file from one
// post shares an identity, e.g. for hashtag search results), then ID, then URL.
// A hashtag collection therefore skips all already-recorded posts on the next
// sync regardless of how many media files each carries.
func partitionNewItems(items []manifest.Item, seen map[string]bool) (newURLs, newIDs []string, urlToItem map[string]manifest.Item, total, skipped int) {
	urlToItem = make(map[string]manifest.Item, len(items))
	for _, item := range items {
		total++
		id := itemID(item)
		if id == "" {
			id = item.URL
		}
		if seen[id] {
			skipped++
			continue
		}
		if item.URL == "" {
			continue
		}
		newURLs = append(newURLs, item.URL)
		newIDs = append(newIDs, id)
		urlToItem[item.URL] = item
	}
	return newURLs, newIDs, urlToItem, total, skipped
}

func recordCapture(c Collection, sessName string, urlToItem map[string]manifest.Item) error {
	cm := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    c.URL,
		Site:         c.Site,
		DownloadedAt: time.Now().UTC(),
		OutputDir:    c.Options.OutputDir,
		Tool:         "provenance",
		ToolVersion:  version.Version,
		Options:      config.CaptureOptionsFromConfig(c.Options),
	}

	runPath := manifest.RunPath(c.Options.OutputDir)

	for _, item := range urlToItem {
		dest := item.Destination
		if dest == "" {
			dest = filepath.Join(c.Options.OutputDir, item.Filename)
		}
		fullPath := dest
		if !filepath.IsAbs(fullPath) {
			fullPath = filepath.Join(c.Options.OutputDir, dest)
		}

		ci := manifest.CaptureItem{
			ExternalID:     item.ID,
			URL:            item.URL,
			Title:          item.Title,
			Author:         item.Creator,
			Kind:           item.Kind,
			Extractor:      item.Source,
			DownloadedPath: dest,
			CapturedAt:     time.Now().UTC(),
			Status:         "ok",
			SessionName:    sessName,
		}
		if item.PostID != "" {
			ci.ExternalID = item.PostID
		}
		if !item.PublishedAt.IsZero() {
			t := item.PublishedAt
			ci.PublishedAt = &t
		}

		if fi, err := os.Stat(fullPath); err == nil {
			ci.ByteSize = fi.Size()
		}

		if item.Kind == "post" && item.Extension == "md" {
			if postContent, err := os.ReadFile(fullPath); err == nil {
				ci.Text = &manifest.TextCapture{
					Body:   string(postContent),
					Format: "markdown",
				}
			}
		}

		hash, err := manifest.FileSha256(fullPath)
		if err == nil {
			ci.Sha256 = hash
		}

		extID := ci.ExternalID
		if extID == "" {
			extID = item.Filename
		}
		if extID != "" {
			_ = manifest.WriteCaptureItem(c.Options.OutputDir, extID, &ci)
		}
		cm.Items = append(cm.Items, ci)
	}

	_ = manifest.WriteCaptureManifest(c.Options.OutputDir, runPath, cm)
	fmt.Fprintf(os.Stderr, "[provenance] recorded %d capture items to %s/.provenance/\n", len(cm.Items), c.Options.OutputDir)
	return nil
}

func printDryRun(name string, result *SyncResult, urls []string) {
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] collection %q: dry-run — %d total, %d new, %d skipped\n",
		name, result.Total, result.New, result.Skipped)
	if len(urls) > 0 {
		limit := len(urls)
		if limit > 20 {
			limit = 20
		}
		for i := 0; i < limit; i++ {
			_, _ = fmt.Fprintf(os.Stderr, "  %s\n", trim(urls[i], 100))
		}
		if len(urls) > 20 {
			_, _ = fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(urls)-20)
		}
	}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}
