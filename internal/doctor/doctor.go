// Package doctor checks the prerequisites provenance needs to operate:
// external tools, cookies, cache, and the optional database. The probe is
// side-effect free apart from creating its own cache directory and a short
// database ping, so it is safe to run at any time.
package doctor

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sk3y04/provenance-engine/internal/capability"
)

// Status is the outcome of one check.
type Status string

const (
	// OK means the check passed.
	OK Status = "ok"
	// Missing means the thing is required-ish but absent (tool not found,
	// file absent, database unreachable).
	Missing Status = "missing"
	// NotConfigured means an optional feature was not configured; this is
	// never a failure.
	NotConfigured Status = "not_configured"
)

// Check is one line of the report.
type Check struct {
	Name     string
	Status   Status
	Detail   string
	Required bool
}

// Options configures a doctor run.
type Options struct {
	CookiesFile string // optional Netscape cookies file to verify
	ChromePath  string // optional Chrome/Chromium override
	// PingTimeout bounds the database ping. Zero uses DefaultPingTimeout.
	PingTimeout time.Duration
}

// DefaultPingTimeout bounds the database ping.
const DefaultPingTimeout = 5 * time.Second

// Report is the full check result. OK is true when every required check
// passed; optional checks never affect it.
type Report struct {
	Checks []Check
	OK     bool
}

// Run performs every check and returns the report.
func Run(ctx context.Context, opts Options) Report {
	timeout := opts.PingTimeout
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}

	set := capability.Probe(ctx, opts.ChromePath)
	checks := []Check{
		toolCheck(capability.ToolYtdlp, set, true),
		toolCheck(capability.ToolFfmpeg, set, true),
		toolCheck(capability.ToolFFprobe, set, true),
		toolCheck(capability.ToolChrome, set, false),
		cookiesCheck(opts.CookiesFile),
		cacheCheck(),
		databaseCheck(ctx, timeout),
	}
	rep := Report{Checks: checks, OK: true}
	for _, c := range checks {
		if c.Required && c.Status != OK {
			rep.OK = false
		}
	}
	return rep
}

func toolCheck(name string, set capability.Set, required bool) Check {
	t, _ := set.Get(name)
	if t.Available {
		return Check{Name: name, Status: OK, Detail: t.Path, Required: required}
	}
	detail := "not found on PATH"
	if name == capability.ToolYtdlp {
		detail = "not found (run `provenance install`)"
	}
	if name == capability.ToolChrome {
		detail = "not found (browser fallback and import-web unavailable)"
	}
	return Check{Name: name, Status: Missing, Detail: detail, Required: required}
}

func cookiesCheck(path string) Check {
	if strings.TrimSpace(path) == "" {
		return Check{Name: "cookies", Status: NotConfigured, Detail: "not set"}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
		return Check{Name: "cookies", Status: Missing, Detail: abs + ": not a regular file"}
	}
	return Check{Name: "cookies", Status: OK, Detail: abs}
}

func cacheCheck() Check {
	base, err := os.UserCacheDir()
	if err != nil {
		return Check{Name: "cache", Status: Missing, Detail: "cannot resolve cache dir: " + err.Error(), Required: true}
	}
	dir := filepath.Join(base, "provenance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Check{Name: "cache", Status: Missing, Detail: dir + ": " + err.Error(), Required: true}
	}
	f, err := os.CreateTemp(dir, "doctor-*")
	if err != nil {
		return Check{Name: "cache", Status: Missing, Detail: dir + " is not writable: " + err.Error(), Required: true}
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return Check{Name: "cache", Status: OK, Detail: dir, Required: true}
}

func databaseCheck(ctx context.Context, timeout time.Duration) Check {
	dsn := os.Getenv("PROVENANCE_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		return Check{
			Name:   "database",
			Status: NotConfigured,
			Detail: "not configured (set PROVENANCE_DATABASE_URL for vault/search)",
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return Check{Name: "database", Status: Missing, Detail: "invalid DSN: " + err.Error()}
	}
	defer pool.Close()
	if pingErr := pool.Ping(ctx); pingErr != nil {
		return Check{Name: "database", Status: Missing, Detail: databaseTarget(dsn) + ": " + pingErr.Error()}
	}
	return Check{Name: "database", Status: OK, Detail: databaseTarget(dsn)}
}

// databaseTarget renders the host part of a DSN for display (no password).
func databaseTarget(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "database"
	}
	return u.Host
}

// Render writes the report as a plain table to w.
func Render(w io.Writer, rep Report) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "provenance doctor")
	for _, c := range rep.Checks {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Name, c.Status, c.Detail)
	}
	_ = tw.Flush()
	if rep.OK {
		_, _ = fmt.Fprintln(w, "result: ok")
	} else {
		_, _ = fmt.Fprintln(w, "result: failed — required check(s) not ok")
	}
}
