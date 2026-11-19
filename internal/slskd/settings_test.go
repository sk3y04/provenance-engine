package slskd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slskd-settings.json")
	s := DefaultSettings()
	s.Enabled = true
	s.BaseURL = "http://127.0.0.1:5030"
	s.URLBase = "/slskd"
	s.APIKey = "abcdefghijklmnop"
	s.WebhookSecret = "secret-value"
	s.SlskdDownloadsRoot = "/srv/slskd/downloads"
	s.ProvenanceDownloadsMount = "/srv/media/downloads"
	s.CuratedShareRoot = "/srv/media/soulseek-shares"
	s.PostImportCleanup = true

	if err := SaveSettings(path, s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file holds a live API key; it must never be group- or world-readable.
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode = %o, want 600", fi.Mode().Perm())
	}
	back, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if back != s {
		t.Fatalf("round trip changed settings:\n%+v\n%+v", s, back)
	}
}

// TestSettingsJSONFieldNames pins the persisted keys: renaming one silently
// drops a stored secret or path from every existing profile.
func TestSettingsJSONFieldNames(t *testing.T) {
	s := DefaultSettings()
	s.Enabled = true
	s.BaseURL = "http://h:1"
	s.APIKey = "k"
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"format", "enabled", "baseUrl", "apiKey", "slskdDownloadsRoot", "provenanceDownloadsMount", "curatedShareRoot", "postImportCleanup", "insecureSkipVerify"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("persisted settings lost key %q: %s", key, data)
		}
	}
	if m["format"] != SettingsFormat {
		t.Fatalf("format = %v, want %s", m["format"], SettingsFormat)
	}
}

// TestSettingsMasking proves no read path exposes the API key or webhook secret.
func TestSettingsMasking(t *testing.T) {
	s := DefaultSettings()
	s.APIKey = "live-api-key-value"
	s.WebhookSecret = "live-webhook-value"
	masked := s.Masked()
	if masked.APIKey != MaskSentinel || masked.WebhookSecret != MaskSentinel {
		t.Fatalf("Masked() = %+v, want sentinels", masked)
	}
	rendered, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "live-api-key-value") || strings.Contains(string(rendered), "live-webhook-value") {
		t.Fatalf("masked settings leaked a secret: %s", rendered)
	}
	// The describe lines the CLI prints must be equally clean.
	for _, line := range masked.Describe() {
		if strings.Contains(line, "live-api-key-value") || strings.Contains(line, "live-webhook-value") {
			t.Fatalf("Describe() leaked a secret: %q", line)
		}
	}
}

// TestSettingsUpdateWithSentinelKeepsSecret proves a round-tripped masked value
// cannot overwrite a stored secret with the literal mask.
func TestSettingsUpdateWithSentinelKeepsSecret(t *testing.T) {
	stored := DefaultSettings()
	stored.APIKey = "real-key"
	stored.WebhookSecret = "real-secret"

	update := stored.Masked()
	update.BaseURL = "http://newhost:5030"
	merged := update.KeepSecrets(stored)
	if merged.APIKey != "real-key" {
		t.Fatalf("APIKey = %q, want the stored key", merged.APIKey)
	}
	if merged.WebhookSecret != "real-secret" {
		t.Fatalf("WebhookSecret = %q, want the stored secret", merged.WebhookSecret)
	}
	if merged.BaseURL != "http://newhost:5030" {
		t.Fatalf("BaseURL = %q, want the update", merged.BaseURL)
	}

	// A genuinely new key replaces the stored one.
	update2 := stored.Masked()
	update2.APIKey = "rotated-key"
	if got := update2.KeepSecrets(stored).APIKey; got != "rotated-key" {
		t.Fatalf("rotated key = %q", got)
	}
}

// TestSettingsValidation covers the fields a usable profile actually needs.
func TestSettingsValidation(t *testing.T) {
	s := DefaultSettings()
	s.Enabled = false
	if err := s.Validate(); err != nil {
		t.Fatalf("disabled profile rejected: %v", err)
	}
	s.Enabled = true
	if err := s.Validate(); err == nil {
		t.Fatal("enabled profile with no base URL accepted")
	}
	s.BaseURL = "http://127.0.0.1:5030"
	if err := s.Validate(); err == nil {
		t.Fatal("enabled profile with no API key accepted")
	}
	s.APIKey = "k"
	if err := s.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	if err := s.ValidateForTest(); err != nil {
		t.Fatalf("ValidateForTest rejected a valid profile: %v", err)
	}
}

// TestSettingsEnvOverrides proves a headless deployment can supply secrets
// through the environment without writing them to disk.
func TestSettingsEnvOverrides(t *testing.T) {
	s := DefaultSettings()
	s.APIKey = "file-key"
	t.Setenv("PROVENANCE_SLSKD_API_KEY", "env-key")
	t.Setenv("PROVENANCE_SLSKD_WEBHOOK_SECRET", "env-secret")
	t.Setenv("PROVENANCE_SLSKD_BASE_URL", "http://envhost:5030")
	t.Setenv("PROVENANCE_SLSKD_ENABLED", "true")
	t.Setenv("PROVENANCE_SLSKD_INSECURE", "true")

	got := s.WithEnv()
	if got.APIKey != "env-key" || got.WebhookSecret != "env-secret" {
		t.Fatalf("env secrets not applied: %+v", got)
	}
	if got.BaseURL != "http://envhost:5030" || !got.Enabled || !got.InsecureSkipVerify {
		t.Fatalf("env scalars not applied: %+v", got)
	}
	// Describe must still mask after env application.
	for _, line := range got.Describe() {
		if strings.Contains(line, "env-key") || strings.Contains(line, "env-secret") {
			t.Fatalf("Describe() leaked an env secret: %q", line)
		}
	}
}

// TestSettingsDefaultPath follows the repo convention:
// $XDG_CONFIG_HOME/provenance/slskd-settings.json, falling back to ~/.config.
func TestSettingsDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path, err := DefaultSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "provenance", "slskd-settings.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

// TestSettingsMissingFileIsNotAnError proves a first run yields defaults rather
// than a failure, so `provenance soulseek settings show` works before any setup.
func TestSettingsMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings(absent): %v", err)
	}
	if s.Format != SettingsFormat || s.Enabled {
		t.Fatalf("defaults = %+v", s)
	}
}

// TestSettingsRefusesFutureFormat keeps a newer profile from being silently
// reinterpreted by an older binary.
func TestSettingsRefusesFutureFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.json")
	if err := os.WriteFile(path, []byte(`{"format":"provenance/slskd-settings+99"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("future format accepted")
	}
}
