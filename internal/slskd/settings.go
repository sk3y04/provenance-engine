package slskd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
)

// SettingsFormat identifies the settings record so a reader can refuse a newer
// one instead of silently reinterpreting it.
const SettingsFormat = "provenance/slskd-settings+1"

// MaskSentinel is what a masked secret reads as. A client that round-trips the
// masked value back keeps the stored secret; any other value replaces it.
const MaskSentinel = "********"

// Settings is the slskd connection profile. It is the only place a secret for
// this integration lives, and it is written 0600.
type Settings struct {
	Format string `json:"format"`
	// Enabled turns every live slskd action on. A disabled profile still renders
	// imported history.
	Enabled bool `json:"enabled"`
	// BaseURL is the slskd origin, e.g. "http://127.0.0.1:5030".
	BaseURL string `json:"baseUrl"`
	// URLBase is an optional reverse-proxy prefix such as "/slskd".
	URLBase string `json:"urlBase"`
	// APIKey is sent as X-API-Key and is masked on every read path.
	APIKey string `json:"apiKey"`
	// WebhookSecret authenticates inbound webhook deliveries and is masked too.
	WebhookSecret string `json:"webhookSecret"`
	// InsecureSkipVerify relaxes TLS verification for a self-signed reverse proxy.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
	// SlskdDownloadsRoot is the download root as slskd sees it, e.g.
	// "/downloads". Remote paths are relative to it.
	SlskdDownloadsRoot string `json:"slskdDownloadsRoot"`
	// ProvenanceDownloadsMount is the same directory as this process sees it.
	ProvenanceDownloadsMount string `json:"provenanceDownloadsMount"`
	// CuratedShareRoot is the isolated share staging root, mounted read-only into
	// slskd.
	CuratedShareRoot string `json:"curatedShareRoot"`
	// PostImportCleanup deletes the staging copy after a verified import. Off by
	// default: an import that is not verified must never cost the bytes.
	PostImportCleanup bool `json:"postImportCleanup"`
}

// DefaultSettings returns an unconfigured profile.
func DefaultSettings() Settings {
	return Settings{
		Format:  SettingsFormat,
		BaseURL: "http://127.0.0.1:5030",
	}
}

// DefaultSettingsPath returns $XDG_CONFIG_HOME/provenance/slskd-settings.json,
// falling back to ~/.config/provenance/, matching the TUI's config location.
func DefaultSettingsPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "provenance", "slskd-settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "provenance", "slskd-settings.json"), nil
}

// LoadSettings reads a profile. A missing file yields defaults; a newer format
// is an error.
func LoadSettings(path string) (Settings, error) {
	if strings.TrimSpace(path) == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			return Settings{}, err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read slskd settings: %w", err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("parse slskd settings %s: %w", path, err)
	}
	if s.Format == "" {
		s.Format = SettingsFormat
	}
	if s.Format != SettingsFormat {
		return Settings{}, fmt.Errorf("slskd settings %s use format %q, this build understands %q", path, s.Format, SettingsFormat)
	}
	return s, nil
}

// SaveSettings writes the profile atomically with 0600 permissions.
func SaveSettings(path string, s Settings) error {
	if strings.TrimSpace(path) == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			return err
		}
		path = p
	}
	if s.Format == "" {
		s.Format = SettingsFormat
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	//nolint:gosec // G117: this file exists to hold the API key; it is written 0600.
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode slskd settings: %w", err)
	}
	if err := atomicfile.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write slskd settings: %w", err)
	}
	return nil
}

// Validate checks a profile before it is saved or used.
func (s Settings) Validate() error {
	if !s.Enabled {
		return nil
	}
	if strings.TrimSpace(s.BaseURL) == "" {
		return errors.New("slskd base URL is required when the integration is enabled")
	}
	if strings.TrimSpace(s.APIKey) == "" {
		return errors.New("slskd API key is required when the integration is enabled")
	}
	return nil
}

// ValidateConnection checks a profile is usable for a connection attempt. It is
// looser than Validate: a disabled profile may still be tested, and no filesystem
// path is required to ask slskd whether it is reachable.
func (s Settings) ValidateConnection() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.BaseURL) == "" {
		return errors.New("slskd base URL is required")
	}
	if strings.TrimSpace(s.APIKey) == "" {
		return errors.New("slskd API key is required")
	}
	return nil
}

// Masked returns a copy with both secrets replaced by MaskSentinel.
func (s Settings) Masked() Settings {
	if s.APIKey != "" {
		s.APIKey = MaskSentinel
	}
	if s.WebhookSecret != "" {
		s.WebhookSecret = MaskSentinel
	}
	return s
}

// KeepSecrets returns update with any masked secret replaced by the value in
// stored, so a UI that echoes the mask cannot erase a real secret.
func (s Settings) KeepSecrets(stored Settings) Settings {
	if s.APIKey == MaskSentinel || s.APIKey == "" {
		s.APIKey = stored.APIKey
	}
	if s.WebhookSecret == MaskSentinel || s.WebhookSecret == "" {
		s.WebhookSecret = stored.WebhookSecret
	}
	return s
}

// WithEnv layers PROVENANCE_SLSKD_* environment values over the profile. Env
// wins: a headless deployment supplies secrets without writing them to disk.
func (s Settings) WithEnv() Settings {
	setStr := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			*dst = v
		}
	}
	setBool := func(dst *bool, key string) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				*dst = b
			}
		}
	}
	setStr(&s.BaseURL, "PROVENANCE_SLSKD_BASE_URL")
	setStr(&s.URLBase, "PROVENANCE_SLSKD_URL_BASE")
	setStr(&s.APIKey, "PROVENANCE_SLSKD_API_KEY")
	setStr(&s.WebhookSecret, "PROVENANCE_SLSKD_WEBHOOK_SECRET")
	setStr(&s.SlskdDownloadsRoot, "PROVENANCE_SLSKD_DOWNLOADS_ROOT")
	setStr(&s.ProvenanceDownloadsMount, "PROVENANCE_SLSKD_MOUNT")
	setStr(&s.CuratedShareRoot, "PROVENANCE_SLSKD_SHARE_ROOT")
	setBool(&s.Enabled, "PROVENANCE_SLSKD_ENABLED")
	setBool(&s.InsecureSkipVerify, "PROVENANCE_SLSKD_INSECURE")
	setBool(&s.PostImportCleanup, "PROVENANCE_SLSKD_CLEANUP")
	return s
}

// ClientConfig converts the profile into a client configuration.
func (s Settings) ClientConfig() Config {
	return Config{
		BaseURL:            s.BaseURL,
		URLBase:            s.URLBase,
		APIKey:             s.APIKey,
		WebhookSecret:      s.WebhookSecret,
		InsecureSkipVerify: s.InsecureSkipVerify,
	}
}

// Describe renders the profile for a terminal, with both secrets masked. This is
// the only rendering the CLI prints.
func (s Settings) Describe() []string {
	onOff := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	show := func(v string) string {
		if strings.TrimSpace(v) == "" {
			return "(not set)"
		}
		return v
	}
	key := "(not set)"
	if s.APIKey != "" {
		key = MaskSentinel
	}
	secret := "(not set)"
	if s.WebhookSecret != "" {
		secret = MaskSentinel
	}
	return []string{
		"enabled: " + onOff(s.Enabled),
		"base URL: " + show(s.BaseURL),
		"url base: " + show(s.URLBase),
		"api key: " + key,
		"webhook secret: " + secret,
		"skip TLS verify: " + onOff(s.InsecureSkipVerify),
		"slskd downloads root: " + show(s.SlskdDownloadsRoot),
		"provenance downloads mount: " + show(s.ProvenanceDownloadsMount),
		"curated share root: " + show(s.CuratedShareRoot),
		"cleanup staging after import: " + onOff(s.PostImportCleanup),
	}
}
