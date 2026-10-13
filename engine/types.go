package engine

import "time"

// Defaults applied by New when a Config field is left at its zero value.
const (
	// DefaultQuality is the video quality used when Config.Quality is empty.
	DefaultQuality = "best"
	// DefaultConcurrency is the parallel download count used when
	// Config.Concurrency is below 1.
	DefaultConcurrency = 4
	// DefaultMaxItems bounds a request when neither the request nor Config
	// set a positive limit.
	DefaultMaxItems = 1000
)

// Config configures an Engine. New fills documented zero values with safe
// defaults; WorkDir is required.
type Config struct {
	// WorkDir is the required scratch/output root for downloaded artifacts.
	// A hosted caller should use a fresh, dedicated directory per operation.
	WorkDir string

	// CookiesFile is an optional path to a Netscape-format cookies file.
	CookiesFile string
	// CookiesFromBrowser asks the extractor to load cookies from an installed
	// browser (for example "chrome" or "firefox").
	CookiesFromBrowser string

	// Quality is one of "best", "1080", "720", or "480". Empty defaults to
	// DefaultQuality.
	Quality string
	// AudioOnly extracts audio instead of video and requires ffmpeg.
	AudioOnly bool

	// Concurrency bounds parallel downloads. Values below 1 default to
	// DefaultConcurrency.
	Concurrency int
	// SpeedLimit caps download speed in bytes per second; 0 is unlimited.
	SpeedLimit int64

	// MaxItems is the hard upper bound applied to a request's Limit. Values
	// below 1 default to DefaultMaxItems. Hosted callers must set an explicit
	// policy-appropriate bound.
	MaxItems int

	// Timeout, when positive, bounds each Resolve and Download call.
	Timeout time.Duration

	// NoArchive disables the engine's own URL archive.
	NoArchive bool

	// OutputLayout selects an output layout preset: "" (default), "flat",
	// "site", "creator", or "date".
	OutputLayout string

	// ChromePath optionally overrides the Chrome/Chromium executable used by
	// the browser fallback.
	ChromePath string

	// AutoInstall permits a missing tool to be downloaded into the local cache.
	// It is false by default so a hosted operation cannot mutate the filesystem;
	// the CLI keeps its historical auto-install behavior.
	AutoInstall bool

	// IsolatedWorkspace gives each operation its own subdirectory under WorkDir.
	// A long-lived agent or worker running concurrent jobs on one Engine must set
	// it (or use one Engine per job): two concurrent jobs sharing one work
	// directory cannot otherwise be separated, and an operation would report
	// files created by another job. It is left false for single-job CLI use,
	// where outputs land directly in WorkDir.
	IsolatedWorkspace bool
}

// ToolStatus is the read-only result of probing one external tool.
type ToolStatus struct {
	Name      string
	Required  bool
	Available bool
	Path      string
}

// FilterOptions narrows which items an operation considers.
type FilterOptions struct {
	IncludeExt  []string
	ExcludeExt  []string
	MinSize     int64
	MaxSize     int64
	TitleMatch  string
	TitleReject string
}

// SourceKind classifies a resolved source.
type SourceKind string

const (
	SourceFeed     SourceKind = "feed"
	SourceSingle   SourceKind = "single"
	SourcePlaylist SourceKind = "playlist"
)

// MediaKind classifies a media asset.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaAudio MediaKind = "audio"
)

// TextFormat identifies how TextContent.Body is encoded.
type TextFormat string

const (
	TextPlain    TextFormat = "plain"
	TextMarkdown TextFormat = "markdown"
	TextHTML     TextFormat = "html"
)

// MediaAsset is a downloadable media file referenced by an Item.
type MediaAsset struct {
	URL       string
	Filename  string
	Extension string
	Size      int64
	Kind      MediaKind
}

// TextContent is optional post body text attached to an Item.
type TextContent struct {
	Body   string
	Format TextFormat
}

// Item is a single discovered post or media entry.
type Item struct {
	ExternalID  string
	URL         string
	Title       string
	Author      string
	PublishedAt *time.Time
	Media       []MediaAsset
	Text        *TextContent
}

// Source is a resolved source and its discovered items.
type Source struct {
	URL          string
	CanonicalURL string
	Kind         SourceKind
	Extractor    string
	Title        string
	Author       string
	Items        []Item
}

// Artifact is a file produced by Download. Path is absolute and refers to a
// file inside Config.WorkDir. A hosted caller should treat Path as engine-local
// and not expose it as a public identifier. ItemRef is best-effort: it is set
// only when the engine can associate the file with a resolved item.
type Artifact struct {
	Path     string
	Filename string
	Size     int64
	SHA256   string
	MIMEType string
	ItemRef  string
}

// Counts tallies an operation.
type Counts struct {
	Discovered int64
	Succeeded  int64
	Failed     int64
	Skipped    int64
}

// Result is the outcome of Download.
type Result struct {
	Source    Source
	Artifacts []Artifact
	Counts    Counts
	Warnings  []string
}

// ResolveRequest describes a Resolve operation.
type ResolveRequest struct {
	URL    string
	Limit  int
	Filter FilterOptions
}

// DownloadRequest describes a Download operation.
type DownloadRequest struct {
	URL             string
	Limit           int
	Filter          FilterOptions
	IncludePosts    bool
	IncludeComments bool
	CommentLimit    int
}
