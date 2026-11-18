package bittorrent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// These tests run the backend in real, separate processes. The lock is an OS
// file lock and process death is part of the contract, so an in-process test
// cannot prove either.

// TestProcessOwnershipExcludesCompetingWriter is acceptance test 1: with a
// deterministic barrier, a second process on the same staging root is refused,
// writes nothing, and cannot publish a capture. Once the owner finishes, the
// competing job leaves exactly one run manifest for the capture key.
func TestProcessOwnershipExcludesCompetingWriter(t *testing.T) {
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{
		"a.bin": patterned(4000, 'a'),
		"b.bin": patterned(20000, 'b'),
	})
	staging := t.TempDir()

	owner := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/b.bin",
		helperPeer:    s.peer(),
		helperGate:    "stdin",
	})
	owner.expectLine("READY", 60*time.Second)

	// The owner holds the root and has not fetched anything yet.
	before := snapshotTree(t, staging)
	competitor := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/b.bin",
		helperPeer:    s.peer(),
	})
	competitor.expectLine("BUSY", 60*time.Second)
	if code := competitor.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("competitor exit = %d, want %d (busy)", code, helperExitBusy)
	}
	if diff := diffSnapshots(before, snapshotTree(t, staging)); diff != "" {
		t.Errorf("competing process touched staging:\n%s", diff)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Fatalf("competing process published %d capture manifest(s)", n)
	}

	// Release the barrier: the owner now does the work and exits.
	owner.release()
	okLine, seen := owner.expectLine("OK ", 90*time.Second)
	if code := owner.wait(60 * time.Second); code != 0 {
		t.Fatalf("owner exit = %d, output:\n%s", code, seen)
	}
	key := fields(okLine)["key"]
	if key == "" {
		t.Fatalf("owner did not report a capture key: %q", okLine)
	}
	if n := len(manifestsWithKey(t, staging, key)); n != 1 {
		t.Fatalf("manifests with key = %d, want exactly 1", n)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Fatalf("total manifests = %d, want 1", n)
	}

	// A retry after the owner exited succeeds and reconciles, still one capture.
	retry := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/b.bin",
		helperPeer:    s.peer(),
	})
	retryLine, retrySeen := retry.expectLine("OK ", 90*time.Second)
	if code := retry.wait(60 * time.Second); code != 0 {
		t.Fatalf("retry exit = %d, output:\n%s", code, retrySeen)
	}
	if fields(retryLine)["reconciled"] != "true" {
		t.Errorf("retry did not reconcile: %q", retryLine)
	}
	if n := len(manifestsWithKey(t, staging, key)); n != 1 {
		t.Fatalf("manifests with key = %d after retry, want 1", n)
	}
}

// TestKilledOwnerReleasesOwnership is acceptance test 2: SIGKILL the owner while
// it holds the lock, then prove a new process acquires the root and recovers the
// job through the existing reconcile path.
//
// Tested on linux only (this box). The mechanism is flock/LockFileEx, which the
// kernel releases when the last descriptor closes, so the same behaviour is
// expected on other unix platforms and on Windows; that is a claim about the
// primitive, not something this suite verified here.
func TestKilledOwnerReleasesOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process kill semantics differ on windows; not verified here")
	}
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(15000, 'k')})
	staging := t.TempDir()

	// A completed job to recover.
	first := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/a.bin",
		helperPeer:    s.peer(),
	})
	firstLine, firstSeen := first.expectLine("OK ", 90*time.Second)
	if code := first.wait(60 * time.Second); code != 0 {
		t.Fatalf("first fetch exit = %d, output:\n%s", code, firstSeen)
	}
	key := fields(firstLine)["key"]

	holder := startChild(t, map[string]string{
		helperOp:      "hold",
		helperStaging: staging,
	})
	holder.expectLine("READY", 60*time.Second)

	// Prove the holder really owns the root before killing it.
	blocked := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/a.bin",
		helperPeer:    s.peer(),
	})
	blocked.expectLine("BUSY", 60*time.Second)
	if code := blocked.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("competitor while holder alive exit = %d, want busy", code)
	}

	holder.kill()
	holder.wait(60 * time.Second)

	// The kernel drops the flock with the process, so this acquires.
	recoverer := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/a.bin",
	})
	line, seen := recoverer.expectLine("OK ", 90*time.Second)
	if code := recoverer.wait(60 * time.Second); code != 0 {
		t.Fatalf("recovery exit = %d, output:\n%s", code, seen)
	}
	got := fields(line)
	if got["reconciled"] != "true" {
		t.Errorf("recovery did not reconcile the committed capture: %q", line)
	}
	if got["bytes_read"] != "0" {
		t.Errorf("recovery read %s bytes, want 0 (staged payload reused)", got["bytes_read"])
	}
	if n := len(manifestsWithKey(t, staging, key)); n != 1 {
		t.Fatalf("manifests with key = %d, want 1 (no duplicate after kill)", n)
	}
}

// TestSeedingOwnerStaysExclusiveUntilCancelled is acceptance test 3: while a
// committed --seed job keeps the client up, the run manifest is readable and a
// competing process cannot mutate staging; cancelling the owner releases it.
func TestSeedingOwnerStaysExclusiveUntilCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interrupt delivery to a child process is not verified on windows")
	}
	content := patterned(18000, 's')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"s.bin": content})
	staging := t.TempDir()

	owner := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/s.bin",
		helperPeer:    s.peer(),
		helperSeed:    "1",
	})
	owner.expectLine("READY", 60*time.Second)
	waitFor(t, "committed capture while seeding", 90*time.Second, func() bool {
		return manifestCount(t, staging) == 1
	})

	// The capture is visible to downstream readers while seeding continues.
	cm := captureManifests(t, staging)[0]
	if len(cm.Items) != 1 || cm.Items[0].Sha256 != sha256Hex(content) {
		t.Fatalf("capture manifest while seeding = %+v", cm)
	}
	job := jobRecordFor(t, staging, s.infohash())
	if job.State.Seeding != SeedingActive || job.State.Capture != CaptureCommitted {
		t.Fatalf("state while seeding = %+v", job.State)
	}

	// A competing process is refused and cannot mutate staging.
	fileBefore := snapshotTree(t, staging)
	competitor := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/s.bin",
		helperPeer:    s.peer(),
	})
	competitor.expectLine("BUSY", 60*time.Second)
	if code := competitor.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("competitor exit = %d, want busy", code)
	}
	if diff := diffSnapshots(fileBefore, snapshotTree(t, staging)); diff != "" {
		t.Errorf("competitor mutated staging while the owner seeds:\n%s", diff)
	}

	// Cancel the owner; ownership must be released.
	owner.interrupt()
	if _, seen := owner.expectLine("OK ", 60*time.Second); seen == "" {
		t.Log("owner produced no OK line")
	}
	if code := owner.wait(60 * time.Second); code != 0 {
		t.Fatalf("owner exit after interrupt = %d", code)
	}
	after := jobRecordFor(t, staging, s.infohash())
	if after.State.Seeding != SeedingStopped {
		t.Errorf("seeding state after cancel = %q, want stopped", after.State.Seeding)
	}

	next := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/s.bin",
	})
	line, seen := next.expectLine("OK ", 60*time.Second)
	if code := next.wait(60 * time.Second); code != 0 {
		t.Fatalf("post-cancel fetch exit = %d, output:\n%s", code, seen)
	}
	if fields(line)["reconciled"] != "true" {
		t.Errorf("post-cancel fetch did not reconcile: %q", line)
	}
}

// TestOwnershipScopeAcrossSpellingsAndRoots is acceptance test 4:
//   - a symlinked spelling of the same root is the same owner;
//   - a different torrent in the same root is refused (root scope, not
//     infohash scope);
//   - genuinely independent roots proceed concurrently.
func TestOwnershipScopeAcrossSpellingsAndRoots(t *testing.T) {
	sA := newSeeder(t, "fixture", 16*1024, map[string][]byte{"a.bin": patterned(5000, 'a')})
	sB := newSeeder(t, "other", 16*1024, map[string][]byte{"z.bin": patterned(5000, 'z')})
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	owner := startChild(t, map[string]string{helperOp: "hold", helperStaging: root, helperGate: "stdin"})
	owner.expectLine("READY", 60*time.Second)

	for _, spelling := range []string{alias, filepath.Join(root, ".", "")} {
		c := startChild(t, map[string]string{
			helperOp:      "fetch",
			helperStaging: spelling,
			helperSource:  sA.magnet(),
			helperSelect:  "fixture/a.bin",
			helperPeer:    sA.peer(),
		})
		c.expectLine("BUSY", 60*time.Second)
		if code := c.wait(60 * time.Second); code != helperExitBusy {
			t.Errorf("spelling %q exit = %d, want busy", spelling, code)
		}
	}

	// A different torrent sharing the root is still excluded: the piece
	// database, payload directories and run-manifest names are root-scoped.
	other := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: root,
		helperSource:  sB.magnet(),
		helperSelect:  "other/z.bin",
		helperPeer:    sB.peer(),
	})
	other.expectLine("BUSY", 60*time.Second)
	if code := other.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("different torrent in the same root exit = %d, want busy", code)
	}

	// An independent root is not affected by the held one.
	independentRoot := t.TempDir()
	independent := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: independentRoot,
		helperSource:  sA.magnet(),
		helperSelect:  "fixture/a.bin",
		helperPeer:    sA.peer(),
	})
	if _, seen := independent.expectLine("OK ", 90*time.Second); seen == "" {
		t.Fatal("independent root produced no result")
	}
	if code := independent.wait(60 * time.Second); code != 0 {
		t.Fatalf("independent root exit = %d (roots must not serialize)", code)
	}
	if n := manifestCount(t, independentRoot); n != 1 {
		t.Errorf("independent root manifests = %d, want 1", n)
	}
	if n := manifestCount(t, root); n != 0 {
		t.Errorf("held root gained %d manifest(s) while owned", n)
	}

	owner.release()
	if code := owner.wait(60 * time.Second); code != 0 {
		t.Fatalf("owner exit = %d", code)
	}
	// The different torrent can now use the root it was refused.
	retry := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: root,
		helperSource:  sB.magnet(),
		helperSelect:  "other/z.bin",
		helperPeer:    sB.peer(),
	})
	if _, seen := retry.expectLine("OK ", 90*time.Second); seen == "" {
		t.Fatal("retry produced no result")
	}
	if code := retry.wait(60 * time.Second); code != 0 {
		t.Fatalf("retry after release exit = %d", code)
	}
}

// TestInspectDoesNotNeedStagingOwnership is the process-level counterpart of the
// metadata-first slice: a separate process resolves metadata for a magnet while
// another process owns the staging root, without a Session, a lock, or any write
// to that root.
func TestInspectDoesNotNeedStagingOwnership(t *testing.T) {
	content := patterned(9000, 'i')
	s := newSeeder(t, "fixture", 16*1024, map[string][]byte{"i.bin": content})
	staging := t.TempDir()

	owner := startChild(t, map[string]string{
		helperOp:      "hold",
		helperStaging: staging,
		helperGate:    "stdin",
	})
	owner.expectLine("READY", 60*time.Second)
	before := snapshotTree(t, staging)

	inspector := startChild(t, map[string]string{
		helperOp:     "inspect",
		helperSource: s.magnet(),
		helperPeer:   s.peer(),
	})
	line, seen := inspector.expectLine("OK ", 60*time.Second)
	if code := inspector.wait(60 * time.Second); code != 0 {
		t.Fatalf("inspector exit = %d, output:\n%s", code, seen)
	}
	if fields(line)["files"] != "1" {
		t.Errorf("inspect reported %q, want 1 file", line)
	}
	if diff := diffSnapshots(before, snapshotTree(t, staging)); diff != "" {
		t.Errorf("inspection changed the owned staging root:\n%s", diff)
	}

	// The owner is still the owner: it was never queued behind the inspector.
	competitor := startChild(t, map[string]string{
		helperOp:      "fetch",
		helperStaging: staging,
		helperSource:  s.magnet(),
		helperSelect:  "fixture/i.bin",
		helperPeer:    s.peer(),
	})
	competitor.expectLine("BUSY", 60*time.Second)
	if code := competitor.wait(60 * time.Second); code != helperExitBusy {
		t.Fatalf("competitor exit = %d, want busy", code)
	}

	owner.release()
	if code := owner.wait(60 * time.Second); code != 0 {
		t.Fatalf("owner exit = %d", code)
	}
}
