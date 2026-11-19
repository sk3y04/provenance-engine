package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSlskd is a minimal slskd stand-in whose individual endpoints can be
// overridden per test.
type fakeSlskd struct {
	version     string
	versionCode int
	application string
	appCode     int
	server      string
	serverCode  int
	options     string
	optionsCode int
}

func (f *fakeSlskd) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v0/application/version"):
			writeJSON(w, orCode(f.versionCode, 200), rawOr(f.version, `"0.26.0.0"`))
		case strings.HasSuffix(r.URL.Path, "/api/v0/application"):
			writeJSON(w, orCode(f.appCode, 200), rawOr(f.application, `{"version":{"current":"0.26.0.0"},"server":{"isConnected":true,"isLoggedIn":true}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v0/server"):
			writeJSON(w, orCode(f.serverCode, 200), rawOr(f.server, `{"state":"Connected, LoggedIn","isConnected":true,"isLoggedIn":true}`))
		case strings.HasSuffix(r.URL.Path, "/api/v0/options"):
			writeJSON(w, orCode(f.optionsCode, 200), rawOr(f.options, `{"shares":{"directories":["[P]/shared"],"cache":{"storageMode":"disk"}},"flags":{}}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func orCode(code, def int) int {
	if code != 0 {
		return code
	}
	return def
}

func orBody(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func rawOr(v, def string) json.RawMessage {
	return json.RawMessage(orBody(v, def))
}

// healthPaths returns a download mount and share root that exist, so the default
// health result is not a path failure.
func healthPaths(t *testing.T) HealthPaths {
	t.Helper()
	root := t.TempDir()
	mount := filepath.Join(root, "downloads")
	share := filepath.Join(root, "shares")
	for _, d := range []string{mount, share} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return HealthPaths{DownloadsMount: mount, ShareRoot: share}
}

func probe(t *testing.T, f *fakeSlskd, paths HealthPaths) Health {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	return CheckHealth(context.Background(), c, paths)
}

// TestHealthHealthy is the baseline: reachable, supported version, connected.
func TestHealthHealthy(t *testing.T) {
	h := probe(t, &fakeSlskd{}, healthPaths(t))
	if h.State != HealthHealthy {
		t.Fatalf("state = %s, diagnostics: %v", h.State, h.Diagnostics)
	}
	if !h.Version.AtLeast(MinVersion) {
		t.Fatalf("version = %s", h.Version)
	}
}

// TestHealthUnreachable covers a stopped or crashed slskd (#1891's observed
// symptom from the client side).
func TestHealthUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, APIKey: "k", HealthTimeout: 200 * time.Millisecond})
	h := CheckHealth(context.Background(), c, healthPaths(t))
	if h.State != HealthUnreachable {
		t.Fatalf("state = %s, want %s", h.State, HealthUnreachable)
	}

	// A closed port is a transport failure, also unreachable.
	dead, _ := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k", HealthTimeout: 200 * time.Millisecond})
	if got := CheckHealth(context.Background(), dead, healthPaths(t)).State; got != HealthUnreachable {
		t.Fatalf("closed port state = %s", got)
	}
}

// TestHealthInvalidKey covers a wrong or revoked API key.
func TestHealthInvalidKey(t *testing.T) {
	h := probe(t, &fakeSlskd{versionCode: 401}, healthPaths(t))
	if h.State != HealthAuthOrConfig {
		t.Fatalf("state = %s, want %s", h.State, HealthAuthOrConfig)
	}
	if len(h.Diagnostics) == 0 {
		t.Fatal("no diagnostic explaining the auth failure")
	}
}

// TestHealthVersionFloor proves < 0.26.0 is a hard stop: those releases have no
// transfer batches, so the durable job store cannot pin a destination.
func TestHealthVersionFloor(t *testing.T) {
	h := probe(t, &fakeSlskd{version: `"0.25.9.0"`}, healthPaths(t))
	if h.State != HealthAuthOrConfig {
		t.Fatalf("state = %s, want %s", h.State, HealthAuthOrConfig)
	}
	joined := strings.Join(h.Diagnostics, " ")
	if !strings.Contains(joined, "0.26.0") || !strings.Contains(joined, "0.25.9") {
		t.Fatalf("diagnostics do not name both versions: %q", joined)
	}
}

// TestHealthAmberWarnings proves 0.26.1 and 0.26.2 are usable with a warning, not
// a hard failure, and that the /options 403 on 0.26.2 is reported as its own
// diagnostic rather than as an auth error.
func TestHealthAmberWarnings(t *testing.T) {
	h := probe(t, &fakeSlskd{version: `"0.26.1.0"`}, healthPaths(t))
	if h.State != HealthHealthy {
		t.Fatalf("0.26.1 state = %s, diagnostics %v", h.State, h.Diagnostics)
	}
	if !hasWarning(h, "#1891") {
		t.Fatalf("0.26.1 missing #1891 warning: %+v", h.Warnings)
	}

	h = probe(t, &fakeSlskd{version: `"0.26.2.0"`, optionsCode: 403}, healthPaths(t))
	if h.State != HealthHealthy {
		t.Fatalf("0.26.2 state = %s, diagnostics %v", h.State, h.Diagnostics)
	}
	if !hasWarning(h, "#1891") || !hasWarning(h, "#1905") {
		t.Fatalf("0.26.2 warnings = %+v, want #1891 and #1905", h.Warnings)
	}
	if h.OptionsReadable {
		t.Fatal("OptionsReadable = true after a 403")
	}
}

// TestHealthOptions403IsNotAuthError is the narrow regression that keeps #1905
// from being classified as a bad key: a readwrite key on 0.26.2 is a version
// artefact, and everything else still works.
func TestHealthOptions403IsNotAuthError(t *testing.T) {
	h := probe(t, &fakeSlskd{version: `"0.26.2.0"`, optionsCode: 403}, healthPaths(t))
	if h.State == HealthAuthOrConfig {
		t.Fatalf("403 on /options was classified as AUTH_OR_CONFIG_ERROR: %v", h.Diagnostics)
	}
}

// TestHealthSoulseekDisconnected covers the third state: slskd answers, but it
// is not logged in to Soulseek.
func TestHealthSoulseekDisconnected(t *testing.T) {
	for _, body := range []string{
		`{"state":"Connected","isConnected":true,"isLoggedIn":false}`,
		`{"state":"None","isConnected":false,"isLoggedIn":false}`,
	} {
		h := probe(t, &fakeSlskd{server: body}, healthPaths(t))
		if h.State != HealthSoulseekDisconnected {
			t.Fatalf("server %s: state = %s", body, h.State)
		}
	}
}

// TestHealthPathDiagnostics covers an unreadable or absent download mount: the
// integration cannot run, and the operator needs to know which path is wrong.
func TestHealthPathDiagnostics(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	h := probe(t, &fakeSlskd{}, HealthPaths{DownloadsMount: missing})
	if h.State != HealthAuthOrConfig {
		t.Fatalf("state = %s, want %s", h.State, HealthAuthOrConfig)
	}
	if !strings.Contains(strings.Join(h.Diagnostics, " "), missing) {
		t.Fatalf("diagnostics do not name the path: %v", h.Diagnostics)
	}

	// A file where a directory is expected is equally fatal.
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := probe(t, &fakeSlskd{}, HealthPaths{DownloadsMount: file}); got.State != HealthAuthOrConfig {
		t.Fatalf("file-as-mount state = %s", got.State)
	}
}

// TestHealthWarnsOnRiskyOptions covers the configuration traps that are legal but
// dangerous: remote configuration (exposes plaintext secrets and strips YAML
// comments), no_share_scan with shares configured (#1663), a memory share cache
// (#610), and 666 permissions (#1760).
func TestHealthWarnsOnRiskyOptions(t *testing.T) {
	opts := `{"remoteConfiguration":true,
	          "shares":{"directories":["[P]/shared"],"cache":{"storageMode":"memory"}},
	          "flags":{"noShareScan":true},
	          "transfers":{"download":{"destination":{"permissions":{"mode":"666"}}}}}`
	h := probe(t, &fakeSlskd{options: opts}, healthPaths(t))
	if h.State != HealthHealthy {
		t.Fatalf("state = %s", h.State)
	}
	for _, code := range []string{"remote-configuration", "#1663", "#610", "#1760"} {
		if !hasWarning(h, code) {
			t.Errorf("missing warning %s; have %+v", code, h.Warnings)
		}
	}
}

// TestHealthRedactsSecrets proves the health document never carries the API key,
// even when a failing endpoint echoes it back.
func TestHealthRedactsSecrets(t *testing.T) {
	const key = "leaky-key-abcdef123456"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+key, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, APIKey: key})
	h := CheckHealth(context.Background(), c, healthPaths(t))
	for _, d := range h.Diagnostics {
		if strings.Contains(d, key) {
			t.Fatalf("diagnostic leaked the key: %q", d)
		}
	}
	for _, w := range h.Warnings {
		if strings.Contains(w.Message, key) {
			t.Fatalf("warning leaked the key: %q", w.Message)
		}
	}
}

// TestHealthBlocking reports which live actions are safe to offer in each state;
// the UI uses this rather than re-deriving policy from the state name.
func TestHealthBlocking(t *testing.T) {
	cases := map[HealthState]struct{ live bool }{
		HealthHealthy:              {true},
		HealthSoulseekDisconnected: {false},
		HealthUnreachable:          {false},
		HealthAuthOrConfig:         {false},
	}
	for state, want := range cases {
		h := Health{State: state}
		if got := h.LiveActionsAllowed(); got != want.live {
			t.Errorf("%s.LiveActionsAllowed() = %v, want %v", state, got, want.live)
		}
	}
}

func hasWarning(h Health, code string) bool {
	for _, w := range h.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
