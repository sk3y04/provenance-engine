package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nopProgress struct{}

func (nopProgress) OnStart(string, string, int64)   {}
func (nopProgress) OnProgress(string, int64, int64) {}
func (nopProgress) OnDone(string, error)            {}

// localURL points the fixture URL at "localhost" instead of the 127.0.0.1
// literal: Download's URL-level guard rejects private IP literals, while the
// dial-level guard lives in NewDialer (which srv.Client() does not use).
func localURL(srv *httptest.Server, path string) string {
	return strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + path
}

// TestDownloadOverEmptyPart proves a zero-byte .part left by an interrupted
// attempt does not block every future download of the destination.
func TestDownloadOverEmptyPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fresh content")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.jpg")
	if err := os.WriteFile(dest+".part", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), Progress: nopProgress{}}
	if err := c.Download(context.Background(), localURL(srv, "/file.jpg"), dest, ""); err != nil {
		t.Fatalf("download over empty .part: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "fresh content" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
	if c.LastPath != dest || c.LastHash == "" {
		t.Fatalf("LastPath/LastHash not set: %q %q", c.LastPath, c.LastHash)
	}
}

// TestDownloadResumesPartial proves a non-empty .part resumes via Range and the
// final file joins both halves.
func TestDownloadResumesPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng == "bytes=5-" {
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, " world")
			return
		}
		fmt.Fprint(w, "hello world")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part", []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), Progress: nopProgress{}}
	if err := c.Download(context.Background(), localURL(srv, "/file.bin"), dest, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
}

// TestDownload416RecoversCompletePart proves a .part that already holds the
// whole resource is renamed into place when the server's Content-Range total
// confirms it, instead of being deleted.
func TestDownload416RecoversCompletePart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */8")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part", []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), Progress: nopProgress{}}
	if err := c.Download(context.Background(), localURL(srv, "/file.bin"), dest, ""); err != nil {
		t.Fatalf("416 recovery: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "complete" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
	if c.LastPath != dest || c.LastHash == "" {
		t.Fatalf("LastPath/LastHash not set on 416 completion: %q %q", c.LastPath, c.LastHash)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part not cleaned up: %v", err)
	}
}

// TestDownload416KeepsUncertainPart proves the .part is not destroyed when the
// server gives no usable Content-Range and no final file exists.
func TestDownload416KeepsUncertainPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest+".part", []byte("maybe complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), Progress: nopProgress{}}
	if err := c.Download(context.Background(), localURL(srv, "/file.bin"), dest, ""); err == nil {
		t.Fatal("expected an error when completion cannot be confirmed")
	}
	if _, err := os.Stat(dest + ".part"); err != nil {
		t.Fatalf(".part destroyed without confirmation: %v", err)
	}
}

// TestDownloadSkipsExistingDest proves the up-front skip reports path/hash and
// removes a stale .part.
func TestDownloadSkipsExistingDest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server contacted for an already-downloaded file")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(dest, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), Progress: nopProgress{}}
	if err := c.Download(context.Background(), localURL(srv, "/file.bin"), dest, ""); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if c.LastPath != dest || c.LastHash == "" {
		t.Fatalf("LastPath/LastHash not set: %q %q", c.LastPath, c.LastHash)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("stale .part not removed: %v", err)
	}
}

// TestDownloadRefusesPrivateIPLiteral proves the URL-level SSRF guard.
func TestDownloadRefusesPrivateIPLiteral(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient, Progress: nopProgress{}}
	err := c.Download(context.Background(), "http://127.0.0.1/x.jpg", filepath.Join(t.TempDir(), "x.jpg"), "")
	if err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("err = %v, want private-network refusal", err)
	}
}

// TestNewDialerBlocksPrivateNetworks proves the shared dial control refuses
// loopback and private ranges, so platform-supplied URLs cannot aim the engine
// at internal services even when the hostname is not an IP literal.
func TestNewDialerBlocksPrivateNetworks(t *testing.T) {
	d := NewDialer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, addr := range []string{"127.0.0.1:80", "10.0.0.1:80", "192.168.1.1:80", "169.254.169.254:80", "[::1]:80"} {
		_, err := d.DialContext(ctx, "tcp", addr)
		if err == nil || !strings.Contains(err.Error(), "private network") {
			t.Errorf("dial %s: err = %v, want private-network block", addr, err)
		}
	}
}

func TestIsPrivateHost(t *testing.T) {
	private := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.1", "169.254.1.1", "::1", "fc00::1", "fd12:3456::1"}
	public := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "2606:4700::1111", "example.com"}
	for _, h := range private {
		if !isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = false, want true", h)
		}
	}
	for _, h := range public {
		if isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = true, want false", h)
		}
	}
}

func TestSafeRedirect(t *testing.T) {
	req := &http.Request{URL: mustParse("http://127.0.0.1/x")}
	if err := SafeRedirect(req, nil); err == nil {
		t.Fatal("redirect to loopback not blocked")
	}
	req = &http.Request{URL: mustParse("https://example.com/x")}
	if err := SafeRedirect(req, nil); err != nil {
		t.Fatalf("public redirect blocked: %v", err)
	}
	via := make([]*http.Request, 10)
	if err := SafeRedirect(req, via); err == nil {
		t.Fatal("redirect chain limit not enforced")
	}
}

func TestContentRangeTotal(t *testing.T) {
	cases := []struct {
		header string
		want   int64
		ok     bool
	}{
		{"bytes */8", 8, true},
		{"bytes */0", 0, true},
		{"bytes 0-7/1234", 1234, true},
		{"", 0, false},
		{"bytes */junk", 0, false},
		{"bytes */-1", 0, false},
	}
	for _, tc := range cases {
		got, ok := contentRangeTotal(tc.header)
		if got != tc.want || ok != tc.ok {
			t.Errorf("contentRangeTotal(%q) = (%d, %v), want (%d, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
