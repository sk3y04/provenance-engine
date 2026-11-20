package collection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/app"
	"github.com/sk3y04/provenance-engine/internal/config"
	"github.com/sk3y04/provenance-engine/internal/dispatcher"
	"github.com/sk3y04/provenance-engine/internal/filelock"
	"github.com/sk3y04/provenance-engine/internal/manifest"
	"github.com/sk3y04/provenance-engine/internal/ratelimit"
	"github.com/sk3y04/provenance-engine/internal/redact"
	"github.com/sk3y04/provenance-engine/internal/session"
	"github.com/sk3y04/provenance-engine/internal/version"
)

type SyncOptions struct {
	DryRun      bool
	Record      bool
	RateLimiter *ratelimit.Manager
}

// ErrCollectionSyncing means another process is already syncing this collection.
// The lock covers the whole scan/download/commit cycle, so two replays cannot
// both decide the same item is new.
var ErrCollectionSyncing = errors.New("collection sync is already running")

// Seams for hermetic tests; production always uses the real paths.
var (
	scanFn          = dispatcher.Scan
	downloadFn      = app.Download
	recordCaptureFn = recordCapture
	addSeenFn       = AddSeen
	recordSyncFn    = RecordSync
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
	lock, err := lockSync(c.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

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

	succeeded := make(map[string]bool)
	for _, e := range sess.EntriesByStatus(session.StatusSucceeded) {
		succeeded[e.URL] = true
	}

	// Commit order is the durability contract:
	//   artifact on disk -> capture item + run manifest -> seen id -> sync result
	// A capture failure leaves every item retryable; a seen-write failure leaves
	// the committed capture and only re-downloads on the next sync.
	var committed []string
	if opts.Record {
		keys := fullySucceededIDs(urlToItem, succeeded)
		subset := itemsByKeys(urlToItem, keys)
		recorded, rerr := recordCaptureFn(*c, sessName, subset)
		if rerr != nil {
			_ = recordSyncFn(c.Name, *result)
			return result, fmt.Errorf("record capture: %w", rerr)
		}
		committed = recorded
	} else {
		committed = fullySucceededIDs(urlToItem, succeeded)
	}
	if len(committed) > 0 {
		if err := addSeenFn(c.Name, committed...); err != nil {
			return result, fmt.Errorf("save seen ids: %w", err)
		}
	}

	if err := recordSyncFn(c.Name, *result); err != nil {
		return result, fmt.Errorf("record sync: %w", err)
	}
	if downloadErr != nil {
		return result, fmt.Errorf("sync download: %w", downloadErr)
	}

	_, _ = fmt.Fprintf(os.Stderr, "[provenance] collection %q: synced — %d downloaded, %d skipped, %d failed\n",
		c.Name, result.New, result.Skipped, result.Failed)

	return result, nil
}

// lockSync serialises the whole replay for one collection. The lock lives next
// to the collection store, keyed by a digest so a collection name never becomes
// a path fragment.
func lockSync(name string) (*filelock.Lock, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(name))
	lockPath := filepath.Join(filepath.Dir(path), "sync-"+hex.EncodeToString(sum[:8]))
	lock, err := filelock.TryAcquire(lockPath)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, fmt.Errorf("%w: %s", ErrCollectionSyncing, name)
	}
	if err != nil {
		return nil, err
	}
	return lock, nil
}

// itemIdentity returns the durable replay key and the legacy key used by older
// versions. A key names the extractor plus the platform's stable item id, so a
// re-signed CDN URL is not mistaken for new content. A generic item with no
// platform id falls back to a URL with its query and fragment stripped.
func itemIdentity(item manifest.Item) (key, legacy string) {
	id := strings.TrimSpace(item.PostID)
	if id == "" {
		id = strings.TrimSpace(item.ID)
	}
	source := strings.TrimSpace(item.Source)
	if id != "" {
		legacy = id
		if source != "" {
			return source + ":" + id, legacy
		}
		return id, legacy
	}
	url := redact.URL(item.URL)
	if url == "" {
		return "", ""
	}
	legacy = item.URL
	if source != "" {
		return source + ":url:" + url, legacy
	}
	return url, legacy
}

// fullySucceededIDs returns the item identities whose every new URL succeeded.
// A post carrying several media files is only marked seen when all of them
// downloaded, so a partial failure never drops the missing files from future
// syncs. The result is sorted for deterministic persistence.
func fullySucceededIDs(urlToItem map[string]manifest.Item, succeeded map[string]bool) []string {
	needed := make(map[string]int)
	done := make(map[string]int)
	for url, item := range urlToItem {
		id, _ := itemIdentity(item)
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

// itemsByKeys returns only the items whose replay key is in keys. Capture is
// committed per fully succeeded post, not per individual successful file of a
// partial post.
func itemsByKeys(urlToItem map[string]manifest.Item, keys []string) map[string]manifest.Item {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	out := make(map[string]manifest.Item, len(want))
	for url, item := range urlToItem {
		key, _ := itemIdentity(item)
		if key == "" {
			key = url
		}
		if want[key] {
			out[url] = item
		}
	}
	return out
}

// partitionNewItems splits scanned items into not-yet-seen URLs/IDs and the
// skipped remainder. Identity is extractor plus the platform's stable item id
// (PostID first, then ID). The legacy key is also checked so a store written by
// an older version does not rescan everything once. A generic item with no
// platform id falls back to a URL with query/fragment removed, never a signed
// CDN URL.
func partitionNewItems(items []manifest.Item, seen map[string]bool) (newURLs, newIDs []string, urlToItem map[string]manifest.Item, total, skipped int) {
	urlToItem = make(map[string]manifest.Item, len(items))
	for _, item := range items {
		total++
		if item.URL == "" {
			continue
		}
		id, legacy := itemIdentity(item)
		if id == "" {
			id = item.URL
		}
		if seen[id] || (legacy != "" && seen[legacy]) {
			skipped++
			continue
		}
		newURLs = append(newURLs, item.URL)
		newIDs = append(newIDs, id)
		urlToItem[item.URL] = item
	}
	return newURLs, newIDs, urlToItem, total, skipped
}

func recordCapture(c Collection, sessName string, urlToItem map[string]manifest.Item) ([]string, error) {
	outputDir := c.Options.OutputDir
	if strings.TrimSpace(outputDir) == "" {
		outputDir = "."
	}
	cm := &manifest.CaptureManifest{
		Format:       manifest.CaptureFormatVersion,
		SourceURL:    c.URL,
		Site:         c.Site,
		DownloadedAt: time.Now().UTC(),
		OutputDir:    outputDir,
		Tool:         "provenance",
		ToolVersion:  version.Version,
		Options:      config.CaptureOptionsFromConfig(c.Options),
	}

	// Persist items in a stable order so capture manifests are reproducible
	// across runs (map iteration order is random).
	urls := make([]string, 0, len(urlToItem))
	for u := range urlToItem {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	committed := make([]string, 0, len(urls))
	items := make([]manifest.CaptureItem, 0, len(urls))
	for _, u := range urls {
		item := urlToItem[u]
		dest := item.Destination
		if dest == "" {
			if strings.TrimSpace(item.Filename) == "" {
				return nil, fmt.Errorf("artifact for %s has no destination", redact.URL(item.URL))
			}
			dest = filepath.Join(outputDir, item.Filename)
		}
		fullPath := dest
		if !filepath.IsAbs(fullPath) {
			fullPath = filepath.Join(outputDir, dest)
		}
		rel, rerr := filepath.Rel(outputDir, fullPath)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("artifact for %s escapes the output directory", redact.URL(item.URL))
		}
		fi, serr := os.Stat(fullPath)
		if serr != nil {
			return nil, fmt.Errorf("artifact for %s is not available: %w", redact.URL(item.URL), serr)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact for %s is not a regular file", redact.URL(item.URL))
		}
		hash, herr := manifest.FileSha256(fullPath)
		if herr != nil {
			return nil, fmt.Errorf("hash artifact for %s: %w", redact.URL(item.URL), herr)
		}

		ci := manifest.CaptureItem{
			ExternalID:     captureExternalID(item),
			URL:            item.URL,
			Title:          item.Title,
			Author:         item.Creator,
			Kind:           item.Kind,
			Extractor:      item.Source,
			DownloadedPath: filepath.ToSlash(rel),
			ByteSize:       fi.Size(),
			Sha256:         hash,
			CapturedAt:     time.Now().UTC(),
			Status:         "ok",
			SessionName:    sessName,
		}
		if !item.PublishedAt.IsZero() {
			t := item.PublishedAt
			ci.PublishedAt = &t
		}

		if item.Kind == "post" && item.Extension == "md" {
			if postContent, err := os.ReadFile(fullPath); err == nil {
				ci.Text = &manifest.TextCapture{
					Body:   string(postContent),
					Format: "markdown",
				}
			}
		}

		items = append(items, ci)
		key, legacy := itemIdentity(item)
		if key == "" {
			key = legacy
		}
		if key == "" {
			key = item.URL
		}
		committed = append(committed, key)
	}

	for i := range items {
		if err := manifest.WriteCaptureItem(outputDir, items[i].ExternalID, &items[i]); err != nil {
			return nil, fmt.Errorf("write capture item %s: %w", items[i].ExternalID, err)
		}
	}
	cm.Items = items
	runPath := manifest.RunPath(outputDir)
	if err := manifest.WriteCaptureManifest(outputDir, runPath, cm); err != nil {
		return nil, fmt.Errorf("write capture manifest: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "[provenance] recorded %d capture items to %s/.provenance/\n", len(cm.Items), outputDir)
	sort.Strings(committed)
	return committed, nil
}

// captureExternalID is a stable, artifact-level identity: extractor plus the
// platform media id, never a CDN URL. When a platform exposes no media id, a
// short hash of the (redacted) URL keeps two files from one post distinct.
func captureExternalID(item manifest.Item) string {
	source := strings.TrimSpace(item.Source)
	id := strings.TrimSpace(item.ID)
	switch {
	case id != "":
	case strings.TrimSpace(item.PostID) != "":
		id = item.PostID + ":" + shortHash(item.URL)
	case item.URL != "":
		id = "url:" + redact.URL(item.URL)
	default:
		id = filepath.Base(item.Filename)
	}
	if id == "" {
		id = "item"
	}
	if source != "" {
		return source + ":" + id
	}
	return id
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
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
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
