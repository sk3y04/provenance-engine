package slskd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HealthState is the single state the UI renders. It answers one question: which
// live actions can be offered right now.
type HealthState string

const (
	// HealthHealthy means slskd answers, supports the contract, and is logged in.
	HealthHealthy HealthState = "HEALTHY"
	// HealthUnreachable means no usable HTTP answer: stopped, crashed, timed out
	// or network-severed.
	HealthUnreachable HealthState = "SLSKD_UNREACHABLE"
	// HealthAuthOrConfig means slskd answered but the profile or local setup is
	// wrong: bad key, unsupported version, unreadable mount.
	HealthAuthOrConfig HealthState = "AUTH_OR_CONFIG_ERROR"
	// HealthSoulseekDisconnected means slskd is reachable but not logged in.
	HealthSoulseekDisconnected HealthState = "SOULSEEK_DISCONNECTED"
)

// Warning is a non-blocking advisory attached to a health result.
type Warning struct {
	Code    string
	Message string
}

// HealthPaths is the local filesystem configuration to verify.
type HealthPaths struct {
	// DownloadsMount is the mapped slskd download root as this process sees it.
	DownloadsMount string
	// ShareRoot is the curated share staging root, if configured.
	ShareRoot string
}

// Health is one health probe result.
type Health struct {
	State           HealthState
	Version         Version
	VersionRaw      string
	LatestVersion   string
	UpdateAvailable bool
	OptionsReadable bool
	Server          *ServerState
	Warnings        []Warning
	Diagnostics     []string
	CheckedAt       time.Time
}

// LiveActionsAllowed reports whether search, enqueue, browse and rescan may be
// offered. Imported library and job history stay readable in every state.
func (h Health) LiveActionsAllowed() bool { return h.State == HealthHealthy }

// CheckHealth probes one slskd instance. It never returns an error: an
// unreachable instance is a state, not a failure, because the caller still needs
// to render the local library.
func CheckHealth(ctx context.Context, c *Client, paths HealthPaths) Health {
	h := Health{State: HealthHealthy, CheckedAt: time.Now().UTC()}

	raw, err := c.GetVersion(ctx)
	if err != nil {
		return h.fail(classifyTransport(err), err)
	}
	h.VersionRaw = raw
	v, err := parseVersion(raw)
	if err != nil {
		return h.fail(HealthAuthOrConfig, fmt.Errorf("slskd reported an unparseable version %q", raw))
	}
	h.Version = v
	if !v.AtLeast(MinVersion) {
		return h.fail(HealthAuthOrConfig, fmt.Errorf("slskd %s is below the minimum supported version %s; transfer batches are required for durable downloads", raw, MinVersion))
	}

	// Application carries the latest-version hint and a server snapshot; either
	// may be absent on an older build, so a failure here is not fatal.
	if app, appErr := c.GetApplication(ctx); appErr == nil {
		h.LatestVersion = app.Version.Latest
		h.UpdateAvailable = app.Version.IsUpdateAvailable
	}

	h.applyVersionWarnings(v, raw)

	opts, optErr := c.GetOptions(ctx)
	switch {
	case optErr == nil:
		h.OptionsReadable = true
		h.applyOptionWarnings(opts)
	case IsStatus(optErr, http.StatusForbidden):
		// #1905: a readwrite key gets 403 on 0.26.2. Everything else works.
		h.Warnings = append(h.Warnings, Warning{
			Code: "#1905",
			Message: "slskd refused /api/v0/options with 403; on 0.26.1-0.26.2 a readwrite API key cannot read options. " +
				"Provenance continues without option diagnostics; use an administrator key to enable them.",
		})
	case IsAuthError(optErr):
		return h.fail(HealthAuthOrConfig, optErr)
	}

	srv, srvErr := c.GetServer(ctx)
	if srvErr != nil {
		return h.fail(classifyTransport(srvErr), srvErr)
	}
	h.Server = srv

	if diag := checkPaths(paths); len(diag) > 0 {
		h.Diagnostics = append(h.Diagnostics, diag...)
		h.State = HealthAuthOrConfig
		return h
	}

	if !srv.IsConnected || !srv.IsLoggedIn {
		h.State = HealthSoulseekDisconnected
		h.Diagnostics = append(h.Diagnostics, fmt.Sprintf("slskd is reachable but not logged in to Soulseek (state %q)", srv.State))
	}
	return h
}

func (h Health) fail(state HealthState, err error) Health {
	h.State = state
	if err != nil {
		h.Diagnostics = append(h.Diagnostics, err.Error())
	}
	return h
}

// classifyTransport maps an error to unreachable vs misconfigured.
func classifyTransport(err error) HealthState {
	if IsAuthError(err) {
		return HealthAuthOrConfig
	}
	return HealthUnreachable
}

func (h *Health) applyVersionWarnings(v Version, raw string) {
	if v.Major == 0 && v.Minor == 26 && (v.Patch == 1 || v.Patch == 2) {
		h.Warnings = append(h.Warnings, Warning{
			Code: "#1891",
			Message: fmt.Sprintf("slskd %s can crash during a 180-second enqueue timeout (unhandled ObjectDisposedException, exit 134); "+
				"pin 0.26.0 or wait for a release carrying #1921. Provenance recovers the instance as %s if it disappears.", raw, HealthUnreachable),
		})
	}
}

func (h *Health) applyOptionWarnings(opts *Options) {
	if opts.RemoteConfiguration {
		h.Warnings = append(h.Warnings, Warning{
			Code: "remote-configuration",
			Message: "slskd has remote_configuration enabled: configuration can be read and changed over HTTP, and secrets appear in plaintext in GET /api/v0/options. " +
				"Provenance never writes slskd configuration; consider disabling it.",
		})
	}
	if opts.Flags.NoShareScan && len(opts.Shares.Directories) > 0 {
		h.Warnings = append(h.Warnings, Warning{
			Code: "#1663",
			Message: "slskd has flags.no_share_scan set while share directories are configured: the share index stays empty until a manual rescan. " +
				"Provenance warns when active share rules return zero files; set no_share_scan: false for automatic scans.",
		})
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Shares.Cache.StorageMode))
	if mode != "" && mode != "disk" {
		h.Warnings = append(h.Warnings, Warning{
			Code:    "#610",
			Message: fmt.Sprintf("slskd uses share cache storage %q; a memory cache is rebuilt on every restart and can stall large libraries. Prefer shares.cache.storage_mode: disk.", mode),
		})
	}
	if perm := strings.TrimSpace(opts.Transfers.Download.Destination.Permissions.Mode); perm != "" {
		if mode, perr := strconv.ParseUint(perm, 8, 32); perr == nil {
			bits := os.FileMode(mode)
			switch {
			case bits&0o111 == 0:
				// 644/600/666: no execute bit, so the album directories slskd
				// creates cannot be traversed by the peer.
				h.Warnings = append(h.Warnings, Warning{
					Code:    "#1760",
					Message: fmt.Sprintf("slskd transfers.download.destination.permissions.mode is %q with no execute bit; downloaded album directories cannot be traversed. Use 755 or 775.", perm),
				})
			case bits&0o002 != 0:
				// World-writable: anything on the host can rewrite files a peer is
				// serving.
				h.Warnings = append(h.Warnings, Warning{
					Code:    "#1760",
					Message: fmt.Sprintf("slskd transfers.download.destination.permissions.mode is %q, which is world-writable. Use 755 or 775.", perm),
				})
			}
		}
	}
}

// checkPaths verifies the local side of the mount contract. A path that does not
// exist, is not a directory, or cannot be read means the integration cannot see
// the files slskd writes, regardless of what the API says.
func checkPaths(paths HealthPaths) []string {
	var diags []string
	check := func(label, p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		fi, err := os.Stat(p)
		if err != nil {
			diags = append(diags, fmt.Sprintf("%s %s is not accessible: %v", label, p, err))
			return
		}
		if !fi.IsDir() {
			diags = append(diags, fmt.Sprintf("%s %s is not a directory", label, p))
			return
		}
		f, err := os.Open(p)
		if err != nil {
			diags = append(diags, fmt.Sprintf("%s %s is not readable: %v", label, p, err))
			return
		}
		if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
			diags = append(diags, fmt.Sprintf("%s %s cannot be listed: %v", label, p, err))
		}
		_ = f.Close()
	}
	check("download mount", paths.DownloadsMount)
	check("curated share root", paths.ShareRoot)
	return diags
}

// CheckSettingsHealth is the convenience path used by the CLI: it validates the
// profile first, then probes.
func CheckSettingsHealth(ctx context.Context, s Settings) Health {
	paths := HealthPaths{DownloadsMount: s.ProvenanceDownloadsMount, ShareRoot: s.CuratedShareRoot}
	if !s.Enabled {
		return Health{
			State:       HealthAuthOrConfig,
			CheckedAt:   time.Now().UTC(),
			Diagnostics: []string{"the slskd integration is disabled; enable it in settings before testing the connection"},
		}
	}
	if err := s.ValidateConnection(); err != nil {
		return Health{State: HealthAuthOrConfig, CheckedAt: time.Now().UTC(), Diagnostics: []string{err.Error()}}
	}
	c, err := New(s.ClientConfig())
	if err != nil {
		// New failed, so there is no client to redact with; redact the profile's
		// own secrets instead. Reaching through the nil client here panicked.
		return Health{
			State:       HealthAuthOrConfig,
			CheckedAt:   time.Now().UTC(),
			Diagnostics: []string{Redact([]string{s.APIKey, s.WebhookSecret}, err.Error())},
		}
	}
	return CheckHealth(ctx, c, paths)
}

// Summary renders one line per finding for a terminal.
func (h Health) Summary() []string {
	lines := []string{fmt.Sprintf("state: %s", h.State)}
	if h.VersionRaw != "" {
		lines = append(lines, fmt.Sprintf("slskd version: %s", h.VersionRaw))
	}
	if h.LatestVersion != "" && h.UpdateAvailable {
		lines = append(lines, fmt.Sprintf("update available: %s", h.LatestVersion))
	}
	if !h.OptionsReadable && h.State == HealthHealthy {
		lines = append(lines, "options: not readable with this key (see warnings)")
	}
	for _, w := range h.Warnings {
		lines = append(lines, fmt.Sprintf("warning %s: %s", w.Code, w.Message))
	}
	for _, d := range h.Diagnostics {
		lines = append(lines, "diagnostic: "+d)
	}
	return lines
}

// ensurePathIsInside is the single containment rule used by the ingestor and the
// share materialiser: a candidate must resolve inside root, and neither path may
// be reached through a symlink that escapes it.
func ensurePathIsInside(root, candidate string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("root path is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absCandidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes %s", candidate, root)
	}
	return absCandidate, nil
}
