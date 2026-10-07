package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sk3y04/provenance/internal/ratelimit"
)

func TestConfigPersistsFormValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cs := newConfigStore()
	cs.setFormValues(
		"https://example.com/thing", // url
		"out-dir",                   // output
		"8",                         // concurrency
		"cookies.txt",               // cookiesFile
		"my-session",                // sessionName
		"bv2",                       // quality
		"",                          // cookiesBrowser
		"",                          // includeExt
		"",                          // excludeExt
		"",                          // minSize
		"",                          // maxSize
		"",                          // titleMatch
		"",                          // titleExclude
		"",                          // postLimit
		"",                          // outputLayout
		"",                          // outputTemplate
		"",                          // speedLimit
	)

	// The config file should now exist on disk.
	path := filepath.Join(dir, "provenance", "tui-config.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	// A fresh store (simulating re-opening the TUI) must load the saved values.
	cs2 := newConfigStore()
	if got := cs2.data.LastGrabURL; got != "https://example.com/thing" {
		t.Fatalf("LastGrabURL = %q, want saved value", got)
	}
	if got := cs2.data.OutputDir; got != "out-dir" {
		t.Fatalf("OutputDir = %q, want out-dir", got)
	}
	if got := cs2.data.Concurrency; got != "8" {
		t.Fatalf("Concurrency = %q, want 8", got)
	}

	// Applying to a freshly created form must override the defaults.
	df := newDownloadForm()
	cs2.applyFormValues(&df)
	if got := df.url.Value(); got != "https://example.com/thing" {
		t.Fatalf("form.url = %q, want saved URL", got)
	}
	if got := df.output.Value(); got != "out-dir" {
		t.Fatalf("form.output = %q, want out-dir (default was ./downloads)", got)
	}
	if got := df.concurrency.Value(); got != "8" {
		t.Fatalf("form.concurrency = %q, want 8 (default was 4)", got)
	}
	if got := df.quality.Value(); got != "bv2" {
		t.Fatalf("form.quality = %q, want bv2 (default was best)", got)
	}
}

func TestConfigScanURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	cs := newConfigStore()
	cs.setScanURL("https://scan.example.com/page")
	if got := cs.lastScanURL(); got != "https://scan.example.com/page" {
		t.Fatalf("lastScanURL = %q, want saved value", got)
	}

	cs2 := newConfigStore()
	if got := cs2.lastScanURL(); got != "https://scan.example.com/page" {
		t.Fatalf("reloaded lastScanURL = %q, want saved value", got)
	}
}

func TestConfigEncodeValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	m := newModel(context.Background(), ratelimit.New())
	m.cfg.setEncodeValues("/videos/in", "libsvtav1", "40")

	// Reset the form to defaults, then apply the saved values.
	m.encodeInput.SetValue("")
	m.encodeEncoder.SetValue("auto")
	m.encodeQuality.SetValue("30")
	m.cfg.applyEncodeValues(m)

	if got := m.encodeInput.Value(); got != "/videos/in" {
		t.Fatalf("encodeInput = %q, want /videos/in", got)
	}
	if got := m.encodeEncoder.Value(); got != "libsvtav1" {
		t.Fatalf("encodeEncoder = %q, want libsvtav1 (default was auto)", got)
	}
	if got := m.encodeQuality.Value(); got != "40" {
		t.Fatalf("encodeQuality = %q, want 40 (default was 30)", got)
	}
}
