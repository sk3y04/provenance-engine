package event

// Stable reason codes. A reason is machine-readable and must never change its
// meaning; human wording belongs in Detail, which consumers must not depend on.
// Codes are grouped by the layer that emits them so adapters can map them to
// product error codes without string matching.
const (
	// Operation lifecycle.
	ReasonInfo       = "info"
	ReasonStart      = "start"
	ReasonItemStart  = "item_start"
	ReasonItemOK     = "item_ok"
	ReasonItemFailed = "item_failed"
	ReasonRetry      = "retry"
	ReasonSkipped    = "skipped"
	ReasonNothing    = "nothing_to_download"

	// Fallbacks and decisions.
	ReasonBrowserCrawl     = "browser_crawl"
	ReasonLinkScrapeFailed = "link_scrape_failed"
	ReasonLinkCacheSaved   = "link_cache_saved"
	ReasonLinkCacheFailed  = "link_cache_failed"
	ReasonSniffedMediaURL  = "sniffed_media_url"
	ReasonMaxPagesCap      = "max_pages_cap"
	ReasonPaginationStop   = "pagination_stop"
	ReasonDuplicateCursor  = "duplicate_cursor"
	ReasonNoNewPosts       = "no_new_posts"
	ReasonPageResults      = "page_results"
	ReasonFoundItems       = "found_items"
	ReasonCapabilityRetry  = "capability_retry"
	ReasonRetryAfterCapped = "retry_after_capped"

	// Diagnostics that may only carry redacted facts.
	ReasonResponseError     = "response_error"
	ReasonStatus            = "status"
	ReasonRateLimited       = "rate_limited"
	ReasonQueryIDStale      = "query_id_stale"
	ReasonSessionRejected   = "session_rejected"
	ReasonAPIFailed         = "api_failed"
	ReasonAPILookupFailed   = "api_lookup_failed"
	ReasonEmptyUserID       = "empty_user_id"
	ReasonNotLoggedIn       = "not_logged_in"
	ReasonWebScrapeFailed   = "web_scrape_failed"
	ReasonFingerprintFail   = "fingerprint_failed"
	ReasonClickNextFailed   = "click_next_failed"
	ReasonArchiveOpenFail   = "archive_open_failed"
	ReasonAlreadyDownloaded = "already_downloaded"
	ReasonRenamed           = "renamed"
	ReasonSaved             = "saved"
	ReasonBatchLoaded       = "batch_loaded"
	ReasonDryRun            = "dry_run"

	// External tools.
	ReasonFfmpegMissing  = "ffmpeg_missing"
	ReasonFFprobeMissing = "ffprobe_missing"
	ReasonChromeMissing  = "chrome_missing"
	ReasonToolMissing    = "tool_missing"
)
