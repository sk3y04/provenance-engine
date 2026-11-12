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
	"strconv"
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

// resolve reports the best known path for name: a PATH hit first, then the
// engine's tool download cache. The cache check mirrors go-ytdlp's layout,
// which stores binaries as "<name>-<version>" (with a plain "<name>" also
// possible) and must not install anything to report them.
func resolve(name string) Tool {
	t := Tool{Name: name}
	if p, err := lookPath(name); err == nil {
		t.Available = true
		t.Path = p
		return t
	}
	if p := resolveCache(name); p != "" {
		t.Available = true
		t.Path = p
	}
	return t
}

// resolveCache finds name in the go-ytdlp cache directory. A plain "<name>"
// file wins; otherwise the newest "<name>-<version>" file is preferred.
func resolveCache(name string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, cacheDir)
	plain := filepath.Join(dir, name)
	if info, err := os.Stat(plain); err == nil && !info.IsDir() {
		return plain
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestSuffix := "", ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		suffix, ok := strings.CutPrefix(e.Name(), name+"-")
		if !ok || suffix == "" {
			continue
		}
		if best == "" || versionLess(bestSuffix, suffix) {
			best, bestSuffix = e.Name(), suffix
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(dir, best)
}

// versionLess reports whether version a sorts before b. Dotted numeric parts
// compare numerically (2026.8.19 > 2026.1.1); anything that is not purely
// dotted-numeric falls back to byte order.
func versionLess(a, b string) bool {
	an, bn := parseVersion(a), parseVersion(b)
	if an != nil && bn != nil {
		for i := 0; i < len(an) && i < len(bn); i++ {
			if an[i] != bn[i] {
				return an[i] < bn[i]
			}
		}
		return len(an) < len(bn)
	}
	return a < b
}

// parseVersion splits a dotted-numeric version; it returns nil when any
// part is not a non-negative integer.
func parseVersion(v string) []int {
	parts := strings.Split(v, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		nums[i] = n
	}
	return nums
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
