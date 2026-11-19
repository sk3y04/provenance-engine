package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runSoulseek executes a soulseek subcommand with stdout captured.
func runSoulseek(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := soulseekCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := cmd.Execute()
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), runErr
}

// TestSoulseekSettingsNeverPrintsSecrets proves the CLI is a masked read path:
// the API key and webhook secret reach the 0600 file and never the terminal.
func TestSoulseekSettingsNeverPrintsSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	const key = "cli-secret-key-abcdef"
	const secret = "cli-webhook-secret-abcdef"

	out, err := runSoulseek(t, "settings", "set",
		"--base-url", "http://127.0.0.1:5030",
		"--api-key", key,
		"--webhook-secret", secret,
		"--enable",
		"--downloads-mount", dir,
	)
	if err != nil {
		t.Fatalf("settings set: %v\n%s", err, out)
	}
	if strings.Contains(out, key) || strings.Contains(out, secret) {
		t.Fatalf("settings set printed a secret:\n%s", out)
	}

	out, err = runSoulseek(t, "settings", "show")
	if err != nil {
		t.Fatalf("settings show: %v", err)
	}
	if strings.Contains(out, key) || strings.Contains(out, secret) {
		t.Fatalf("settings show printed a secret:\n%s", out)
	}
	if !strings.Contains(out, "********") {
		t.Fatalf("settings show did not mask secrets:\n%s", out)
	}

	// The file itself must hold the real values, 0600.
	path := filepath.Join(dir, "provenance", "slskd-settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), key) {
		t.Fatal("settings file does not contain the API key")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode = %o", fi.Mode().Perm())
	}
}

// TestSoulseekSettingsRoundTripKeepsMaskedSecret proves a show/set round trip
// cannot overwrite the stored key with the literal mask.
func TestSoulseekSettingsRoundTripKeepsMaskedSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	const key = "keep-me-key"
	if _, err := runSoulseek(t, "settings", "set", "--base-url", "http://h:1", "--api-key", key, "--enable"); err != nil {
		t.Fatalf("initial set: %v", err)
	}
	if _, err := runSoulseek(t, "settings", "set", "--url-base", "/slskd"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	path := filepath.Join(dir, "provenance", "slskd-settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["apiKey"] != key {
		t.Fatalf("apiKey = %v, want the stored key", stored["apiKey"])
	}
	if stored["urlBase"] != "/slskd" {
		t.Fatalf("urlBase = %v", stored["urlBase"])
	}
}

// TestSoulseekHealthStates drives the CLI against a fake slskd for the healthy
// and unreachable states, and asserts the exit error names the state.
func TestSoulseekHealthStates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/application/version"):
			_, _ = w.Write([]byte(`"0.26.0.0"`))
		case strings.HasSuffix(r.URL.Path, "/application"):
			_, _ = w.Write([]byte(`{"version":{"current":"0.26.0.0"},"server":{"isConnected":true,"isLoggedIn":true}}`))
		case strings.HasSuffix(r.URL.Path, "/server"):
			_, _ = w.Write([]byte(`{"state":"Connected, LoggedIn","isConnected":true,"isLoggedIn":true}`))
		case strings.HasSuffix(r.URL.Path, "/options"):
			_, _ = w.Write([]byte(`{"shares":{"directories":[]},"flags":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer healthy.Close()

	if _, err := runSoulseek(t, "settings", "set", "--base-url", healthy.URL, "--api-key", "k", "--enable", "--downloads-mount", dir); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := runSoulseek(t, "health")
	if err != nil {
		t.Fatalf("health on a healthy instance: %v\n%s", err, out)
	}
	if !strings.Contains(out, "HEALTHY") {
		t.Fatalf("health output did not report HEALTHY:\n%s", out)
	}

	// Point at a dead port: the same command must fail and name the state.
	if _, err := runSoulseek(t, "settings", "set", "--base-url", "http://127.0.0.1:1"); err != nil {
		t.Fatalf("set dead: %v", err)
	}
	out, err = runSoulseek(t, "health")
	if err == nil {
		t.Fatalf("health on a dead instance succeeded:\n%s", out)
	}
	if !strings.Contains(out, "SLSKD_UNREACHABLE") {
		t.Fatalf("health output did not report SLSKD_UNREACHABLE:\n%s", out)
	}
}

// TestSoulseekSettingsRefusesEnableWithoutKey keeps a half-configured profile
// from being saved as enabled.
func TestSoulseekSettingsRefusesEnableWithoutKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := runSoulseek(t, "settings", "set", "--enable", "--base-url", "http://127.0.0.1:5030"); err == nil {
		t.Fatal("enabled profile without an API key was saved")
	}
}
