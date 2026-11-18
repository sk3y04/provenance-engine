package bittorrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
)

// Magnet ingestion: v2-only (btmh) and hybrid (btih, btmh, or both).

func btmhMagnet(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	h := infohash_v2.HashBytes(mi.InfoBytes)
	return "magnet:?xt=urn:btmh:1220" + h.HexString()
}

func btihMagnet(mi *metainfo.MetaInfo) string {
	return "magnet:?xt=urn:btih:" + metainfo.HashBytes(mi.InfoBytes).HexString()
}

func dualMagnet(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	h := infohash_v2.HashBytes(mi.InfoBytes)
	return "magnet:?xt=urn:btih:" + metainfo.HashBytes(mi.InfoBytes).HexString() +
		"&xt=urn:btmh:1220" + h.HexString()
}

// TestV2OnlyMagnet: inspection works (identity and layout from metadata alone),
// payload is never requested, and a fetch fails with the precise blocker instead
// of a timeout. Piece layers are only distributed in .torrent files.
func TestV2OnlyMagnet(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(96<<10, 'a')} // 3 pieces
	mi := v2MetaInfo(t, "fixture", 32*1024, files)
	sel := newMetaSeeder(t, "fixture", files, mi, 0)
	magnet := btmhMagnet(t, mi)

	staging := t.TempDir()
	before := snapshotTree(t, staging)

	info, err := Inspect(context.Background(), magnet, InspectConfig{
		Peers: []string{sel.peer()}, DisablePeerDiscovery: true, MetadataTimeout: 30e9,
	})
	if err != nil {
		t.Fatalf("inspect v2-only magnet: %v", err)
	}
	v2h := infohash_v2.HashBytes(mi.InfoBytes)
	if info.InfoHash != "" || info.InfoHashV2 != v2h.HexString() {
		t.Fatalf("identity = %q / %q", info.InfoHash, info.InfoHashV2)
	}
	if len(info.Files) != 1 || info.Files[0].Length != 96<<10 {
		t.Fatalf("files = %+v", info.Files)
	}
	if stats := sel.torrent.Stats(); stats.BytesWrittenData.Int64() != 0 {
		t.Errorf("seeder wrote %d payload bytes to an inspector, want 0", stats.BytesWrittenData.Int64())
	}
	if diff := diffSnapshots(before, snapshotTree(t, staging)); diff != "" {
		t.Errorf("inspection touched staging:\n%s", diff)
	}

	// Fetching it cannot be verified: fail before a job is created.
	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	defer cancel()
	_, err = sess.Fetch(ctx, magnet, []string{"fixture/a.bin"})
	if err == nil {
		t.Fatal("v2-only magnet fetch reported success")
	}
	if !errors.Is(err, ErrUnsupportedIdentity) {
		t.Fatalf("error = %v, want ErrUnsupportedIdentity naming the blocker", err)
	}
	if !strings.Contains(err.Error(), "piece layers") {
		t.Errorf("error does not name the missing piece layers: %v", err)
	}
	if n := manifestCount(t, staging); n != 0 {
		t.Errorf("run manifests = %d, want 0", n)
	}
	entries, _ := os.ReadDir(filepath.Join(staging, ".provenance", "torrent"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".job.json") {
			t.Errorf("a job record was created for an unverifiable torrent: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(staging, "fixture")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("payload directory exists: %v", err)
	}
}

// TestHybridMagnetEveryForm: one canonical job and one capture, whichever source
// form names it. A magnet-originated hybrid has no piece layers; its v1 piece
// hashes are what allow verification and restart.
func TestHybridMagnetEveryForm(t *testing.T) {
	a := patterned(64<<10, 'a') // 2 pieces at 32 KiB
	b := patterned(32<<10, 'b')
	files := map[string][]byte{"a.bin": a, "b.bin": b}
	mi := hybridMetaInfo(t, "fixture", 32*1024, files)
	wantV1 := metainfo.HashBytes(mi.InfoBytes).HexString()
	v2h := infohash_v2.HashBytes(mi.InfoBytes)
	wantV2 := v2h.HexString()
	sel := newMetaSeeder(t, "fixture", files, mi, 0)

	staging := t.TempDir()
	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	first, err := sess.Fetch(ctx, btihMagnet(mi), []string{"fixture/a.bin"})
	cancel()
	if err != nil {
		t.Fatalf("hybrid fetch by btih magnet: %v", err)
	}
	if first.JobID != "bt-"+wantV1 || first.InfoHash != wantV1 || first.InfoHashV2 != wantV2 {
		t.Fatalf("first identity = %q %q %q", first.JobID, first.InfoHash, first.InfoHashV2)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Fatalf("run manifests = %d, want 1", n)
	}
	// A hybrid's metainfo does not need piece layers for resume, but the record
	// must have gained the v2 identity the magnet did not carry.
	persisted, err := metainfo.LoadFromFile(filepath.Join(staging, ".provenance", "torrent", wantV1+".torrent"))
	if err != nil {
		t.Fatalf("persisted metainfo: %v", err)
	}
	if len(persisted.PieceLayers) != 0 {
		t.Logf("note: persisted metainfo carries %d piece layers", len(persisted.PieceLayers))
	}
	if job := jobRecordFor(t, staging, "bt-"+wantV1); job.InfoHashV2 != wantV2 {
		t.Errorf("job did not record the resolved v2 identity: %q", job.InfoHashV2)
	}

	// Reopen through each other form. One of them runs with no peers at all, to
	// show restart does not need to fetch the info dictionary again.
	cases := []struct {
		name  string
		src   string
		peers []string
	}{
		{"btmh-only", btmhMagnet(t, mi), []string{sel.peer()}},
		{"dual", dualMagnet(t, mi), []string{sel.peer()}},
		{"local .torrent", writeTorrentFile(t, mi), nil},
	}
	for _, tc := range cases {
		s := newTestSession(t, staging, tc.peers...)
		c, cancel := waitCtx(t)
		cap, err := s.Fetch(c, tc.src, []string{"fixture/a.bin"})
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cap.JobID != "bt-"+wantV1 || cap.CaptureKey != first.CaptureKey {
			t.Errorf("%s resolved to %q / %q, want the canonical job and key", tc.name, cap.JobID, cap.CaptureKey)
		}
		if !cap.Reconciled {
			t.Errorf("%s did not reconcile the existing capture", tc.name)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("%s close: %v", tc.name, err)
		}
	}
	if n := manifestCount(t, staging); n != 1 {
		t.Errorf("run manifests after every form = %d, want 1", n)
	}
	entries, _ := os.ReadDir(filepath.Join(staging, ".provenance", "torrent"))
	jobs := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".job.json") {
			jobs++
		}
	}
	if jobs != 1 {
		t.Errorf("job records = %d, want 1 canonical job", jobs)
	}
}

// TestHybridDualMagnetWrongHashIsNotAdopted: a dual magnet whose v2 hash
// contradicts the metadata must not adopt the existing job or publish anything.
func TestHybridDualMagnetWrongHashIsNotAdopted(t *testing.T) {
	files := map[string][]byte{"a.bin": patterned(64<<10, 'a'), "b.bin": patterned(32<<10, 'b')}
	mi := hybridMetaInfo(t, "fixture", 32*1024, files)
	sel := newMetaSeeder(t, "fixture", files, mi, 0)

	staging := t.TempDir()
	sess := newTestSession(t, staging, sel.peer())
	ctx, cancel := waitCtx(t)
	if _, err := sess.Fetch(ctx, btihMagnet(mi), []string{"fixture/a.bin"}); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	cancel()
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := runManifestDigests(t, staging)
	jobBefore := jobRecordFor(t, staging, "bt-"+metainfo.HashBytes(mi.InfoBytes).HexString())

	// (a) Wrong v1 hash, right v2 hash: the library keys the torrent by v2, so
	// metadata arrives and the adapter rejects the contradictory identity
	// explicitly.
	goodV1 := metainfo.HashBytes(mi.InfoBytes).HexString()
	v2 := infohash_v2.HashBytes(mi.InfoBytes)
	wrongV1 := flipHex(goodV1)
	dualWrongV1 := "magnet:?xt=urn:btih:" + wrongV1 + "&xt=urn:btmh:1220" + v2.HexString()
	cfgA := testConfig(t, staging, sel.peer())
	cfgA.MetadataTimeout = 2 * time.Second
	sess2, err := New(cfgA)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	ctx2, cancel2 := waitCtx(t)
	_, err = sess2.Fetch(ctx2, dualWrongV1, []string{"fixture/a.bin"})
	cancel2()
	if err == nil {
		t.Fatal("a dual magnet with a wrong v1 hash was accepted")
	}
	if !errors.Is(err, ErrMetadataTimeout) || !strings.Contains(err.Error(), "both supplied infohashes") {
		t.Errorf("wrong-v1 error = %v, want a metadata failure naming the contradictory pair", err)
	}
	if err := sess2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// (b) Right v1, wrong v2: the pinned library keys the torrent by the v2 hash,
	// so no metadata is ever exchanged. The contradiction therefore surfaces as a
	// bounded metadata timeout. What matters is that nothing is adopted and
	// nothing is published either way.
	wrongV2 := flipHex(v2.HexString())
	dualWrongV2 := "magnet:?xt=urn:btih:" + goodV1 + "&xt=urn:btmh:1220" + wrongV2
	cfg := testConfig(t, staging, sel.peer())
	cfg.MetadataTimeout = 2 * time.Second
	sess3, err := New(cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	ctx3, cancel3 := waitCtx(t)
	_, err = sess3.Fetch(ctx3, dualWrongV2, []string{"fixture/a.bin"})
	cancel3()
	if err == nil {
		t.Fatal("a dual magnet with a wrong v2 hash was accepted")
	}
	if !errors.Is(err, ErrMetadataTimeout) || !strings.Contains(err.Error(), "both supplied infohashes") {
		t.Errorf("wrong-v2 error = %v, want a metadata failure naming the contradictory pair", err)
	}
	if err := sess3.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	after := runManifestDigests(t, staging)
	if len(after) != len(before) {
		t.Errorf("run manifests = %d, want %d", len(after), len(before))
	}
	for p, h := range before {
		if after[p] != h {
			t.Errorf("manifest %s changed", p)
		}
	}
	jobAfter := jobRecordFor(t, staging, jobBefore.JobID)
	if jobAfter.InfoHashV2 != jobBefore.InfoHashV2 {
		t.Errorf("job identity was rewritten: %q -> %q", jobBefore.InfoHashV2, jobAfter.InfoHashV2)
	}
}

// flipHex returns the same-length hex string with its first digit changed, so a
// wrong hash stays a well-formed identity.
func flipHex(s string) string {
	if s[0] == '0' {
		return "1" + s[1:]
	}
	return "0" + s[1:]
}

// TestIdentityConflictOnSharedStorageKey: two synthetic identities that differ in
// full but collide on the library's 20-byte key cannot be distinguished by that
// key, so the adapter must refuse the second job rather than share state.
func TestIdentityConflictOnSharedStorageKey(t *testing.T) {
	// A v2 hash whose first 20 bytes are a chosen v1 hash, and a v1 identity
	// with exactly that hash: distinct identities, identical StorageKey.
	v2 := infohash_v2.T{}
	for i := range v2 {
		v2[i] = byte(i)
	}
	var v1 metainfo.Hash
	copy(v1[:], v2[:20])

	first := Identity{V2: v2, HasV2: true}
	second := Identity{V1: v1, HasV1: true}
	if first.StorageKey() != second.StorageKey() {
		t.Fatalf("synthetic identities do not share a storage key")
	}
	if first.JobID() == second.JobID() {
		t.Fatalf("synthetic identities share a job id; the test proves nothing")
	}

	staging := t.TempDir()
	s := &Session{staging: staging}
	if err := os.MkdirAll(s.statePath(""), 0o755); err != nil {
		t.Fatal(err)
	}
	// Record the v1 job on disk, then ask whether the colliding v2 identity may
	// start a job in the same root.
	job := &jobRecord{
		Format: JobFormatVersion, JobID: second.JobID(), Backend: Backend,
		InfoHash: second.V1Hex(), Selection: []string{"fixture/a.bin"},
		Resume: resumeRef{MetainfoPath: metainfoRelFor(second), DataDir: ".", PartFiles: true},
	}
	if err := s.saveJob(job); err != nil {
		t.Fatalf("save job: %v", err)
	}
	err := s.checkIdentityConflict(first)
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("conflict = %v, want ErrIdentityConflict", err)
	}
	if !strings.Contains(err.Error(), "piece-completion key") {
		t.Errorf("error does not name the shared key: %v", err)
	}
	// The check is a decision, not a side effect: no payload directory, no
	// metainfo, and the existing record untouched.
	entries, _ := os.ReadDir(s.statePath(""))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".torrent") {
			t.Errorf("conflict check created metainfo %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(staging, "fixture")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("conflict check created a payload directory")
	}
	reloaded, err := readJob(s.jobPath(second.JobID()))
	if err != nil || reloaded.InfoHash != second.V1Hex() {
		t.Errorf("existing record changed: %+v, %v", reloaded, err)
	}
}
