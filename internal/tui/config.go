package tui

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"sync"

	"github.com/charmbracelet/bubbles/textinput"
)

// ---------------------------------------------------------------------------
// TUI config persistence
// ---------------------------------------------------------------------------

// defaultConfig is the config used when no file exists on disk.
var defaultConfig = tuiConfig{
	OutputDir:      "./downloads",
	Concurrency:    "4",
	Quality:        "best",
	CookiesFile:    "",
	SessionName:    "",
	CookiesBrowser: "",
	IncludeExt:     "",
	ExcludeExt:     "",
	MinSize:        "",
	MaxSize:        "",
	TitleMatch:     "",
	TitleExclude:   "",
	PostLimit:      "",
	OutputLayout:   "",
	OutputTemplate: "",
	SpeedLimit:     "",
	Encoder:        "auto",
	EncodeQuality:  "30",
	LastGrabURL:    "",
	LastScanURL:    "",
	EncodeDir:      "",
}

// tuiConfig holds last-used TUI form values.
type tuiConfig struct {
	OutputDir      string `json:"output_dir"`
	Concurrency    string `json:"concurrency"`
	Quality        string `json:"quality"`
	CookiesFile    string `json:"cookies_file"`
	SessionName    string `json:"session_name"`
	CookiesBrowser string `json:"cookies_browser"`
	IncludeExt     string `json:"include_ext"`
	ExcludeExt     string `json:"exclude_ext"`
	MinSize        string `json:"min_size"`
	MaxSize        string `json:"max_size"`
	TitleMatch     string `json:"title_match"`
	TitleExclude   string `json:"title_exclude"`
	PostLimit      string `json:"post_limit"`
	OutputLayout   string `json:"output_layout"`
	OutputTemplate string `json:"output_template"`
	SpeedLimit     string `json:"speed_limit"`
	Encoder        string `json:"encoder"`
	EncodeQuality  string `json:"encode_quality"`
	LastGrabURL    string `json:"last_grab_url"`
	LastScanURL    string `json:"last_scan_url"`
	EncodeDir      string `json:"encode_dir"`
}

// configStore manages the on-disk JSON config for TUI form persistence.
type configStore struct {
	mu   sync.Mutex
	path string
	data tuiConfig
}

// newConfigStore creates a config store that reads/writes the TUI config file.
// The config is loaded synchronously into memory so that all subsequent
// accesses are from the in-memory copy (protected by the mutex).
func newConfigStore() *configStore {
	cs := &configStore{
		data: defaultConfig,
		path: configPath(),
	}
	cs.load()
	return cs
}

// load reads the config from disk, merging into the in-memory defaults.
func (cs *configStore) load() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	data, err := os.ReadFile(cs.path)
	if err != nil {
		// File doesn't exist or isn't readable – keep defaults.
		cs.data = defaultConfig
		return
	}

	var cfg tuiConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		cs.data = defaultConfig
		return
	}
	cs.data = mergeConfig(defaultConfig, cfg)
}

// saveLocked writes the in-memory config to disk atomically.
// The caller must hold cs.mu.
func (cs *configStore) saveLocked() {
	data, err := json.MarshalIndent(cs.data, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(cs.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// Write to a temp file then rename for atomicity.
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, cs.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// mergeConfig copies non-empty fields from src into dst.
func mergeConfig(dst, src tuiConfig) tuiConfig {
	if src.OutputDir != "" {
		dst.OutputDir = src.OutputDir
	}
	if src.Concurrency != "" {
		dst.Concurrency = src.Concurrency
	}
	if src.Quality != "" {
		dst.Quality = src.Quality
	}
	if src.CookiesFile != "" {
		dst.CookiesFile = src.CookiesFile
	}
	if src.SessionName != "" {
		dst.SessionName = src.SessionName
	}
	if src.CookiesBrowser != "" {
		dst.CookiesBrowser = src.CookiesBrowser
	}
	if src.IncludeExt != "" {
		dst.IncludeExt = src.IncludeExt
	}
	if src.ExcludeExt != "" {
		dst.ExcludeExt = src.ExcludeExt
	}
	if src.MinSize != "" {
		dst.MinSize = src.MinSize
	}
	if src.MaxSize != "" {
		dst.MaxSize = src.MaxSize
	}
	if src.TitleMatch != "" {
		dst.TitleMatch = src.TitleMatch
	}
	if src.TitleExclude != "" {
		dst.TitleExclude = src.TitleExclude
	}
	if src.PostLimit != "" {
		dst.PostLimit = src.PostLimit
	}
	if src.OutputLayout != "" {
		dst.OutputLayout = src.OutputLayout
	}
	if src.OutputTemplate != "" {
		dst.OutputTemplate = src.OutputTemplate
	}
	if src.SpeedLimit != "" {
		dst.SpeedLimit = src.SpeedLimit
	}
	if src.Encoder != "" {
		dst.Encoder = src.Encoder
	}
	if src.EncodeQuality != "" {
		dst.EncodeQuality = src.EncodeQuality
	}
	if src.LastGrabURL != "" {
		dst.LastGrabURL = src.LastGrabURL
	}
	if src.LastScanURL != "" {
		dst.LastScanURL = src.LastScanURL
	}
	if src.EncodeDir != "" {
		dst.EncodeDir = src.EncodeDir
	}
	return dst
}

// setFormValues updates the in-memory config from the download form values.
func (cs *configStore) setFormValues(url, output, concurrency, cookiesFile, sessionName string, quality, cookiesBrowser, includeExt, excludeExt, minSize, maxSize, titleMatch, titleExclude, postLimit, outputLayout, outputTemplate, speedLimit string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if url != "" {
		cs.data.LastGrabURL = url
	}
	if output != "" {
		cs.data.OutputDir = output
	}
	if concurrency != "" {
		cs.data.Concurrency = concurrency
	}
	if quality != "" {
		cs.data.Quality = quality
	}
	if cookiesFile != "" {
		cs.data.CookiesFile = cookiesFile
	}
	if sessionName != "" {
		cs.data.SessionName = sessionName
	}
	if cookiesBrowser != "" {
		cs.data.CookiesBrowser = cookiesBrowser
	}
	if includeExt != "" {
		cs.data.IncludeExt = includeExt
	}
	if excludeExt != "" {
		cs.data.ExcludeExt = excludeExt
	}
	if minSize != "" {
		cs.data.MinSize = minSize
	}
	if maxSize != "" {
		cs.data.MaxSize = maxSize
	}
	if titleMatch != "" {
		cs.data.TitleMatch = titleMatch
	}
	if titleExclude != "" {
		cs.data.TitleExclude = titleExclude
	}
	if postLimit != "" {
		cs.data.PostLimit = postLimit
	}
	if outputLayout != "" {
		cs.data.OutputLayout = outputLayout
	}
	if outputTemplate != "" {
		cs.data.OutputTemplate = outputTemplate
	}
	if speedLimit != "" {
		cs.data.SpeedLimit = speedLimit
	}
	cs.saveLocked()
}

// setScanURL remembers the last URL entered in scan & pick.
func (cs *configStore) setScanURL(url string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if url != "" {
		cs.data.LastScanURL = url
	}
	cs.saveLocked()
}

// setScanValues updates the in-memory config from scan & pick advanced form values.
func (cs *configStore) setScanValues(quality, cookiesBrowser, includeExt, excludeExt, minSize, maxSize, titleMatch, titleExclude, postLimit, outputLayout, outputTemplate, speedLimit string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if quality != "" {
		cs.data.Quality = quality
	}
	if cookiesBrowser != "" {
		cs.data.CookiesBrowser = cookiesBrowser
	}
	if includeExt != "" {
		cs.data.IncludeExt = includeExt
	}
	if excludeExt != "" {
		cs.data.ExcludeExt = excludeExt
	}
	if minSize != "" {
		cs.data.MinSize = minSize
	}
	if maxSize != "" {
		cs.data.MaxSize = maxSize
	}
	if titleMatch != "" {
		cs.data.TitleMatch = titleMatch
	}
	if titleExclude != "" {
		cs.data.TitleExclude = titleExclude
	}
	if postLimit != "" {
		cs.data.PostLimit = postLimit
	}
	if outputLayout != "" {
		cs.data.OutputLayout = outputLayout
	}
	if outputTemplate != "" {
		cs.data.OutputTemplate = outputTemplate
	}
	if speedLimit != "" {
		cs.data.SpeedLimit = speedLimit
	}
	cs.saveLocked()
}

// setEncodeValues updates the in-memory config from the encode form values.
func (cs *configStore) setEncodeValues(dir, encoder, quality string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if dir != "" {
		cs.data.EncodeDir = dir
	}
	if encoder != "" {
		cs.data.Encoder = encoder
	}
	if quality != "" {
		cs.data.EncodeQuality = quality
	}
	cs.saveLocked()
}

// applyFormValues fills the download form text inputs with saved values.
// Saved values override the form's defaults so last-used settings survive
// navigating away and back.
func (cs *configStore) applyFormValues(df *downloadForm) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	set := func(in *textinput.Model, v string) {
		if v != "" {
			in.SetValue(v)
		}
	}
	set(&df.url, cs.data.LastGrabURL)
	set(&df.output, cs.data.OutputDir)
	set(&df.concurrency, cs.data.Concurrency)
	set(&df.cookies, cs.data.CookiesFile)
	set(&df.sessionName, cs.data.SessionName)
	// Advanced fields
	set(&df.quality, cs.data.Quality)
	set(&df.cookiesBrowser, cs.data.CookiesBrowser)
	set(&df.includeExt, cs.data.IncludeExt)
	set(&df.excludeExt, cs.data.ExcludeExt)
	set(&df.minSize, cs.data.MinSize)
	set(&df.maxSize, cs.data.MaxSize)
	set(&df.titleMatch, cs.data.TitleMatch)
	set(&df.titleExclude, cs.data.TitleExclude)
	set(&df.postLimit, cs.data.PostLimit)
	set(&df.outputLayout, cs.data.OutputLayout)
	set(&df.outputTemplate, cs.data.OutputTemplate)
	set(&df.speedLimit, cs.data.SpeedLimit)
}

// applyScanValues fills the scan advanced form inputs with saved values.
// Saved values override the form's defaults.
func (cs *configStore) applyScanValues(f *advOptsForm) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	set := func(in *textinput.Model, v string) {
		if v != "" {
			in.SetValue(v)
		}
	}
	set(&f.quality, cs.data.Quality)
	set(&f.cookiesBrowser, cs.data.CookiesBrowser)
	set(&f.includeExt, cs.data.IncludeExt)
	set(&f.excludeExt, cs.data.ExcludeExt)
	set(&f.minSize, cs.data.MinSize)
	set(&f.maxSize, cs.data.MaxSize)
	set(&f.titleMatch, cs.data.TitleMatch)
	set(&f.titleExclude, cs.data.TitleExclude)
	set(&f.postLimit, cs.data.PostLimit)
	set(&f.outputLayout, cs.data.OutputLayout)
	set(&f.outputTemplate, cs.data.OutputTemplate)
	set(&f.speedLimit, cs.data.SpeedLimit)
}

// applyEncodeValues fills the encode form inputs with saved values.
// Saved values override the form's defaults.
func (cs *configStore) applyEncodeValues(m *model) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	set := func(in *textinput.Model, v string) {
		if v != "" {
			in.SetValue(v)
		}
	}
	set(&m.encodeInput, cs.data.EncodeDir)
	set(&m.encodeEncoder, cs.data.Encoder)
	set(&m.encodeQuality, cs.data.EncodeQuality)
}

// lastScanURL returns the last URL entered in scan & pick, if any.
func (cs *configStore) lastScanURL() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.data.LastScanURL
}

// configPath returns the path to the TUI config file.
// Uses $XDG_CONFIG_HOME/provenance/tui-config.json or falls back to
// ~/.config/provenance/tui-config.json.
func configPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "provenance", "tui-config.json")
	}
	if us, err := user.Current(); err == nil && us.HomeDir != "" {
		return filepath.Join(us.HomeDir, ".config", "provenance", "tui-config.json")
	}
	return "tui-config.json"
}
