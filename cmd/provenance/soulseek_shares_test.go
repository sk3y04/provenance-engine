package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSoulseekSharesAndUploadsCLI drives the share opt-in and upload monitor
// against a fake slskd.
func TestSoulseekSharesAndUploadsCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	mount := filepath.Join(dir, "downloads")
	shareRoot := filepath.Join(dir, "shares")
	src := filepath.Join(dir, "media", "Album")
	for _, d := range []string{mount, shareRoot, src} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "track.flac"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/api/v0/shares"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/api/v0/shares/contents"):
			_, _ = w.Write([]byte(`[{"name":"Provenance\\Album","fileCount":1,"files":[{"filename":"track.flac","size":5,"extension":"flac"}]}]`))
		case strings.HasSuffix(r.URL.Path, "/api/v0/shares"):
			_, _ = w.Write([]byte(`{"local":[{"id":"X","alias":"Test","isExcluded":false,"localPath":"/shared","remotePath":"Test","directories":1,"files":1}]}`))
		case strings.HasSuffix(r.URL.Path, "/api/v0/transfers/uploads"):
			_, _ = w.Write([]byte(`[{"id":"u1","username":"peer1","filename":"music\\a.flac","state":"InProgress","size":10}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if _, err := runSoulseek(t, "settings", "set", "--base-url", srv.URL, "--api-key", "k", "--enable",
		"--downloads-mount", mount, "--curated-share-root", shareRoot); err != nil {
		t.Fatal(err)
	}

	out, err := runSoulseek(t, "shares", "preview", src)
	if err != nil {
		t.Fatalf("preview: %v\n%s", err, out)
	}
	if !strings.Contains(out, "files: 1") || !strings.Contains(out, "excluded: 1") {
		t.Fatalf("preview output = %q", out)
	}
	if !strings.Contains(out, "nothing has been linked yet") {
		t.Fatalf("preview does not make the confirmation explicit: %q", out)
	}

	out, err = runSoulseek(t, "shares", "add", src, "--alias", "Test")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	linked := filepath.Join(shareRoot, "Test", "track.flac")
	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("hardlink not created: %v", err)
	}
	srcInfo, _ := os.Stat(filepath.Join(src, "track.flac"))
	linkInfo, _ := os.Stat(linked)
	if !os.SameFile(srcInfo, linkInfo) {
		t.Fatal("share copy is not a hardlink")
	}

	out, err = runSoulseek(t, "shares", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "Test") || !strings.Contains(out, "1 file(s)") {
		t.Fatalf("list output = %q", out)
	}

	out, err = runSoulseek(t, "shares", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Test") || !strings.Contains(out, "1 file(s)") {
		t.Fatalf("status output = %q", out)
	}

	out, err = runSoulseek(t, "shares", "files", "--page-size", "10")
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	if !strings.Contains(out, "track.flac") || !strings.Contains(out, "page 1/1") {
		t.Fatalf("files output = %q", out)
	}

	out, err = runSoulseek(t, "uploads", "list")
	if err != nil {
		t.Fatalf("uploads: %v", err)
	}
	if !strings.Contains(out, "active=1") {
		t.Fatalf("uploads output = %q", out)
	}

	out, err = runSoulseek(t, "shares", "remove", "test")
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(shareRoot, "Test")); !os.IsNotExist(err) {
		t.Fatal("share directory survived removal")
	}
	if _, err := os.Stat(filepath.Join(src, "track.flac")); err != nil {
		t.Fatal("removal deleted the source file")
	}
}

// TestSoulseekSharesRequireConfiguredRoot proves the CLI does not invent a share
// root.
func TestSoulseekSharesRequireConfiguredRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if _, err := runSoulseek(t, "settings", "set", "--base-url", "http://127.0.0.1:5030", "--api-key", "k",
		"--enable", "--downloads-mount", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runSoulseek(t, "shares", "list"); err == nil {
		t.Fatal("shares list succeeded without a curated share root")
	}
}
