package catalog

import (
	"fmt"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

// makeRevision writes a collection revision captured at at and returns its id.
func makeRevision(t *testing.T, vaultRoot, col string, at time.Time) string {
	t.Helper()
	id := fmt.Sprintf("rev-%s-%d", col, at.UnixNano())
	rev := &archive.Revision{
		ID:         id,
		CapturedAt: at,
		Source:     archive.Source{Kind: archive.SourceCollection, Reference: col},
		Entities:   []archive.Entity{{ExternalID: col + "-entity", CapturedAt: at}},
	}
	if err := archive.WriteRevision(vaultRoot, rev); err != nil {
		t.Fatal(err)
	}
	return id
}

func revExists(vaultRoot, id string) bool {
	_, err := archive.ReadRevision(vaultRoot, id)
	return err == nil
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestPruneRevisionsKeepsNewestAndIgnoresOtherCollections: pruning keeps the
// newest revisions of the target collection only; other collections and the
// kept revisions must be untouched.
func TestPruneRevisionsKeepsNewestAndIgnoresOtherCollections(t *testing.T) {
	vault := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var c1 []string
	for i := 0; i < 5; i++ {
		c1 = append(c1, makeRevision(t, vault, "c1", base.Add(time.Duration(i)*time.Hour)))
	}
	c2a := makeRevision(t, vault, "c2", base)
	c2b := makeRevision(t, vault, "c2", base.Add(time.Hour))

	removed, err := PruneRevisions(vault, "c1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(idSet(removed), map[string]bool{c1[0]: true, c1[1]: true, c1[2]: true}) {
		t.Fatalf("removed = %v, want the three oldest c1 revisions", removed)
	}
	for _, id := range []string{c1[0], c1[1], c1[2]} {
		if revExists(vault, id) {
			t.Fatalf("pruned revision %s still on disk", id)
		}
	}
	if !revExists(vault, c1[3]) || !revExists(vault, c1[4]) {
		t.Fatal("newest c1 revisions must survive pruning")
	}
	if !revExists(vault, c2a) || !revExists(vault, c2b) {
		t.Fatal("other collections must not be pruned")
	}
}

// TestPruneRevisionsNoOpWithinKeep: at or under the keep threshold nothing is
// removed.
func TestPruneRevisionsNoOpWithinKeep(t *testing.T) {
	vault := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := makeRevision(t, vault, "c1", base)
	b := makeRevision(t, vault, "c1", base.Add(time.Hour))

	for _, keep := range []int{5, 2} {
		removed, err := PruneRevisions(vault, "c1", keep)
		if err != nil || len(removed) != 0 {
			t.Fatalf("prune keep=%d: removed=%v err=%v", keep, removed, err)
		}
	}
	if !revExists(vault, a) || !revExists(vault, b) {
		t.Fatal("no revision may be removed within the keep threshold")
	}
}
