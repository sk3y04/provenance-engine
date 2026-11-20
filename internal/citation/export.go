package citation

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

// BibTeX renders one entity as a @misc record. The pinned provenance URI is
// carried in the note, so a bibliography and an audit trail stay separately
// usable without putting archive internals in the citation key.
func BibTeX(collection, revisionID string, ent *archive.Entity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@misc{%s,\n", bibKey(ent))
	if ent.Title != "" {
		fmt.Fprintf(&b, "  title = {%s},\n", bibEscape(ent.Title))
	}
	if ent.Author != "" {
		fmt.Fprintf(&b, "  author = {%s},\n", bibEscape(ent.Author))
	}
	if year := entYear(ent); year != "" {
		fmt.Fprintf(&b, "  year = {%s},\n", year)
	}
	if ent.URL != "" {
		fmt.Fprintf(&b, "  url = {%s},\n", bibEscape(ent.URL))
	}
	if doi := ent.Identifiers["doi"]; doi != "" {
		fmt.Fprintf(&b, "  doi = {%s},\n", bibEscape(doi))
	}
	if arxiv := ent.Identifiers["arxiv"]; arxiv != "" {
		fmt.Fprintf(&b, "  eprint = {%s},\n  archiveprefix = {arXiv},\n", bibEscape(arxiv))
	}
	pinned := Format(collection, revisionID, ent.ExternalID)
	fmt.Fprintf(&b, "  note = {Pinned evidence: %s},\n", pinned)
	b.WriteString("}\n")
	return b.String()
}

// CSLJSON renders one entity as a CSL-JSON item. It includes the pinned
// provenance URI and the archived artifact digest beside the standard fields.
func CSLJSON(collection, revisionID string, ent *archive.Entity) map[string]any {
	item := map[string]any{
		"id":         ent.ExternalID,
		"type":       cslType(ent.Kind),
		"provenance": Format(collection, revisionID, ent.ExternalID),
	}
	if ent.Title != "" {
		item["title"] = ent.Title
	}
	if ent.Author != "" {
		item["author"] = []map[string]string{{"literal": ent.Author}}
	}
	if ent.URL != "" {
		item["URL"] = ent.URL
	}
	if year, month, day := entDate(ent); year > 0 {
		item["issued"] = map[string]any{"date-parts": [][]int{{year, month, day}}}
	}
	if doi := ent.Identifiers["doi"]; doi != "" {
		item["DOI"] = doi
	}
	if arxiv := ent.Identifiers["arxiv"]; arxiv != "" {
		item["arXiv"] = arxiv
	}
	if len(ent.Artifacts) > 0 {
		item["archive_sha256"] = ent.Artifacts[0]
	}
	return item
}

func bibKey(ent *archive.Entity) string {
	key := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == ':', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, ent.ExternalID)
	if strings.Trim(key, "_") == "" {
		return "provenance"
	}
	return key
}

func bibEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `{`, `\{`)
	s = strings.ReplaceAll(s, `}`, `\}`)
	return strings.Join(strings.Fields(s), " ")
}

func entYear(ent *archive.Entity) string {
	year, _, _ := entDate(ent)
	if year <= 0 {
		return ""
	}
	return strconv.Itoa(year)
}

func entDate(ent *archive.Entity) (int, int, int) {
	t := ent.CapturedAt
	if ent.PublishedAt != nil && !ent.PublishedAt.IsZero() {
		t = *ent.PublishedAt
	}
	return dateParts(t)
}

func dateParts(t time.Time) (int, int, int) {
	if t.IsZero() {
		return 0, 0, 0
	}
	return t.Year(), int(t.Month()), t.Day()
}

func cslType(kind string) string {
	switch strings.ToLower(kind) {
	case "pdf", "paper", "article", "scholarly_work":
		return "article"
	case "video", "audio":
		return "motion_picture"
	case "image", "photo":
		return "graphic"
	case "post", "text", "docs_site", "docs", "page":
		return "webpage"
	default:
		return "document"
	}
}
