package importers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

// PDFOptions configures optional structured extraction. The zero value keeps
// the built-in, dependency-free text extractor. Docling is only run when a path
// or runner is explicitly configured.
type PDFOptions struct {
	// DoclingPath is a local docling executable. Empty disables Docling.
	DoclingPath string
	// DoclingTimeout bounds the Docling process. Zero uses DefaultDoclingTimeout.
	DoclingTimeout time.Duration
	// DoclingVersion is recorded as the parser version evidence.
	DoclingVersion string
	// DoclingRunner is a test seam; production uses DoclingPath.
	DoclingRunner func(ctx context.Context, pdfPath string) ([]byte, error)
}

// DefaultDoclingTimeout bounds one local Docling run.
const DefaultDoclingTimeout = 5 * time.Minute

const maxDoclingJSON = 64 << 20

type doclingPage struct {
	Label  string
	Width  float64
	Height float64
}

type doclingBlock struct {
	Text      string
	PageIndex int
	PageLabel string
	Rect      *archive.Rect
}

// ImportPDF keeps the historical API: parse with the built-in text extractor,
// store the immutable PDF, and index one document per non-empty page.
func ImportPDF(ctx context.Context, vaultRoot, pdfPath, collectionName string) (*archive.Revision, error) {
	return ImportPDFWithOptions(ctx, vaultRoot, pdfPath, collectionName, PDFOptions{})
}

// ImportPDFWithOptions extracts page-pinned passages. The PDF itself is stored
// once and never rewritten; any structured analysis is stored as a separate
// content-addressed derivation pointing at the PDF's SHA-256.
func ImportPDFWithOptions(ctx context.Context, vaultRoot, pdfPath, collectionName string, opts PDFOptions) (*archive.Revision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bs := blobstore.New(vaultRoot)

	if _, err := os.Stat(pdfPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("pdf not found: %s", pdfPath)
	}

	rev := newRevision(pdfPath, "provenance-import-pdf", "1.1")

	blobHash, byteSize, err := storeFile(bs, pdfPath)
	if err != nil {
		return nil, fmt.Errorf("store pdf blob: %w", err)
	}

	rev.Artifacts = append(rev.Artifacts, archive.Artifact{
		Sha256:   blobHash,
		Path:     filepath.Base(pdfPath),
		Size:     byteSize,
		Kind:     archive.ArtifactBinary,
		MimeType: "application/pdf",
	})

	entity := archive.Entity{
		ExternalID: filepath.Base(pdfPath),
		URL:        pdfPath,
		Title:      filepath.Base(pdfPath),
		Kind:       "pdf",
		Extractor:  "pdf-importer",
		CapturedAt: rev.CapturedAt,
		Artifacts:  []string{blobHash},
	}

	var docs []archive.Document
	pages := 0
	parser := "builtin-pdf"
	if opts.DoclingPath != "" || opts.DoclingRunner != nil {
		raw, derr := runDoclingJSON(ctx, pdfPath, opts)
		if derr != nil {
			return nil, derr
		}
		blocks, pageLabels, derr := parseDoclingJSON(raw)
		if derr != nil {
			return nil, derr
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("docling analysis contained no page text")
		}
		docs, derr = doclingDocuments(filepath.Base(pdfPath), blobHash, blocks, pageLabels)
		if derr != nil {
			return nil, derr
		}
		for _, b := range blocks {
			if b.PageIndex+1 > pages {
				pages = b.PageIndex + 1
			}
		}
		parser = "docling"
		// The exact parser output is evidence, stored separately from the PDF.
		jsonHash, jsonSize, serr := storeDerivation(bs, raw, filepath.Base(pdfPath)+".docling.json")
		if serr != nil {
			return nil, serr
		}
		rev.Artifacts = append(rev.Artifacts, archive.Artifact{
			Sha256:   jsonHash,
			Path:     filepath.Base(pdfPath) + ".docling.json",
			Size:     jsonSize,
			MimeType: "application/json",
			Kind:     archive.ArtifactAnalysis,
		})
		rev.Derivations = append(rev.Derivations, archive.Derivation{
			Kind:           "docling-json",
			ArtifactSha256: blobHash,
			BlobSha256:     jsonHash,
			Parser:         "docling",
			ParserVersion:  valueOr(opts.DoclingVersion, "docling"),
			ConfigHash:     hashString("docling --to json"),
			CreatedAt:      time.Now().UTC(),
		})
	} else {
		var derr error
		docs, pages, derr = pdfDocuments(ctx, pdfPath, blobHash)
		if derr != nil {
			return nil, derr
		}
	}
	entity.Documents = docs
	for _, doc := range docs {
		entity.Relations = append(entity.Relations, archive.Relation{
			From: entity.ExternalID,
			To:   doc.ExternalID,
			Kind: archive.RelContains,
		})
	}
	meta, _ := json.Marshal(struct {
		Pages  int    `json:"pages"`
		Parser string `json:"parser"`
	}{Pages: pages, Parser: parser})
	entity.RawMetadata = meta
	rev.Entities = []archive.Entity{entity}

	if err := persistRevision(vaultRoot, rev); err != nil {
		return nil, fmt.Errorf("write revision: %w", err)
	}

	return rev, nil
}

func pdfDocuments(ctx context.Context, pdfPath, artifactHash string) ([]archive.Document, int, error) {
	pdfFile, reader, err := pdf.Open(pdfPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open pdf: %w", err)
	}
	defer func() { _ = pdfFile.Close() }()

	totalPage := reader.NumPage()
	docs := make([]archive.Document, 0, totalPage)
	for pageNum := 1; pageNum <= totalPage; pageNum++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		page := reader.Page(pageNum)
		text, err := page.GetPlainText(nil)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		pageIndex := pageNum - 1
		docs = append(docs, archive.Document{
			ExternalID: fmt.Sprintf("%s:p%d", filepath.Base(pdfPath), pageIndex),
			Content:    text,
			Format:     archive.DocPlain,
			Locator: &archive.Locator{
				ArtifactSha256:   artifactHash,
				PageIndex:        pageIndex,
				CoordinateOrigin: "top-left",
			},
		})
	}
	return docs, totalPage, nil
}

func doclingDocuments(pdfName, artifactHash string, blocks []doclingBlock, labels map[int]string) ([]archive.Document, error) {
	docs := make([]archive.Document, 0, len(blocks))
	for i, b := range blocks {
		if strings.TrimSpace(b.Text) == "" || b.PageIndex < 0 {
			continue
		}
		label := b.PageLabel
		if label == "" {
			label = labels[b.PageIndex+1]
		}
		loc := &archive.Locator{
			ArtifactSha256:   artifactHash,
			PageIndex:        b.PageIndex,
			PageLabel:        label,
			CoordinateOrigin: "top-left",
		}
		if b.Rect != nil {
			loc.Rectangles = []archive.Rect{*b.Rect}
		}
		docs = append(docs, archive.Document{
			ExternalID: fmt.Sprintf("%s:p%d:b%d", pdfName, b.PageIndex, i),
			Content:    strings.TrimSpace(b.Text),
			Format:     archive.DocPlain,
			Locator:    loc,
		})
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("docling analysis produced no usable passages")
	}
	return docs, nil
}

func runDoclingJSON(ctx context.Context, pdfPath string, opts PDFOptions) ([]byte, error) {
	if opts.DoclingRunner != nil {
		return opts.DoclingRunner(ctx, pdfPath)
	}
	timeout := opts.DoclingTimeout
	if timeout <= 0 {
		timeout = DefaultDoclingTimeout
	}
	dir, err := os.MkdirTemp("", "provenance-docling-*")
	if err != nil {
		return nil, fmt.Errorf("create docling output dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, opts.DoclingPath, "--to", "json", "--output", dir, pdfPath)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("docling: %w", runCtx.Err())
		}
		return nil, fmt.Errorf("docling failed: %w: %s", err, trimText(stderr.String(), 300))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read docling output: %w", err)
	}
	var jsonPath string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			jsonPath = filepath.Join(dir, e.Name())
			break
		}
	}
	if jsonPath == "" {
		return nil, fmt.Errorf("docling wrote no JSON output")
	}
	fi, err := os.Stat(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("stat docling output: %w", err)
	}
	if fi.Size() > maxDoclingJSON {
		return nil, fmt.Errorf("docling JSON is larger than %d bytes", maxDoclingJSON)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read docling output: %w", err)
	}
	return raw, nil
}

// parseDoclingJSON accepts the fields Docling uses for page text and provenance
// (page_no, prov/bbox, pages/size/label) while ignoring unknown structure. The
// exact JSON is preserved separately, so a richer schema can be indexed later
// without re-running the parser.
func parseDoclingJSON(raw []byte) ([]doclingBlock, map[int]string, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("decode docling json: %w", err)
	}
	pages := make(map[int]doclingPage)
	labels := make(map[int]string)
	collectDoclingPages(root, pages, labels)

	var blocks []doclingBlock
	seen := make(map[string]bool)
	collectDoclingBlocks(root, pages, &blocks, seen)
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if a.PageIndex != b.PageIndex {
			return a.PageIndex < b.PageIndex
		}
		ay, by := 0.0, 0.0
		ax, bx := 0.0, 0.0
		if a.Rect != nil {
			ay, ax = a.Rect.Y, a.Rect.X
		}
		if b.Rect != nil {
			by, bx = b.Rect.Y, b.Rect.X
		}
		if ay != by {
			return ay < by
		}
		if ax != bx {
			return ax < bx
		}
		return a.Text < b.Text
	})
	return blocks, labels, nil
}

func collectDoclingPages(v any, pages map[int]doclingPage, labels map[int]string) {
	switch x := v.(type) {
	case map[string]any:
		pn, hasPage := asInt(x["page_no"])
		if hasPage {
			p := pages[pn]
			if size, ok := asMap(x["size"]); ok {
				p.Width = asFloat(size["width"])
				p.Height = asFloat(size["height"])
			}
			if label, ok := x["page_label"].(string); ok && strings.TrimSpace(label) != "" {
				p.Label = strings.TrimSpace(label)
			}
			pages[pn] = p
			if p.Label != "" {
				labels[pn] = p.Label
			}
		}
		for _, child := range x {
			collectDoclingPages(child, pages, labels)
		}
	case []any:
		for _, child := range x {
			collectDoclingPages(child, pages, labels)
		}
	}
}

func collectDoclingBlocks(v any, pages map[int]doclingPage, out *[]doclingBlock, seen map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		text, _ := x["text"].(string)
		if strings.TrimSpace(text) != "" {
			provs, _ := x["prov"].([]any)
			if len(provs) == 0 {
				if pn, ok := asInt(x["page_no"]); ok {
					provs = []any{map[string]any{"page_no": float64(pn), "bbox": x["bbox"]}}
				}
			}
			for _, p := range provs {
				pm, ok := asMap(p)
				if !ok {
					continue
				}
				pn, ok := asInt(pm["page_no"])
				if !ok || pn <= 0 {
					continue
				}
				pageIndex := pn - 1
				block := doclingBlock{Text: strings.TrimSpace(text), PageIndex: pageIndex}
				if bbox, ok := asMap(pm["bbox"]); ok {
					block.Rect = normalizeBBox(bbox, pages[pn])
				}
				key := fmt.Sprintf("%d|%s|%v", pageIndex, block.Text, block.Rect)
				if seen[key] {
					continue
				}
				seen[key] = true
				*out = append(*out, block)
			}
		}
		for _, child := range x {
			collectDoclingBlocks(child, pages, out, seen)
		}
	case []any:
		for _, child := range x {
			collectDoclingBlocks(child, pages, out, seen)
		}
	}
}

func normalizeBBox(bbox map[string]any, page doclingPage) *archive.Rect {
	l := firstFloat(bbox, "l", "left")
	t := firstFloat(bbox, "t", "top")
	r := firstFloat(bbox, "r", "right")
	b := firstFloat(bbox, "b", "bottom")
	w, h := r-l, b-t
	if w <= 0 || h <= 0 {
		return nil
	}
	if page.Width > 0 && page.Height > 0 && (r > 1 || b > 1) {
		l, t, w, h = l/page.Width, t/page.Height, w/page.Width, h/page.Height
	}
	rect := archive.Rect{X: clamp01(l), Y: clamp01(t), Width: clamp01(w), Height: clamp01(h)}
	if rect.Width <= 0 || rect.Height <= 0 {
		return nil
	}
	return &rect
}

func storeDerivation(bs *blobstore.Store, data []byte, name string) (string, int64, error) {
	dir, err := os.MkdirTemp("", "provenance-derivation-*")
	if err != nil {
		return "", 0, fmt.Errorf("create derivation dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, filepath.Base(name))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", 0, fmt.Errorf("write derivation: %w", err)
	}
	hash, size, err := storeFile(bs, path)
	if err != nil {
		return "", 0, fmt.Errorf("store derivation: %w", err)
	}
	return hash, size, nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
	case string:
		i, err := strconv.Atoi(n)
		if err == nil {
			return i, true
		}
	}
	return 0, false
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return asFloat(v)
		}
	}
	return 0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func trimText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
