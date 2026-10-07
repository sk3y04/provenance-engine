package extractor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRdNormalizeExt(t *testing.T) {
	cases := map[string]string{
		"png": "png", ".PNG": "png", " jpg ": "jpg", "Image": "", "": "",
		"gifv": "gifv", "webp": "webp", "svg": "",
	}
	for in, want := range cases {
		if got := rdNormalizeExt(in); got != want {
			t.Errorf("rdNormalizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRdPostMediaGalleryExt(t *testing.T) {
	cases := []struct {
		name string
		e    string
		u    string
		want string
	}{
		{"valid passthrough", "png", "https://i.redd.it/abc.png", "png"},
		{"uppercase normalized", "PNG", "https://i.redd.it/abc.png", "png"},
		{"junk falls back to url", "Image", "https://i.redd.it/abc.png", "png"},
		{"empty falls back to url", "", "https://i.redd.it/abc.webp", "webp"},
		{"junk and no url ext", "Image", "https://i.redd.it/abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := rdPostData{
				GalleryData: &rdGalleryData{Items: []rdGalleryItem{{MediaID: "m1"}}},
				MediaMetadata: map[string]rdMediaMetadata{
					"m1": {S: rdMediaMetadataSource{U: tc.u}, E: tc.e},
				},
			}
			items := rdPostMedia(d)
			if len(items) != 1 {
				t.Fatalf("expected 1 item, got %d", len(items))
			}
			if items[0].Ext != tc.want {
				t.Errorf("ext = %q, want %q", items[0].Ext, tc.want)
			}
		})
	}
}

func TestFixMediaExt(t *testing.T) {
	dir := t.TempDir()

	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	jpgMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}

	t.Run("renames bogus extension", func(t *testing.T) {
		src := filepath.Join(dir, "abc.Image")
		if err := os.WriteFile(src, pngMagic, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixMediaExt(src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "abc.png")); err != nil {
			t.Errorf("expected rename to abc.png: %v", err)
		}
		if _, err := os.Stat(src); err == nil {
			t.Errorf("original abc.Image should be gone")
		}
	})

	t.Run("adds missing extension", func(t *testing.T) {
		src := filepath.Join(dir, "abc")
		if err := os.WriteFile(src, jpgMagic, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixMediaExt(src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "abc.jpg")); err != nil {
			t.Errorf("expected rename to abc.jpg: %v", err)
		}
	})

	t.Run("matching extension untouched", func(t *testing.T) {
		src := filepath.Join(dir, "abc2.png")
		if err := os.WriteFile(src, pngMagic, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixMediaExt(src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("file should be untouched: %v", err)
		}
	})

	t.Run("unknown content untouched", func(t *testing.T) {
		src := filepath.Join(dir, "abc3.txt")
		if err := os.WriteFile(src, []byte("plain text"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixMediaExt(src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("file should be untouched: %v", err)
		}
	})

	t.Run("no clobber", func(t *testing.T) {
		src := filepath.Join(dir, "abc4.Image")
		tgt := filepath.Join(dir, "abc4.png")
		if err := os.WriteFile(src, pngMagic, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tgt, pngMagic, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fixMediaExt(src); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("source must remain when target exists: %v", err)
		}
	})
}
