// Package capability reports which external tools an operation needs and whether
// they are present. A probe is read-only: it never installs a tool, never
// downloads anything, and never mutates the filesystem, so a health check or a
// capability request cannot have side effects.
package capability

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const cacheDir = "go-ytdlp"

// Tool names.
const (
	ToolYtdlp   = "yt-dlp"
	ToolFfmpeg  = "ffmpeg"
	ToolFFprobe = "ffprobe"
	ToolChrome  = "chrome"
)

// Tool is the read-only result of probing one external tool.
type Tool struct {
	Name      string
	Required  bool
	Available bool
	Path      string
}

// Set is the capability report for one engine.
type Set []Tool

// ChromeCandidates are the executable names probed when no explicit path is
// configured.
var ChromeCandidates = []string{
	"google-chrome",
	"chrome",
	"chromium",
	"chromium-browser",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
}

// lookPath is a package variable rather than a direct exec.LookPath call so
// tests can resolve fake tools without touching the filesystem.
var lookPath = exec.LookPath

// Probe reports the tools reachable for an operation. chromePath overrides the
// candidate search when set. The context is honored so a caller can bound the
// probe; no tool is installed or written to disk.
func Probe(ctx context.Context, chromePath string) Set {
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	ytdlp := resolve(ToolYtdlp)
	ytdlp.Required = true
	return Set{ytdlp, resolve(ToolFfmpeg), resolve(ToolFFprobe), resolveChrome(chromePath)}
}

func resolve(name string) Tool {
	t := Tool{Name: name}
	if p, err := lookPath(name); err == nil {
		t.Available = true
		t.Path = p
		return t
	}
	// The engine's tool library also resolves from its own download cache. A
	// probe must report that without installing anything.
	if p, err := os.UserCacheDir(); err == nil {
		candidate := filepath.Join(p, cacheDir, name)
		if info, serr := os.Stat(candidate); serr == nil && !info.IsDir() {
			t.Available = true
			t.Path = candidate
		}
	}
	return t
}

func resolveChrome(chromePath string) Tool {
	t := Tool{Name: ToolChrome}
	if p := strings.TrimSpace(chromePath); p != "" {
		if resolved, err := lookPath(p); err == nil {
			t.Available = true
			t.Path = resolved
		}
		return t
	}
	for _, name := range ChromeCandidates {
		if p, err := lookPath(name); err == nil {
			t.Available = true
			t.Path = p
			return t
		}
	}
	return t
}

// Path returns the resolved path for a tool, or "" when it is not available.
func (s Set) Path(name string) string {
	for _, t := range s {
		if t.Name == name {
			return t.Path
		}
	}
	return ""
}

// Available reports whether name was found by the probe.
func (s Set) Available(name string) bool {
	t, ok := s.Get(name)
	return ok && t.Available
}

// Get returns the tool entry for name, or false.
func (s Set) Get(name string) (Tool, bool) {
	for _, t := range s {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
