package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTool puts an executable stub on a temp PATH dir.
func fakeTool(t *testing.T, pathDir, name string) {
	t.Helper()
	p := filepath.Join(pathDir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hermeticHome points HOME and XDG_CACHE_HOME at an empty temp dir so the
// probe cannot resolve tools from the user's real go-ytdlp cache.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("PROVENANCE_DATABASE_URL", "")
	return home
}

func toolPath(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, n := range []string{"yt-dlp", "ffmpeg", "ffprobe", "google-chrome"} {
		fakeTool(t, p, n)
	}
	t.Setenv("PATH", p)
	return p
}

func findCheck(rep Report, name string) (Check, bool) {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestRunReportsRequiredToolsOK(t *testing.T) {
	hermeticHome(t)
	toolPath(t)

	rep := Run(context.Background(), Options{PingTimeout: 2 * time.Second})
	if !rep.OK {
		t.Fatalf("expected OK with all required tools present, got %#v", rep.Checks)
	}
	for _, name := range []string{"yt-dlp", "ffmpeg", "ffprobe"} {
		c, ok := findCheck(rep, name)
		if !ok {
			t.Fatalf("check %q missing from report", name)
		}
		if c.Status != OK {
			t.Fatalf("check %q: status = %s, want ok", name, c.Status)
		}
		if !c.Required {
			t.Fatalf("check %q must be marked required", name)
		}
	}
	// Optional checks must not break OK.
	db, ok := findCheck(rep, "database")
	if !ok || db.Status != NotConfigured {
		t.Fatalf("database check = %#v, want not_configured", db)
	}
	ck, ok := findCheck(rep, "cookies")
	if !ok || ck.Status != NotConfigured {
		t.Fatalf("cookies check = %#v, want not_configured", ck)
	}
}

func TestRunFailsWhenRequiredToolMissing(t *testing.T) {
	home := hermeticHome(t)
	p := t.TempDir()
	fakeTool(t, p, "ffmpeg")
	fakeTool(t, p, "ffprobe")
	t.Setenv("PATH", p)

	rep := Run(context.Background(), Options{PingTimeout: 2 * time.Second})
	if rep.OK {
		t.Fatal("expected not OK when yt-dlp is missing")
	}
	c, ok := findCheck(rep, "yt-dlp")
	if !ok || c.Status != Missing {
		t.Fatalf("yt-dlp check = %#v, want missing", c)
	}
	// The cache check must pass on the fresh hermetic home.
	ca, ok := findCheck(rep, "cache")
	if !ok || ca.Status != OK || !strings.Contains(ca.Detail, home) {
		t.Fatalf("cache check = %#v", ca)
	}
}

func TestRunChecksCookiesFile(t *testing.T) {
	hermeticHome(t)
	toolPath(t)

	good := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(good, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Options{CookiesFile: good, PingTimeout: time.Second})
	c, _ := findCheck(rep, "cookies")
	if c.Status != OK {
		t.Fatalf("existing cookies file must be ok, got %#v", c)
	}

	rep = Run(context.Background(), Options{CookiesFile: filepath.Join(t.TempDir(), "absent.txt"), PingTimeout: time.Second})
	c, _ = findCheck(rep, "cookies")
	if c.Status != Missing {
		t.Fatalf("absent cookies file must be missing, got %#v", c)
	}
	if c.Required {
		t.Fatal("cookies must not be a required check")
	}
}

func TestRunDatabaseUnreachable(t *testing.T) {
	hermeticHome(t)
	toolPath(t)
	t.Setenv("PROVENANCE_DATABASE_URL", "postgres://user:***@127.0.0.1:1/provenance?connect_timeout=2")

	rep := Run(context.Background(), Options{PingTimeout: 2 * time.Second})
	// Database is optional: OK must stay true.
	if !rep.OK {
		t.Fatalf("an unreachable optional database must not fail the report, got %#v", rep.Checks)
	}
	c, ok := findCheck(rep, "database")
	if !ok || c.Status != Missing {
		t.Fatalf("database check = %#v, want missing", c)
	}
	if !strings.Contains(c.Detail, "127.0.0.1") {
		t.Fatalf("detail should name the target, got %q", c.Detail)
	}
}

func TestRenderListsChecksAndResult(t *testing.T) {
	hermeticHome(t)
	toolPath(t)

	rep := Run(context.Background(), Options{PingTimeout: time.Second})
	var sb strings.Builder
	Render(&sb, rep)
	out := sb.String()

	for _, want := range []string{"yt-dlp", "ffmpeg", "ffprobe", "cookies", "cache", "database"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "result: ok") {
		t.Fatalf("rendered output missing result line:\n%s", out)
	}
}
