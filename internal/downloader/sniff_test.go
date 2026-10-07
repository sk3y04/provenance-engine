package downloader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSniffExt(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, "png", true},
		{"jpg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, "jpg", true},
		{"gif", []byte("GIF89a"), "gif", true},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "webp", true},
		{"mp4", []byte("\x00\x00\x00\x20ftypisom"), "mp4", true},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x01}, "webm", true},
		{"bmp", []byte("BM\x00\x00"), "bmp", true},
		{"unknown", []byte("hello world"), "", false},
		{"empty", []byte{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), tc.name+".bin")
			if err := os.WriteFile(p, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := SniffExt(p)
			if got != tc.want || ok != tc.ok {
				t.Errorf("SniffExt = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSniffExtMissingFile(t *testing.T) {
	if _, ok := SniffExt("/nonexistent/file.png"); ok {
		t.Errorf("expected ok=false for missing file")
	}
}
