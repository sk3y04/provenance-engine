package downloader

import (
	"bytes"
	"io"
	"os"
)

// SniffExt inspects the magic bytes of the file at path and returns the
// canonical extension for recognized media formats. The second return value
// is false when the file is too short or its format is unknown.
func SniffExt(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	var buf [16]byte
	n, _ := io.ReadFull(f, buf[:])
	if n < 4 {
		return "", false
	}
	b := buf[:n]
	switch {
	case bytes.Equal(b[:4], []byte{0x89, 'P', 'N', 'G'}):
		return "png", true
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg", true
	case bytes.Equal(b[:4], []byte("GIF8")):
		return "gif", true
	case n >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp", true
	case n >= 8 && bytes.Equal(b[4:8], []byte("ftyp")):
		return "mp4", true
	case b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3:
		return "webm", true
	case b[0] == 'B' && b[1] == 'M':
		return "bmp", true
	}
	return "", false
}
