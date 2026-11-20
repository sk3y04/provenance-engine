package citation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

func TestBibTeXAndCSLCarryPinnedEvidence(t *testing.T) {
	pub := time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC)
	ent := &archive.Entity{
		ExternalID:  "paper-1",
		Title:       "A {curly} title",
		Author:      "Ada Lovelace",
		URL:         "https://example.test/paper",
		PublishedAt: &pub,
		Kind:        "scholarly_work",
		Artifacts:   []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Identifiers: map[string]string{"doi": "10.1000/xyz", "arxiv": "2401.00001"},
	}
	bib := BibTeX("papers", "rev1", ent)
	for _, want := range []string{
		"@misc{paper-1,", "title = {A \\{curly\\} title}", "doi = {10.1000/xyz}",
		"eprint = {2401.00001}", "year = {2024}",
		"note = {Pinned evidence: provenance://papers@rev1#paper-1}",
	} {
		if !strings.Contains(bib, want) {
			t.Fatalf("BibTeX missing %q:\n%s", want, bib)
		}
	}

	item := CSLJSON("papers", "rev1", ent)
	if item["DOI"] != "10.1000/xyz" || item["arXiv"] != "2401.00001" {
		t.Fatalf("CSL identifiers = %+v", item)
	}
	if item["provenance"] != "provenance://papers@rev1#paper-1" {
		t.Fatalf("CSL provenance = %v", item["provenance"])
	}
	if item["archive_sha256"] != ent.Artifacts[0] {
		t.Fatalf("CSL archive digest = %v", item["archive_sha256"])
	}
	data, err := json.Marshal(item)
	if err != nil || !strings.Contains(string(data), `"article"`) {
		t.Fatalf("CSL JSON = %s err=%v", data, err)
	}
}
