// compat_baseline_test.go locks the CLI surface the refactoring guardrails
// promise to preserve: exit codes, stdout/stderr separation and the documented
// --json shapes. It runs the real binary built from this module rather than
// calling command constructors in-process, so exit codes and stream routing are
// the shipped behavior and not a test harness convenience.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cliBinary is built once by TestMain and shared by every case. The build costs
// one warm-cache compile per test binary, which is the price of asserting on real
// process exit codes.
var cliBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "provenance-compat-")
	if err != nil {
		panic("create compat build dir: " + err.Error())
	}
	bin := filepath.Join(dir, "provenance")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/provenance")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		_, _ = os.Stderr.WriteString(string(out))
		panic("build provenance binary: " + err.Error())
	}
	cliBinary = bin

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// runCLI executes the built binary with a hermetic environment. HOME and the
// cache root point at a per-call temp dir so no case can read or write the
// developer's real CLI state, and every PROVENANCE_* override is cleared.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(cliBinary, args...)
	home := t.TempDir()
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"PROVENANCE_HISTORY_FILE="+filepath.Join(home, "history.json"),
		"PROVENANCE_SESSION_DIR="+filepath.Join(home, "sessions"),
		"PROVENANCE_COLLECTION_FILE="+filepath.Join(home, "collection.json"),
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code = 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errb.String(), code
}

// TestCLIHelpAndVersionExitZero covers the documented success codes for the
// surface every script depends on.
func TestCLIHelpAndVersionExitZero(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		wants string
	}{
		{"version", []string{"--version"}, "provenance"},
		{"root-help", []string{"--help"}, "Usage:"},
		{"help-command", []string{"help"}, "Usage:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stdout, tc.wants) {
				t.Fatalf("stdout does not contain %q:\n%s", tc.wants, stdout)
			}
			if stderr != "" {
				t.Fatalf("help/version wrote to stderr: %q", stderr)
			}
		})
	}
}

// TestCLIUsageErrorsGoToStderr proves the error contract: the diagnostic goes to
// stderr, stdout stays parseable, and the exit code is 1.
func TestCLIUsageErrorsGoToStderr(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"unknown-command", []string{"frobnicate"}},
		{"torrent-status-invalid-id", []string{"torrent", "-o", root, "status", "not-a-job", "--json"}},
		{"torrent-status-missing-job", []string{"torrent", "-o", root, "status", "bt-" + strings.Repeat("e", 40), "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args...)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr)
			}
			// Two prefixes reach stderr by design and both are preserved:
			// cobra's "Error:" plus usage for a rejected invocation, and main's
			// own "error:" line (with an optional "hint:" line) for the exit.
			if !strings.Contains(stderr, "error:") {
				t.Fatalf("stderr carries no error line: %q", stderr)
			}
			if strings.Contains(stderr, "hint:") && !strings.Contains(stderr, "error:") {
				t.Fatalf("a hint was printed without its error: %q", stderr)
			}
			if stdout != "" {
				t.Fatalf("a failed command wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestCLITorrentQueueListJSONShape locks the documented --json document for the
// queue listing: a JSON object on stdout, diagnostics off stdout, exit 0.
func TestCLITorrentQueueListJSONShape(t *testing.T) {
	root := t.TempDir()

	stdout, stderr, code := runCLI(t, "torrent", "-o", root, "queue", "list", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("queue list --json wrote to stderr: %q", stderr)
	}
	var doc struct {
		RunnerHeld *bool            `json:"runner_held"`
		Entries    []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if doc.RunnerHeld == nil {
		t.Fatalf("document is missing runner_held: %s", stdout)
	}
	if *doc.RunnerHeld {
		t.Fatalf("an empty queue reported runner_held=true: %s", stdout)
	}
	if doc.Entries == nil || len(doc.Entries) != 0 {
		t.Fatalf("entries = %v, want an empty array (got %s)", doc.Entries, stdout)
	}

	// The human view stays on stdout and reports the same absence, so scripts
	// that do not pass --json keep working.
	stdout, stderr, code = runCLI(t, "torrent", "-o", root, "queue", "list")
	if code != 0 {
		t.Fatalf("human exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "runner:") || !strings.Contains(stdout, "no entries") {
		t.Fatalf("human queue listing = %q", stdout)
	}
}

// TestCLITorrentStatusEmptyRootIsSuccess locks the documented success case for a
// staging root with no recorded jobs: exit 0, no files created, stdout only.
func TestCLITorrentStatusEmptyRootIsSuccess(t *testing.T) {
	root := t.TempDir()

	stdout, stderr, code := runCLI(t, "torrent", "-o", root, "status")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("status on an empty root wrote to stderr: %q", stderr)
	}
	if !strings.Contains(stdout, "no torrent jobs recorded") {
		t.Fatalf("stdout = %q", stdout)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read staging root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("status created files in the staging root: %v", entries)
	}
}
