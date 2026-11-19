package bittorrent

// Isolated workspaces: acceptance tests for BT-05. A workspace is one job's own
// provenance root (see workspace.go), keyed by the full canonical identity, with
// its own payload, completion database, records, lock and capture namespace.
// Everything here uses real sessions and real transfers over loopback.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
)

// isolatedConfig is the direct, hermetic configuration for workspace tests.
func isolatedConfig(root string, peers ...string) Config {
	return Config{
		StagingDir:      root,
		Peers:           peers,
		Discovery:       DiscoveryDirect,
		MetadataTimeout: 30 * time.Second,
		Timeout:         90 * time.Second,
		MaxPeers:        4,
	}
}

func mustWorkspacePath(t *testing.T, root string, id Identity) string {
	t.Helper()
	ws, err := WorkspacePath(root, id)
	if err != nil {
		t.Fatalf("WorkspacePath(%s): %v", id, err)
	}
	return ws
}

func identityOfMetainfo(t *testing.T, mi *metainfo.MetaInfo) Identity {
	t.Helper()
	id, err := IdentityFromMetaInfo(mi)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

// TestIsolatedTwoConcurrentJobsSameName is acceptance 1: two real sessions run
// concurrently under one top-level root, on distinct torrents that share a
// display name and relative file names but not their bytes. Payloads,
// completion databases, job records and captures must all be distinct, and both
// digests correct. The barrier is a channel, not a sleep.
func TestIsolatedTwoConcurrentJobsSameName(t *testing.T) {
	filesA := map[string][]byte{"a.bin": patterned(48000, 91)}
	filesB := map[string][]byte{"a.bin": patterned(48000, 97)}
	seedA := newSeeder(t, "fixture", 16<<10, filesA)
	seedB := newSeeder(t, "fixture", 16<<10, filesB)
	if seedA.infohash() == seedB.infohash() {
		t.Fatal("the two fixtures have the same identity; the test proves nothing")
	}

	root := t.TempDir()
	openA, err := OpenIsolated(context.Background(), isolatedConfig(root, seedA.peer()), seedA.magnet())
	if err != nil {
		t.Fatalf("open workspace A: %v", err)
	}
	t.Cleanup(func() { _ = openA.Close() })
	openB, err := OpenIsolated(context.Background(), isolatedConfig(root, seedB.peer()), seedB.magnet())
	if err != nil {
		t.Fatalf("open workspace B: %v", err)
	}
	t.Cleanup(func() { _ = openB.Close() })

	// Both workspaces exist and are distinct before either transfer starts, so
	// the concurrency below is not a race for a directory.
	if openA.Workspace() == openB.Workspace() {
		t.Fatalf("both jobs resolved to one workspace: %s", openA.Workspace())
	}
	for _, ws := range []string{openA.Workspace(), openB.Workspace()} {
		if fi, serr := os.Stat(ws); serr != nil || !fi.IsDir() {
			t.Fatalf("workspace %s missing: %v", ws, serr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	type result struct {
		cap Capture
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, job := range []*IsolatedJob{openA, openB} {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cap, ferr := job.Fetch(ctx, []string{"fixture/a.bin"})
			results <- result{cap: cap, err: ferr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	caps := make([]Capture, 0, 2)
	for res := range results {
		if res.err != nil {
			t.Fatalf("concurrent fetch: %v", res.err)
		}
		caps = append(caps, res.cap)
	}
	if len(caps) != 2 {
		t.Fatalf("got %d captures, want 2", len(caps))
	}
	if caps[0].CaptureKey == caps[1].CaptureKey {
		t.Error("the two jobs produced the same capture key")
	}
	wantDigests := map[string]bool{sha256Hex(filesA["a.bin"]): true, sha256Hex(filesB["a.bin"]): true}
	for _, cap := range caps {
		if len(cap.Assets) != 1 {
			t.Fatalf("capture has %d assets, want 1: %+v", len(cap.Assets), cap.Assets)
		}
		if !wantDigests[cap.Assets[0].SHA256] {
			t.Errorf("captured digest %s matches neither fixture", cap.Assets[0].SHA256)
		}
		if cap.StagingDir != mustWorkspacePath(t, root, identityOfCapture(t, cap)) {
			t.Errorf("capture staging dir %s is not its workspace", cap.StagingDir)
		}
	}
	if caps[0].Assets[0].SHA256 == caps[1].Assets[0].SHA256 {
		t.Error("both jobs captured identical bytes")
	}

	// Payloads are in different workspaces and hold their own bytes.
	payloadA := filepath.Join(openA.Workspace(), "fixture", "a.bin")
	payloadB := filepath.Join(openB.Workspace(), "fixture", "a.bin")
	gotA, err := os.ReadFile(payloadA)
	if err != nil {
		t.Fatalf("payload A: %v", err)
	}
	gotB, err := os.ReadFile(payloadB)
	if err != nil {
		t.Fatalf("payload B: %v", err)
	}
	if string(gotA) != string(filesA["a.bin"]) || string(gotB) != string(filesB["a.bin"]) {
		t.Error("a workspace payload does not match its fixture")
	}

	// Completion databases are per workspace, not the root.
	for _, ws := range []string{openA.Workspace(), openB.Workspace()} {
		db := filepath.Join(ws, filepath.FromSlash(workspaceCompletionDir), ".torrent.db")
		if _, serr := os.Stat(db); serr != nil {
			t.Errorf("workspace %s has no completion database at %s: %v", ws, db, serr)
		}
		if _, serr := os.Stat(filepath.Join(ws, ".torrent.db")); serr == nil {
			t.Errorf("workspace %s has a root-level completion database", ws)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".torrent.db")); err == nil {
		t.Error("the top-level root grew a completion database")
	}

	// Job records are filed under their own job id, each describing its identity.
	for _, job := range []*IsolatedJob{openA, openB} {
		record := jobRecordFor(t, job.Workspace(), job.JobID())
		if record.JobID != job.JobID() {
			t.Errorf("record job id = %s, want %s", record.JobID, job.JobID())
		}
		if record.InfoHash != job.Identity().V1Hex() {
			t.Errorf("record infohash = %s, want %s", record.InfoHash, job.Identity().V1Hex())
		}
	}
	// Nothing leaked into the legacy layout of the root.
	for _, name := range []string{"fixture", ".provenance/torrent/bt-" + seedA.infohash() + ".job.json"} {
		if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(name))); serr == nil {
			t.Errorf("isolated job wrote legacy path %s", name)
		}
	}
}

// identityOfCapture recovers the identity from a capture's job id.
func identityOfCapture(t *testing.T, cap Capture) Identity {
	t.Helper()
	job, err := readJob(filepath.Join(cap.StagingDir, filepath.FromSlash(torrentStateDir), cap.JobID+".job.json"))
	if err != nil {
		t.Fatalf("read job record for %s: %v", cap.JobID, err)
	}
	id, err := identityFromJob(job)
	if err != nil {
		t.Fatalf("identity from record: %v", err)
	}
	return id
}

// TestIsolatedWorkspaceBusyAndRecovery is acceptance 2: a held workspace refuses
// a second writer, an unrelated workspace transfers while it is held, and a
// cancelled owner leaves a recoverable payload rather than a duplicate run.
func TestIsolatedWorkspaceBusyAndRecovery(t *testing.T) {
	// Rate limited so the first attempt can be cancelled mid-transfer.
	seedA := newSeederRateLimited(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(200000, 101)}, 64<<10)
	filesB := map[string][]byte{"a.bin": patterned(24000, 103)}
	seedB := newSeeder(t, "fixture", 16<<10, filesB)
	root := t.TempDir()

	owner, err := OpenIsolated(context.Background(), isolatedConfig(root, seedA.peer()), seedA.magnet())
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	defer func() { _ = owner.Close() }()

	// A second writer for the same workspace is busy, and learns nothing else.
	_, err = OpenIsolated(context.Background(), isolatedConfig(root, seedA.peer()), seedA.magnet())
	if !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("second writer error = %v, want ErrStagingBusy", err)
	}

	// An unrelated workspace is unaffected while the first is held.
	other, err := OpenIsolated(context.Background(), isolatedConfig(root, seedB.peer()), seedB.magnet())
	if err != nil {
		t.Fatalf("open unrelated workspace while another is held: %v", err)
	}
	ctxB, cancelB := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelB()
	capB, err := other.Fetch(ctxB, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("unrelated workspace transfer while the other is held: %v", err)
	}
	if len(capB.Assets) != 1 || capB.Assets[0].SHA256 != sha256Hex(filesB["a.bin"]) {
		t.Fatalf("unrelated capture = %+v", capB.Assets)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close unrelated: %v", err)
	}

	// Cancel the owner mid-transfer, then reopen its workspace and recover.
	ctxA, cancelA := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, ferr := owner.Fetch(ctxA, []string{"fixture/a.bin"})
		done <- ferr
	}()
	waitFor(t, "the first transfer to start", 30*time.Second, func() bool {
		return len(listPartFiles(t, owner.Workspace())) > 0
	})
	cancelA()
	select {
	case ferr := <-done:
		if ferr == nil {
			t.Fatal("cancelled transfer reported success")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("cancelled transfer did not return")
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close owner after cancel: %v", err)
	}
	if got := len(captureManifests(t, owner.Workspace())); got != 0 {
		t.Fatalf("a cancelled transfer published %d manifest(s)", got)
	}

	// Reopen: ownership is free, retained bytes are reused, exactly one run.
	reopened, err := OpenIsolated(context.Background(), isolatedConfig(root, seedA.peer()), seedA.magnet())
	if err != nil {
		t.Fatalf("reopen after cancel: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.Workspace() != owner.Workspace() {
		t.Fatalf("reopen landed in %s, want %s", reopened.Workspace(), owner.Workspace())
	}
	ctxR, cancelR := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancelR()
	recovered, err := reopened.Fetch(ctxR, []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("recovery fetch: %v", err)
	}
	if recovered.BytesRead >= 200000 {
		t.Errorf("recovery read %d bytes; retained data was not reused", recovered.BytesRead)
	}
	if got := len(captureManifests(t, reopened.Workspace())); got != 1 {
		t.Errorf("%d run manifests after recovery, want exactly 1", got)
	}
	// The other workspace is still intact and untouched by all of this.
	if got := len(captureManifests(t, mustWorkspacePath(t, root, identityOfCapture(t, capB)))); got != 1 {
		t.Errorf("the unrelated workspace has %d manifests, want 1", got)
	}
}

// TestIsolatedWorkspacesKeyedByFullIdentity is acceptance 3: two identities that
// collide on the library's 20-byte key get distinct workspaces and distinct
// completion databases, and a legacy record for one does not block the other.
// Nothing is ever adopted by the truncated key.
func TestIsolatedWorkspacesKeyedByFullIdentity(t *testing.T) {
	var v2 infohash_v2.T
	for i := range v2 {
		v2[i] = byte(31 - i)
	}
	var v1 metainfo.Hash
	copy(v1[:], v2[:20])
	first := Identity{V2: v2, HasV2: true}
	second := Identity{V1: v1, HasV1: true}
	if first.StorageKey() != second.StorageKey() {
		t.Fatal("the synthetic identities do not share a storage key")
	}
	if first.JobID() == second.JobID() {
		t.Fatal("the synthetic identities share a job id; the test proves nothing")
	}
	root := t.TempDir()
	wsFirst := mustWorkspacePath(t, root, first)
	wsSecond := mustWorkspacePath(t, root, second)
	if wsFirst == wsSecond {
		t.Fatalf("both identities resolved to one workspace: %s", wsFirst)
	}
	if !strings.HasSuffix(wsFirst, "bt2-"+first.V2Hex()) || !strings.HasSuffix(wsSecond, "bt-"+second.V1Hex()) {
		t.Fatalf("workspace names are not derived from the full identity: %s / %s", wsFirst, wsSecond)
	}

	cfg := isolatedConfig(root)
	s1, err := openWorkspace(root, first, cfg)
	if err != nil {
		t.Fatalf("open first workspace: %v", err)
	}
	defer func() { _ = s1.Close() }()
	s2, err := openWorkspace(root, second, cfg)
	if err != nil {
		t.Fatalf("open second workspace: %v", err)
	}
	defer func() { _ = s2.Close() }()

	if s1.staging == s2.staging {
		t.Fatal("two workspaces share a directory")
	}
	dbFirst := filepath.Join(s1.staging, filepath.FromSlash(workspaceCompletionDir), ".torrent.db")
	dbSecond := filepath.Join(s2.staging, filepath.FromSlash(workspaceCompletionDir), ".torrent.db")
	if dbFirst == dbSecond {
		t.Fatal("two workspaces share a completion database path")
	}
	for _, db := range []string{dbFirst, dbSecond} {
		if _, serr := os.Stat(db); serr != nil {
			t.Errorf("completion database %s missing: %v", db, serr)
		}
	}

	// A legacy record for the truncated key's v1 form does not block the v2
	// identity: the conflict check compares full identities, not storage keys.
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(torrentStateDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := &jobRecord{
		Format: JobFormatVersion, JobID: second.JobID(), Backend: Backend,
		InfoHash: second.V1Hex(), Selection: []string{"fixture/a.bin"},
		Resume: resumeRef{MetainfoPath: metainfoRelFor(second), DataDir: ".", PartFiles: true},
	}
	legacySess := &Session{staging: root}
	if err := legacySess.saveJob(legacy); err != nil {
		t.Fatalf("write legacy record: %v", err)
	}
	if err := checkLegacyConflict(root, first); err != nil {
		t.Errorf("a legacy record for the colliding v1 identity blocked the v2 identity: %v", err)
	}
	if err := checkLegacyConflict(root, second); !errors.Is(err, ErrLegacyJobConflict) {
		t.Errorf("checkLegacyConflict(second) = %v, want ErrLegacyJobConflict", err)
	}
}

// TestIsolatedHybridResolvesToOneWorkspace is acceptance 4: every supported
// source form of one hybrid torrent lands in the same workspace and the same
// capture. A btmh-only magnet must resolve to the canonical v1 identity rather
// than creating a provisional bt2- job.
func TestIsolatedHybridResolvesToOneWorkspace(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(96<<10, 107)}
	mi := hybridMetaInfo(t, "hybridfixture", 32<<10, files)
	id := identityOfMetainfo(t, mi)
	if !id.HasV1 || !id.HasV2 {
		t.Fatalf("fixture is not a hybrid: %s", id)
	}
	seed := newMetaSeeder(t, "hybridfixture", files, mi, 0)
	root := t.TempDir()
	cfg := isolatedConfig(root, seed.peer())

	// 1. The local .torrent is the anchor: it needs no metadata exchange.
	job, err := OpenIsolated(context.Background(), cfg, seed.torrentFile(t))
	if err != nil {
		t.Fatalf("open from .torrent: %v", err)
	}
	if job.JobID() != id.JobID() {
		t.Fatalf("job id %s, want canonical %s", job.JobID(), id.JobID())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	first, err := job.Fetch(ctx, []string{"hybridfixture/a.bin"})
	if err != nil {
		t.Fatalf("fetch from .torrent: %v", err)
	}
	ws := job.Workspace()
	if err := job.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 2. btih, btih+btmh and btmh-only all resolve to that same workspace and
	// reconcile to the same capture.
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"btih", btihMagnet(mi)},
		{"dual", dualMagnet(t, mi)},
		{"btmh-only", btmhMagnet(t, mi)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			again, oerr := OpenIsolated(context.Background(), cfg, tc.source)
			if oerr != nil {
				t.Fatalf("open from %s magnet: %v", tc.name, oerr)
			}
			defer func() { _ = again.Close() }()
			if again.Workspace() != ws {
				t.Fatalf("%s magnet resolved to %s, want %s", tc.name, again.Workspace(), ws)
			}
			if again.JobID() != id.JobID() {
				t.Fatalf("%s magnet resolved to job %s, want %s", tc.name, again.JobID(), id.JobID())
			}
			cap, cerr := again.Fetch(ctx, []string{"hybridfixture/a.bin"})
			if cerr != nil {
				t.Fatalf("fetch from %s magnet: %v", tc.name, cerr)
			}
			if !cap.Reconciled {
				t.Errorf("%s magnet wrote a new capture instead of reconciling", tc.name)
			}
			if cap.CaptureKey != first.CaptureKey {
				t.Errorf("%s magnet capture key %s, want %s", tc.name, cap.CaptureKey, first.CaptureKey)
			}
			if got := len(captureManifests(t, ws)); got != 1 {
				t.Errorf("%s magnet produced %d run manifests, want 1", tc.name, got)
			}
		})
	}
}

// TestIsolatedLegacyConflictIsRefused is the conflict policy: an identity the
// top-level root already tracks in the legacy layout is refused, the legacy
// record is not touched, and no workspace is created for it.
func TestIsolatedLegacyConflictIsRefused(t *testing.T) {
	seed := newSeeder(t, "fixture", 16<<10, map[string][]byte{"a.bin": patterned(20000, 109)})
	root := t.TempDir()
	src := seed.torrentFile(t)
	srcIdentity, err := parseSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(torrentStateDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := &jobRecord{
		Format: JobFormatVersion, JobID: srcIdentity.id.JobID(), Backend: Backend,
		InfoHash: srcIdentity.id.V1Hex(), Selection: []string{"fixture/a.bin"},
		Resume: resumeRef{MetainfoPath: metainfoRelFor(srcIdentity.id), DataDir: ".", PartFiles: true},
	}
	if err := (&Session{staging: root}).saveJob(legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(torrentStateDir), srcIdentity.id.JobID()+".job.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = OpenIsolated(context.Background(), isolatedConfig(root, seed.peer()), src)
	if !errors.Is(err, ErrLegacyJobConflict) {
		t.Fatalf("OpenIsolated error = %v, want ErrLegacyJobConflict", err)
	}
	if _, serr := os.Stat(mustWorkspacePath(t, root, srcIdentity.id)); serr == nil {
		t.Error("a refused conflict still created a workspace")
	}
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(torrentStateDir), srcIdentity.id.JobID()+".job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the legacy record changed during a refused isolated open")
	}
}

// workspaceTree renders a workspace as an indented tree, for test logs and for
// anyone reading a failure without shell access to the temp dir.
func workspaceTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator))
		info, _ := d.Info()
		size := int64(0)
		if info != nil && !d.IsDir() {
			size = info.Size()
		}
		if d.IsDir() {
			b.WriteString(strings.Repeat("  ", depth+1) + filepath.Base(p) + "/\n")
		} else {
			b.WriteString(strings.Repeat("  ", depth+1) + filepath.Base(p) + "  (" + strconv.FormatInt(size, 10) + " bytes)\n")
		}
		return nil
	})
	return b.String()
}

// TestIsolatedCaptureIsReadableByExistingReaders: the workspace is a
// self-contained provenance root, so the readers that already exist find it when
// pointed at it — no new index and no mirrored manifests.
func TestIsolatedCaptureIsReadableByExistingReaders(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(30000, 113)}
	seed := newSeeder(t, "fixture", 16<<10, files)
	root := t.TempDir()

	cap, err := FetchIsolated(context.Background(), isolatedConfig(root, seed.peer()), seed.magnet(), []string{"fixture/a.bin"})
	if err != nil {
		t.Fatalf("FetchIsolated: %v", err)
	}
	if !strings.HasPrefix(cap.ManifestPath, ".provenance/runs/") {
		t.Errorf("manifest path %q is not workspace-relative", cap.ManifestPath)
	}
	if !filepath.IsAbs(cap.StagingDir) {
		t.Errorf("capture staging dir %q is not absolute", cap.StagingDir)
	}
	// Log the real tree: it is the layout the docs describe, and a failure
	// elsewhere is easier to read next to it.
	t.Logf("workspace tree for %s:\n%s", cap.JobID, workspaceTree(t, cap.StagingDir))

	// The status reader (same decoder as the writer) sees exactly one job.
	status, err := ReadJobStatus(cap.StagingDir, cap.JobID)
	if err != nil {
		t.Fatalf("ReadJobStatus on workspace: %v", err)
	}
	if status.JobID != cap.JobID || status.RecordedState.Capture != CaptureCommitted {
		t.Errorf("status = %+v, want the committed isolated job", status)
	}
	// And the capture namespace holds exactly one run plus its item.
	if got := len(captureManifests(t, cap.StagingDir)); got != 1 {
		t.Errorf("%d runs in the workspace capture namespace, want 1", got)
	}
	entries, err := os.ReadDir(filepath.Join(cap.StagingDir, ".provenance", "items"))
	if err != nil || len(entries) != 1 {
		t.Errorf("items = %v (%v), want exactly 1", entries, err)
	}
	// The top-level root has no capture of its own.
	if got := len(captureManifests(t, root)); got != 0 {
		t.Errorf("the top-level root has %d runs, want none", got)
	}
}
