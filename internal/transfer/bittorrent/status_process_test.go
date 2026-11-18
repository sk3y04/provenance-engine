package bittorrent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are the read-only status acceptance tests. They drive the real CLI
// binary against a staging root that another process owns, which is the point:
// status must work for a human or a script without joining the swarm, taking the
// writer lock, or touching the payload.

var (
	cliBuildOnce sync.Once
	cliBuildPath string
	cliBuildDir  string
	cliBuildErr  error
)

// provenanceBin builds ./cmd/provenance once per test binary. Building the real
// command (rather than calling the package API) keeps the acceptance tests
// honest about flags, exit codes and stdout/stderr separation.
func provenanceBin(t *testing.T) string {
	t.Helper()
	cliBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "provenance-cli-")
		if err != nil {
			cliBuildErr = err
			return
		}
		cliBuildDir = dir
		out := filepath.Join(dir, "provenance")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/provenance")
		cmd.Dir = filepath.Join("..", "..", "..")
		if b, err := cmd.CombinedOutput(); err != nil {
			cliBuildErr = fmt.Errorf("%v\n%s", err, b)
			return
		}
		cliBuildPath = out
	})
	if cliBuildErr != nil {
		t.Fatalf("build provenance CLI: %v", cliBuildErr)
	}
	return cliBuildPath
}

// runCLIStatus runs `provenance torrent status ...` and reports how long it took.
func runCLIStatus(t *testing.T, args ...string) (stdout, stderr string, code int, elapsed time.Duration) {
	t.Helper()
	bin := provenanceBin(t)
	cmd := exec.Command(bin, append([]string{"torrent", "status"}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	start := time.Now()
	err := cmd.Run()
	elapsed = time.Since(start)
	return out.String(), errOut.String(), exitCodeOf(err), elapsed
}

// statusDoc is the subset of the status JSON the acceptance tests assert on.
type statusDoc struct {
	StagingRoot string `json:"staging_root"`
	Jobs        []struct {
		JobID         string `json:"job_id"`
		InfoHash      string `json:"info_hash"`
		UpdatedAt     string `json:"updated_at"`
		RecordedState struct {
			Payload string `json:"payload"`
			Capture string `json:"capture"`
			Seeding string `json:"seeding"`
		} `json:"recorded_state"`
		Capture *struct {
			Manifest   string `json:"manifest"`
			CaptureKey string `json:"capture_key"`
		} `json:"capture"`
	} `json:"jobs"`
	Unreadable []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"unreadable"`
}

func parseStatusDoc(t *testing.T, stdout string) statusDoc {
	t.Helper()
	var doc statusDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("status stdout is not JSON: %v\n%s", err, stdout)
	}
	return doc
}

// snapshotRecords captures the files status must never touch: the job record,
// the retained metainfo, the published capture, and the payload. The torrent
// client's own piece database and the lock file are excluded because the owner
// may write them for reasons unrelated to status.
func snapshotRecords(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for p, sig := range snapshotTree(t, root) {
		base := filepath.Base(p)
		if base == "staging.lock" || strings.HasPrefix(base, ".torrent.db") {
			continue
		}
		out[p] = sig
	}
	return out
}

// TestCLIStatusDuringActiveSeeding is acceptance test 1: the real status
// command runs against a root owned by a seeding process, returns promptly,
// reports the recorded capture, and changes nothing.
func TestCLIStatusDuringActiveSeeding(t *testing.T) {
	content := patterned(18000, 'c')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"c.bin": content,
		"d.bin": patterned(6000, 'd'),
	})
	staging := t.TempDir()

	owner := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/c.bin",
		helperPeer:    s.peer(),
		helperSeed:    "1",
	})
	owner.expectLine("READY", 60*time.Second)
	waitFor(t, "committed capture while seeding", 90*time.Second, func() bool {
		return manifestCount(t, staging) == 1
	})
	before := snapshotRecords(t, staging)

	stdout, stderr, code, elapsed := runCLIStatus(t, "-o", staging, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d, stderr:\n%s", code, stderr)
	}
	if elapsed > 10*time.Second {
		t.Errorf("status took %s against an owned root; it must not wait for the writer", elapsed)
	}
	doc := parseStatusDoc(t, stdout)
	if len(doc.Unreadable) != 0 {
		t.Errorf("unreadable = %+v", doc.Unreadable)
	}
	if len(doc.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1:\n%s", len(doc.Jobs), stdout)
	}
	job := doc.Jobs[0]
	if job.InfoHash != s.infohash() {
		t.Errorf("infohash = %s, want %s", job.InfoHash, s.infohash())
	}
	if job.RecordedState.Capture != "committed" {
		t.Errorf("recorded capture = %q, want committed", job.RecordedState.Capture)
	}
	if job.RecordedState.Seeding != "seeding" {
		t.Errorf("recorded seeding = %q, want the last recorded value", job.RecordedState.Seeding)
	}
	if job.Capture == nil || job.Capture.Manifest == "" || job.Capture.CaptureKey == "" {
		t.Errorf("capture reference missing from status: %+v", job.Capture)
	}
	if diff := diffSnapshots(before, snapshotRecords(t, staging)); diff != "" {
		t.Errorf("status changed the staging tree:\n%s", diff)
	}

	// Still owned: status must not have taken or released the writer lock.
	competitor := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/c.bin",
		helperPeer:    s.peer(),
	})
	competitor.expectLine("BUSY", 60*time.Second)
	if code := competitor.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("competitor exit = %d, want busy (status disturbed ownership)", code)
	}

	// The human output must label the state as recorded, not live.
	human, _, code, _ := runCLIStatus(t, "-o", staging)
	if code != 0 {
		t.Fatalf("human status exit = %d", code)
	}
	for _, want := range []string{"capture=committed", "not a liveness check", "recorded"} {
		if !strings.Contains(human, want) {
			t.Errorf("human status missing %q:\n%s", want, human)
		}
	}

	owner.interrupt()
	if code := owner.wait(60 * time.Second); code != 0 {
		t.Fatalf("owner exit after interrupt = %d", code)
	}
}

// TestCLIStatusAfterKilledSeedingOwner is acceptance test 2: a SIGKILLed owner
// leaves a lock file and a record that says "seeding". Status must report the
// last recorded state without claiming liveness, and a later Fetch must still
// take ownership and reconcile.
func TestCLIStatusAfterKilledSeedingOwner(t *testing.T) {
	content := patterned(18000, 'k')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"k.bin": content})
	staging := t.TempDir()

	owner := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/k.bin",
		helperPeer:    s.peer(),
		helperSeed:    "1",
	})
	owner.expectLine("READY", 60*time.Second)
	waitFor(t, "committed capture while seeding", 90*time.Second, func() bool {
		return manifestCount(t, staging) == 1
	})

	lockPath := filepath.Join(staging, filepath.FromSlash(torrentStateDir), "staging.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected an owner lock file: %v", err)
	}

	owner.kill()
	if code := owner.wait(60 * time.Second); code != -1 {
		t.Fatalf("killed owner exit = %d, want the signal exit", code)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file vanished on SIGKILL; the test is no longer about a stale file: %v", err)
	}

	stdout, stderr, code, _ := runCLIStatus(t, "-o", staging, "--json")
	if code != 0 {
		t.Fatalf("status after SIGKILL exit = %d, stderr:\n%s", code, stderr)
	}
	doc := parseStatusDoc(t, stdout)
	if len(doc.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1:\n%s", len(doc.Jobs), stdout)
	}
	if got := doc.Jobs[0].RecordedState.Seeding; got != "seeding" {
		t.Errorf("recorded seeding = %q, want the value the dead process left behind", got)
	}
	if doc.Jobs[0].RecordedState.Capture != "committed" {
		t.Errorf("recorded capture = %q", doc.Jobs[0].RecordedState.Capture)
	}
	for _, forbidden := range []string{"\"live\"", "\"running\"", "\"alive\"", "\"active\""} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("status claims liveness via %s:\n%s", forbidden, stdout)
		}
	}

	human, _, code, _ := runCLIStatus(t, "-o", staging)
	if code != 0 {
		t.Fatalf("human status exit = %d", code)
	}
	if !strings.Contains(human, "not a liveness check") {
		t.Errorf("human status does not disclaim liveness:\n%s", human)
	}

	// A real fetch must still acquire the abandoned root and reconcile.
	next := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/k.bin",
		helperPeer:    s.peer(),
	})
	line, seen := next.expectLine("OK ", 90*time.Second)
	if code := next.wait(90 * time.Second); code != 0 {
		t.Fatalf("fetch after a killed owner exit = %d, output:\n%s", code, seen)
	}
	if fields(line)["reconciled"] != "true" {
		t.Errorf("fetch after a killed owner did not reconcile: %q", line)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests after recovery = %d, want 1", n)
	}

	// The recovering fetch did not seed, so the record must no longer claim the
	// dead process is seeding. Status reports the new recorded value.
	afterDoc := parseStatusDoc(t, mustStatusJSON(t, staging))
	if got := afterDoc.Jobs[0].RecordedState.Seeding; got != "stopped" {
		t.Errorf("recorded seeding after a non-seeding recovery = %q, want stopped", got)
	}
}

func mustStatusJSON(t *testing.T, staging string) string {
	t.Helper()
	stdout, stderr, code, _ := runCLIStatus(t, "-o", staging, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d, stderr:\n%s", code, stderr)
	}
	return stdout
}
