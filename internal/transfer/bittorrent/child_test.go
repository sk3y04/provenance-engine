package bittorrent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stderrBuf synchronises the os/exec copy goroutine's writes with the test's
// reads. A bare bytes.Buffer assigned to cmd.Stderr races: exec copies the
// child's stderr in its own goroutine, and nothing orders that goroutine
// against a test-side read before Wait returns (observed as a DATA RACE in the
// v0.10.0 release gate when expectLine reported a failure while the child's
// library logs were still draining).
type stderrBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *stderrBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *stderrBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// child is one real process running the torrent backend in helper mode.
type child struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *stderrBuf
	exited bool
	code   int
}

// startChild re-executes the test binary in helper mode. The child is a real
// process with its own file descriptors, which is the only way to prove
// flock/LockFileEx behaviour and process-death release.
func startChild(t *testing.T, env map[string]string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	c := &child{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 32), stderr: stderr}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() { c.stop() })
	return c
}

// expectLine waits for a line with the given prefix and returns all observed
// lines so far plus the match.
func (c *child) expectLine(prefix string, timeout time.Duration) (string, string) {
	c.t.Helper()
	deadline := time.After(timeout)
	var seen []string
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				c.t.Fatalf("child exited before %q; saw %v, stderr:\n%s", prefix, seen, c.stderr.String())
			}
			seen = append(seen, line)
			if strings.HasPrefix(line, prefix) {
				return line, strings.Join(seen, "\n")
			}
		case <-deadline:
			c.t.Fatalf("timeout waiting for %q; saw %v, stderr:\n%s", prefix, seen, c.stderr.String())
		}
	}
}

// release closes the child's stdin, which is the gate the helper waits on.
func (c *child) release() {
	c.t.Helper()
	if err := c.stdin.Close(); err != nil {
		c.t.Fatalf("close child stdin: %v", err)
	}
}

// wait blocks for the process and returns its exit code.
func (c *child) wait(timeout time.Duration) int {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		c.exited = true
		c.code = exitCodeOf(err)
		return c.code
	case <-time.After(timeout):
		_ = c.cmd.Process.Kill()
		c.t.Fatalf("child did not exit within %s; stderr:\n%s", timeout, c.stderr.String())
		return -1
	}
}

// kill terminates the child without giving it a chance to release anything.
func (c *child) kill() {
	c.t.Helper()
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		c.t.Fatalf("kill child: %v", err)
	}
}

// interrupt sends a graceful cancellation request.
func (c *child) interrupt() {
	c.t.Helper()
	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		c.t.Fatalf("interrupt child: %v", err)
	}
}

// stop is the cleanup path: never leave a helper process behind.
func (c *child) stop() {
	if c.exited || c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// fields parses the helper's "OK key=value ..." lines.
func fields(line string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

// snapshotTree records the size and modification time of every file under root,
// so a test can prove that a competing process wrote nothing.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out[p] = fmt.Sprintf("%d/%d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// diffSnapshots reports file-level differences between two snapshots.
func diffSnapshots(before, after map[string]string) string {
	var b strings.Builder
	for p, sig := range after {
		if prev, ok := before[p]; !ok {
			fmt.Fprintf(&b, "  created %s (%s)\n", p, sig)
		} else if prev != sig {
			fmt.Fprintf(&b, "  modified %s (%s -> %s)\n", p, prev, sig)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			fmt.Fprintf(&b, "  removed %s\n", p)
		}
	}
	return b.String()
}

// manifestCount is the number of run manifests currently published.
func manifestCount(t *testing.T, staging string) int {
	t.Helper()
	return len(captureManifests(t, staging))
}

// jobStateDigest summarises the durable job record so a test can assert that a
// competing writer left it alone without depending on timestamps.
func jobStateDigest(t *testing.T, staging, infohash string) string {
	t.Helper()
	job := jobRecordFor(t, staging, infohash)
	key := ""
	if job.Capture != nil {
		key = job.Capture.Key
	}
	return strings.Join([]string{
		job.Format,
		job.JobID,
		strings.Join(job.Selection, "|"),
		job.State.Payload,
		job.State.Capture,
		job.State.Seeding,
		key,
	}, ";")
}
