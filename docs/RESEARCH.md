# Research evidence

Status: PR-4A (page-pinned PDF citations) is shipped. PR-4B (scholarly identity
and DOI/arXiv enrichment) and PR-4C (versioned documentation evidence) are
staged follow-ups; this page records the boundary rather than implying they
exist.

## What a PDF import stores

`provenance archive import-pdf` stores the original PDF once in the
content-addressed blob store. It is immutable: re-indexing never rewrites it.

Each indexed passage is an `archive.Document` with a Provenance-owned
`archive.Locator`:

| Field | Meaning |
|-------|---------|
| `artifact_sha256` | SHA-256 of the archived PDF blob |
| `page_index` | Zero-based **physical** page index |
| `page_label` | Printed page label when the parser exposes one (`iv`, `12`, `A-3`) |
| `rectangles` | Optional normalized rectangles |
| `coordinate_origin` | `top-left` for the rectangles above |

A citation URI stays entity-level:

```
provenance://<collection>@<revision>#<entity>
```

The locator travels beside the reference (in JSON/plain output), not inside the
URI. A re-index may change passage/chunk IDs; it does not change the permanent
citation URI. `vault cite --page N` selects the physical page and reports the
printed label separately.

### Rectangles

Rectangles are fractions of the page width and height in `[0,1]`, with the
top-left corner as origin. A parser bbox in page points is normalized by the
page size. If a bbox cannot be normalized it is omitted rather than stored as a
misleading number.

### Docling

Docling is optional and explicit:

```bash
provenance archive import-pdf paper.pdf --collection papers \
  --docling /usr/local/bin/docling --docling-version 2.x
```

The process is bounded by `--docling-timeout` (default five minutes) and its
output is capped. If Docling is not configured, the built-in text extractor
still records one document per non-empty physical page with the same locator
shape (no rectangles and no printed label).

When Docling is used, the **exact JSON** is stored as a separate analysis
artifact and recorded in the revision's `derivations` list with:

- `kind: docling-json`
- `artifact_sha256` (parent PDF)
- `blob_sha256` (the JSON itself)
- `parser`, `parser_version`, `config_hash`
- `created_at`

The parser is deliberately tolerant about the surrounding Docling schema and
reads the fields it needs (`page_no`, `prov`, `bbox`, page size/label). Keeping
the raw JSON means a future schema adapter can re-index from evidence without
running Docling again.

### Offline resolution

`archive.ResolveLocator(vaultRoot, revisionID, locator)` reads the revision
manifest and the blob store only. It rejects a locator whose artifact is not part
of that revision, a negative page index, or a non-SHA-256 artifact address. The
PR-3 `archive verify` therefore also checks the PDF and the stored Docling JSON
as ordinary referenced blobs.

The acceptance fixture uses printed labels (`Cover`, `iv`, `1`) that differ from
the physical zero-based indices (`0`, `1`, `2`), re-indexes with changed passage
IDs, and resolves physical page `1` to the same archived PDF as `iv` offline.

## What is deliberately not in this slice

### 4B — scholarly identity and enrichment

A work/version/capture/derivation model, DOI and arXiv intake, Crossref
correction/retraction observations, preprint/publication linking, and
BibTeX/CSL JSON export are not implemented. The PDF importer currently produces
an ordinary `archive.Entity` with `kind: pdf`; no work identity is inferred from
the file name or content. No network lookup is performed.

When this lands, it must keep the captured PDF immutable, store provider
metadata with retrieval time, and never rewrite a PDF because a retraction or
correction was observed. GROBID should remain a derivation producer rather than
a replacement for the Docling/raw evidence.

### 4C — versioned documentation

Documentation crawling by release/commit, normalized text/heading comparison,
navigation-only change detection, evidence notes, table/figure locators, and
JSONL export are not implemented. `/latest/` is not treated as a stable release.

## Safety and cost limits

- A Docling run has a bounded process lifetime; the JSON output is capped at
  64 MiB.
- The PDF is read only for the built-in fallback; when Docling is configured it
  is the sole text source, so a PDF the fallback cannot parse can still be
  indexed.
- The original source path is recorded as entity `URL` evidence; the archived
  bytes are addressed by SHA-256 and can be resolved without that path.
- No OCR, embeddings, vector index or inference is performed by the engine.
