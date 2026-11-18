package manifest

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureManifestCaptureKeyRoundTrip: the idempotency key is an additive,
// optional field. A producer that sets it must read it back, and a manifest
// written without one must not gain a stray field that changes the file.
func TestCaptureManifestCaptureKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cm := &CaptureManifest{
		Format:     CaptureFormatVersion,
		OutputDir:  dir,
		CaptureKey: "abc123def456",
		Items: []CaptureItem{{
			ExternalID:     "x",
			URL:            "https://example.test/x",
			DownloadedPath: "f.bin",
			Sha256:         strings.Repeat("0", 64),
		}},
	}
	path := filepath.Join(dir, "runs", "run.json")
	if err := WriteCaptureManifest(dir, path, cm); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadCaptureManifest(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.CaptureKey != cm.CaptureKey {
		t.Errorf("capture key = %q, want %q", got.CaptureKey, cm.CaptureKey)
	}
	if len(got.Items) != 1 || got.Items[0].DownloadedPath != "f.bin" {
		t.Errorf("items = %+v", got.Items)
	}

	// A producer that never retries a capture leaves the key empty, and the
	// serialised form stays compatible with readers that predate the field.
	plain := &CaptureManifest{Format: CaptureFormatVersion, OutputDir: dir}
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "capture_key") {
		t.Errorf("empty key was serialised: %s", raw)
	}
	var back CaptureManifest
	if err := json.Unmarshal([]byte(`{"format":"provenance/capture+1","items":[]}`), &back); err != nil {
		t.Fatalf("a manifest without a key must still parse: %v", err)
	}
	if back.CaptureKey != "" {
		t.Errorf("missing key decoded as %q", back.CaptureKey)
	}
}
