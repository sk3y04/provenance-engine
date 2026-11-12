package archive

import "testing"

// TestDiffRevisionsClassifiesAddedRemovedChanged: entities present only in
// the new revision are added, only in the old one removed, and field-level
// changes are reported with old/new values.
func TestDiffRevisionsClassifiesAddedRemovedChanged(t *testing.T) {
	old := &Revision{ID: "a", Entities: []Entity{
		{ExternalID: "e1", Title: "v1"},
		{ExternalID: "e2", Title: "gone"},
	}}
	neu := &Revision{ID: "b", Entities: []Entity{
		{ExternalID: "e1", Title: "v2"},
		{ExternalID: "e3", Title: "fresh"},
	}}

	d := DiffRevisions(old, neu)

	if got := ids(d.Added); !sameSet(got, map[string]bool{"e3": true}) {
		t.Fatalf("added = %v, want {e3}", got)
	}
	if got := ids(d.Removed); !sameSet(got, map[string]bool{"e2": true}) {
		t.Fatalf("removed = %v, want {e2}", got)
	}
	if len(d.Changed) != 1 || d.Changed[0].ExternalID != "e1" {
		t.Fatalf("changed = %+v, want exactly e1", d.Changed)
	}
	found := false
	for _, c := range d.Changed[0].Changes {
		if c.Field == "title" && c.OldVal == "v1" && c.NewVal == "v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("title change not reported: %+v", d.Changed[0].Changes)
	}
	if d.Unchanged != 0 {
		t.Fatalf("unchanged = %d, want 0", d.Unchanged)
	}
}

// TestDiffRevisionsIdenticalIsUnchanged: equal revisions produce no diffs.
func TestDiffRevisionsIdenticalIsUnchanged(t *testing.T) {
	mk := func() *Revision {
		return &Revision{ID: "r", Entities: []Entity{
			{ExternalID: "e1", Title: "same"},
			{ExternalID: "e2", Title: "also same"},
		}}
	}
	d := DiffRevisions(mk(), mk())
	if d.Unchanged != 2 || len(d.Added) != 0 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("identical revisions must be fully unchanged: %+v", d)
	}
}

func ids(es []EntityDiff) map[string]bool {
	m := make(map[string]bool, len(es))
	for _, e := range es {
		m[e.ExternalID] = true
	}
	return m
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
