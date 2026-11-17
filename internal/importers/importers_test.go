package importers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/event"
)

// ---------------------------------------------------------------------------
// Git argument validation (security boundary)
// ---------------------------------------------------------------------------

func TestValidateGitArgs(t *testing.T) {
	cases := []struct {
		name    string
		repoURL string
		ref     string
		wantErr string
	}{
		{"https ok", "https://github.com/user/repo", "", ""},
		{"https with ref", "https://github.com/user/repo", "main", ""},
		{"ssh ok", "ssh://git@github.com/user/repo", "v1.0", ""},
		{"git ok", "git://github.com/user/repo", "", ""},
		{"http ok", "http://example.com/repo", "", ""},
		{"ext transport", "ext::sh -c 'touch /tmp/pwned'", "", "ext::"},
		{"file scheme", "file:///etc", "", "unsupported git URL scheme"},
		{"ftp scheme", "ftp://example.com/repo", "", "unsupported git URL scheme"},
		{"no scheme", "github.com/user/repo", "", "invalid repo URL"},
		{"empty url", "", "", "invalid repo URL"},
		{"dash ref", "https://github.com/user/repo", "--upload-pack=evil", "not allowed"},
		{"single dash ref", "https://github.com/user/repo", "-x", "not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitArgs(tc.repoURL, tc.ref)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestImportGitCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ImportGit(ctx, t.TempDir(), "https://example.com/repo", "", "coll")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestImportGitCloneHonorsCancellation proves the git subprocess is killed when
// the operation is canceled mid-clone instead of running to completion.
func TestImportGitCloneHonorsCancellation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	hit := make(chan struct{}, 1)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-hit:
			time.Sleep(100 * time.Millisecond)
			cancel()
		case <-done:
		}
	}()
	start := time.Now()
	_, err := ImportGit(ctx, t.TempDir(), srv.URL+"/repo.git", "", "coll")
	close(done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("clone ignored cancellation: %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// PDF import
// ---------------------------------------------------------------------------

// buildTestPDF writes a minimal single-page PDF with correct xref offsets.
func buildTestPDF(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	stream := "BT /F1 24 Tf 100 700 Td (Hello Provenance) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	offsets := make([]int, 0, len(objs))
	for i, o := range objs {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefPos)
	path := filepath.Join(t.TempDir(), "test.pdf")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportPDF(t *testing.T) {
	vault := t.TempDir()
	pdfPath := buildTestPDF(t)

	rev, err := ImportPDF(context.Background(), vault, pdfPath, "coll")
	if err != nil {
		t.Fatalf("ImportPDF: %v", err)
	}
	if len(rev.ID) != 64 {
		t.Fatalf("revision id = %q, want a sha256 hex", rev.ID)
	}
	if len(rev.Entities) != 1 || rev.Entities[0].Kind != "pdf" {
		t.Fatalf("entities = %+v", rev.Entities)
	}
	if len(rev.Artifacts) != 1 || !blobstore.New(vault).Exists(rev.Artifacts[0].Sha256) {
		t.Fatalf("pdf blob not stored: %+v", rev.Artifacts)
	}
	if len(rev.Entities[0].Documents) == 0 {
		t.Fatal("no page text extracted")
	}
	if !strings.Contains(rev.Entities[0].Documents[0].Content, "Hello Provenance") {
		t.Fatalf("page text = %q", rev.Entities[0].Documents[0].Content)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportPDF(ctx, vault, pdfPath, "coll"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// OpenAPI import
// ---------------------------------------------------------------------------

func TestImportOpenAPI(t *testing.T) {
	vault := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "openapi.yaml")
	spec := `openapi: 3.0.0
info:
  title: Test API
  version: 1.2.3
  description: A test spec
paths:
  /pets:
    get:
      summary: List pets
    post:
      summary: Create pet
`
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	rev, err := ImportOpenAPI(context.Background(), vault, specPath, "coll")
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(rev.Entities) != 1 || rev.Entities[0].Title != "Test API" || rev.Entities[0].Kind != "api_spec" {
		t.Fatalf("entities = %+v", rev.Entities)
	}
	if len(rev.Entities[0].Documents) != 2 {
		t.Fatalf("endpoint documents = %+v, want GET and POST", rev.Entities[0].Documents)
	}
	if rev.Entities[0].Text == nil || rev.Entities[0].Text.Content != "A test spec" {
		t.Fatalf("description text = %+v", rev.Entities[0].Text)
	}
	if len(rev.Artifacts) != 1 || !blobstore.New(vault).Exists(rev.Artifacts[0].Sha256) {
		t.Fatalf("spec blob not stored: %+v", rev.Artifacts)
	}

	if _, err := ImportOpenAPI(context.Background(), vault, filepath.Join(vault, "absent.yaml"), "coll"); err == nil {
		t.Fatal("expected an error for a missing spec")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportOpenAPI(ctx, vault, specPath, "coll"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Docs crawl
// ---------------------------------------------------------------------------

func TestImportDocsCrawlsWithinScope(t *testing.T) {
	mux := http.NewServeMux()
	page := func(title, links string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<html><body><h1>%s</h1>%s</body></html>", title, links)
		}
	}
	mux.HandleFunc("/docs/", page("Docs Root", `<a href="/docs/a">a</a> <a href="/other/x">out</a>`))
	mux.HandleFunc("/docs/a", page("Alpha", `<a href="/docs/b">b</a>`))
	mux.HandleFunc("/docs/b", page("Beta", ""))
	mux.HandleFunc("/other/x", page("Out Of Scope", ""))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	vault := t.TempDir()
	rev, err := ImportDocs(context.Background(), vault, srv.URL+"/docs/", srv.URL+"/docs/", 10, "coll")
	if err != nil {
		t.Fatalf("ImportDocs: %v", err)
	}
	docs := rev.Entities[0].Documents
	if len(docs) != 3 {
		t.Fatalf("documents = %d, want 3 in-scope pages", len(docs))
	}
	var titles []string
	for _, d := range docs {
		titles = append(titles, d.Content)
		if strings.Contains(d.ExternalID, "/other/") {
			t.Fatalf("out-of-scope page crawled: %s", d.ExternalID)
		}
	}
	joined := strings.Join(titles, "\n")
	for _, want := range []string{"## Docs Root", "## Alpha", "## Beta"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("heading %q not extracted; docs = %q", want, joined)
		}
	}

	// maxPages bounds the crawl.
	rev, err = ImportDocs(context.Background(), vault, srv.URL+"/docs/", srv.URL+"/docs/", 2, "coll")
	if err != nil {
		t.Fatalf("ImportDocs bounded: %v", err)
	}
	if len(rev.Entities[0].Documents) != 2 {
		t.Fatalf("bounded documents = %d, want 2", len(rev.Entities[0].Documents))
	}
}

func TestImportDocsHonorsCancellation(t *testing.T) {
	hit := make(chan struct{}, 8)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		if r.URL.Path == "/slow" {
			<-block
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h1>Root</h1><a href="/slow">slow</a></body></html>`)
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-hit:
			time.Sleep(100 * time.Millisecond)
			cancel()
		case <-done:
		}
	}()
	start := time.Now()
	_, err := ImportDocs(ctx, t.TempDir(), srv.URL+"/", "", 10, "coll")
	close(done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("crawl ignored cancellation: %v", elapsed)
	}
}

func TestImportDocsRejectsNonHTTPScheme(t *testing.T) {
	if _, err := ImportDocs(context.Background(), t.TempDir(), "ftp://example.com/docs", "", 10, "coll"); err == nil ||
		!strings.Contains(err.Error(), "unsupported url scheme") {
		t.Fatalf("err = %v, want unsupported scheme error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportDocs(ctx, t.TempDir(), "https://example.com/", "", 10, "coll"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Web import pre-flight (no Chrome required)
// ---------------------------------------------------------------------------

func TestImportWebPreflight(t *testing.T) {
	if _, err := ImportWeb(context.Background(), t.TempDir(), "ftp://example.com/", "", 5, false, "coll", ""); err == nil ||
		!strings.Contains(err.Error(), "unsupported url scheme") {
		t.Fatalf("err = %v, want unsupported scheme error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportWeb(ctx, t.TempDir(), "https://example.com/", "", 5, false, "coll", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestSafeTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"}, // cutting inside é must not split the rune
		{"héllo", 3, "hé"},
		{"日本語", 4, "日"}, // 3-byte runes
	}
	for _, tc := range cases {
		if got := safeTruncate(tc.in, tc.max); got != tc.want {
			t.Errorf("safeTruncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestResolveURL(t *testing.T) {
	cases := []struct {
		base, href, want string
	}{
		{"https://x.com/docs/", "a.html", "https://x.com/docs/a.html"},
		{"https://x.com/docs/", "/other", "https://x.com/other"},
		{"https://x.com/docs/", "https://y.com/z", "https://y.com/z"},
	}
	for _, tc := range cases {
		got, err := resolveURL(tc.base, tc.href)
		if err != nil || got != tc.want {
			t.Errorf("resolveURL(%q, %q) = (%q, %v), want %q", tc.base, tc.href, got, err, tc.want)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

// TestNarrateIsTransportNeutral pins the importer narration contract: with a
// sink no terminal write happens and the event carries the stable fact; without
// a sink the legacy CLI text is preserved byte-for-byte.
func TestNarrateIsTransportNeutral(t *testing.T) {
	var got []event.Event
	sink := event.Func(func(_ context.Context, e event.Event) { got = append(got, e) })

	stderr := captureStderr(t, func() {
		narratef(event.WithSink(context.Background(), sink), event.KindWarning, event.ReasonWebScrapeFailed,
			"https://example.com/a?token=CANARYQUERY", "capture failed",
			"[provenance] capture failed for %s: %v\n", "https://example.com/a?token=CANARYQUERY", errors.New("boom"))
	})
	if stderr != "" {
		t.Fatalf("terminal write with a sink: %q", stderr)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	e := got[0]
	if e.Kind != event.KindWarning || e.Reason != event.ReasonWebScrapeFailed || e.Detail != "capture failed" {
		t.Fatalf("event = %+v", e)
	}
	if strings.Contains(e.URL, "CANARYQUERY") || strings.Contains(e.Detail, "CANARYQUERY") {
		t.Fatalf("canary leaked: %+v", e)
	}

	stderr = captureStderr(t, func() {
		narratef(context.Background(), event.KindWarning, event.ReasonWebScrapeFailed, "", "",
			"[provenance] capture failed for %s: %v\n", "https://example.com/a", errors.New("boom"))
	})
	want := "[provenance] capture failed for https://example.com/a: boom\n"
	if stderr != want {
		t.Fatalf("legacy output changed: got %q want %q", stderr, want)
	}
}
