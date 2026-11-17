package config

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sk3y04/provenance-engine/internal/manifest"
)

// fullConfig populates every field so the round trip and field-name pins cover
// the whole persisted shape.
func fullConfig() Config {
	return Config{
		OutputDir:          "downloads",
		CookiesFile:        "/tmp/cookies.txt",
		Concurrency:        6,
		Quality:            "720",
		AudioOnly:          true,
		NoArchive:          true,
		Filter:             manifest.FilterOptions{IncludeExt: []string{"mp4"}, ExcludeExt: []string{"webm"}, MinSize: 1, MaxSize: 99, TitleMatch: "a", TitleReject: "b"},
		PostLimit:          50,
		IncludePosts:       true,
		IncludeComments:    true,
		CommentLimit:       10,
		CookiesFromBrowser: "chrome",
		OutputLayout:       "flat",
		OutputTemplate:     "%(title)s.%(ext)s",
		SpeedLimit:         1024,
		ChromePath:         "/usr/bin/chromium",
	}
}

// TestConfigJSONRoundTrip proves the serializable configuration survives a
// marshal/unmarshal cycle unchanged; sessions, watches, collections, and
// history all persist this shape.
func TestConfigJSONRoundTrip(t *testing.T) {
	cfg := fullConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, back) {
		t.Fatalf("round trip changed config:\n%+v\n%+v", cfg, back)
	}
}

// TestConfigJSONFieldNames pins the persisted JSON keys: renaming one would
// silently drop settings from existing session/watch/collection/history files.
func TestConfigJSONFieldNames(t *testing.T) {
	data, err := json.Marshal(fullConfig())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"output_dir", "cookies_file", "concurrency", "quality", "audio_only",
		"no_archive", "filter", "post_limit", "include_posts", "include_comments",
		"comment_limit", "cookies_from_browser", "output_layout", "output_template",
		"speed_limit", "chrome_path",
	}
	for _, key := range want {
		if _, ok := m[key]; !ok {
			t.Errorf("persisted key %q missing from %s", key, data)
		}
	}
	if len(m) != len(want) {
		t.Errorf("unexpected extra keys in %s", data)
	}
}

// TestConfigOmitEmpty proves optional settings stay out of the persisted shape
// when unset, keeping existing files byte-compatible.
func TestConfigOmitEmpty(t *testing.T) {
	data, err := json.Marshal(Config{OutputDir: "downloads", Concurrency: 4, Quality: "best"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"cookies_file", "post_limit", "include_posts", "include_comments",
		"comment_limit", "cookies_from_browser", "output_layout", "output_template",
		"speed_limit", "chrome_path",
	} {
		if _, ok := m[key]; ok {
			t.Errorf("unset key %q persisted: %s", key, data)
		}
	}
}

// TestCaptureOptionsFromConfig proves every capture-relevant setting maps onto
// the capture manifest options.
func TestCaptureOptionsFromConfig(t *testing.T) {
	cfg := fullConfig()
	got := CaptureOptionsFromConfig(cfg)
	want := manifest.CaptureOptions{
		OutputDir:          cfg.OutputDir,
		Concurrency:        cfg.Concurrency,
		Quality:            cfg.Quality,
		AudioOnly:          cfg.AudioOnly,
		IncludePosts:       cfg.IncludePosts,
		CookiesFile:        cfg.CookiesFile,
		CookiesFromBrowser: cfg.CookiesFromBrowser,
		OutputLayout:       cfg.OutputLayout,
		OutputTemplate:     cfg.OutputTemplate,
		Limit:              cfg.PostLimit,
		IncludeComments:    cfg.IncludeComments,
		CommentLimit:       cfg.CommentLimit,
	}
	if got != want {
		t.Fatalf("capture options = %+v, want %+v", got, want)
	}
}
