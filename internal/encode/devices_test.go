package encode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildNodeTree creates a fake /dev/dri + /sys layout where each render node
// name is backed by a DRM card whose PCI vendor ID is given in vendors
// (e.g. "0x8086" for Intel). It returns the dri and sys roots.
func buildNodeTree(t *testing.T, vendors map[string]string) (string, string) {
	t.Helper()
	dri := t.TempDir()
	sys := t.TempDir()

	names := make([]string, 0, len(vendors))
	for n := range vendors {
		names = append(names, n)
	}
	sort.Strings(names)

	for i, n := range names {
		if err := os.WriteFile(filepath.Join(dri, n), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		card := filepath.Join(sys, "class", "drm", fmt.Sprintf("card%d", i))
		if err := os.MkdirAll(filepath.Join(card, "device", "drm"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(card, "device", "vendor"), []byte(vendors[n]), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(card, "device", "drm", n), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dri, sys
}

func TestDetectRenderNodesMapsVendors(t *testing.T) {
	dri, sys := buildNodeTree(t, map[string]string{
		"renderD128": "0x10de", // NVIDIA
		"renderD129": "0x8086", // Intel
		"renderD130": "0x8086", // Intel
	})

	nodes := detectRenderNodes(dri, sys)
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %#v", nodes)
	}
	// Nodes must be ordered by render node number, not card index.
	var got []string
	for _, n := range nodes {
		got = append(got, n.Node+"/"+n.Vendor)
	}
	want := []string{"renderD128/10de", "renderD129/8086", "renderD130/8086"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	if nodes[0].Path != filepath.Join(dri, "renderD128") {
		t.Fatalf("Path must be the device path, got %q", nodes[0].Path)
	}
}

func TestDetectRenderNodesSymlinkedCards(t *testing.T) {
	// On a real sysfs mount, /sys/class/drm/cardN entries are symlinks, so
	// detection must not rely on DirEntry.IsDir().
	dri := t.TempDir()
	sys := t.TempDir()
	real := t.TempDir()

	if err := os.WriteFile(filepath.Join(dri, "renderD129"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "device", "drm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "device", "vendor"), []byte("0x8086"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "device", "drm", "renderD129"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	classDrm := filepath.Join(sys, "class", "drm")
	if err := os.MkdirAll(classDrm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(classDrm, "card1")); err != nil {
		t.Fatal(err)
	}

	nodes := detectRenderNodes(dri, sys)
	if len(nodes) != 1 || nodes[0].Vendor != "8086" {
		t.Fatalf("symlinked card must still resolve its vendor, got %#v", nodes)
	}
}

func TestDetectRenderNodesUnresolvableVendor(t *testing.T) {
	dri := t.TempDir()
	sys := t.TempDir()
	// Node exists but no card claims it: vendor stays empty.
	if err := os.WriteFile(filepath.Join(dri, "renderD128"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	nodes := detectRenderNodes(dri, sys)
	if len(nodes) != 1 || nodes[0].Node != "renderD128" || nodes[0].Vendor != "" {
		t.Fatalf("expected single node with empty vendor, got %#v", nodes)
	}
}

func TestDetectRenderNodesMissingDri(t *testing.T) {
	nodes := detectRenderNodes(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	if len(nodes) != 0 {
		t.Fatalf("expected no nodes without a dri root, got %#v", nodes)
	}
}

func TestSelectDevicesQSVPrefersIntel(t *testing.T) {
	nodes := []renderNode{
		{Node: "renderD128", Path: "/dev/dri/renderD128", Vendor: "10de"},
		{Node: "renderD129", Path: "/dev/dri/renderD129", Vendor: "8086"},
		{Node: "renderD130", Path: "/dev/dri/renderD130", Vendor: "8086"},
	}
	got := selectDevices(EncoderQSV, nodes)
	if strings.Join(got, ",") != "/dev/dri/renderD129,/dev/dri/renderD130" {
		t.Fatalf("qsv must pin Intel nodes only, got %v", got)
	}
}

func TestSelectDevicesAMFPrefersAMD(t *testing.T) {
	nodes := []renderNode{
		{Node: "renderD128", Path: "/dev/dri/renderD128", Vendor: "10de"},
		{Node: "renderD129", Path: "/dev/dri/renderD129", Vendor: "1002"},
		{Node: "renderD130", Path: "/dev/dri/renderD130", Vendor: "1022"},
	}
	got := selectDevices(EncoderAMF, nodes)
	if strings.Join(got, ",") != "/dev/dri/renderD129,/dev/dri/renderD130" {
		t.Fatalf("amf must pin AMD nodes only, got %v", got)
	}
}

func TestSelectDevicesFallsBackToAllWhenNoVendorMatch(t *testing.T) {
	nodes := []renderNode{
		{Node: "renderD128", Path: "/dev/dri/renderD128", Vendor: "10de"},
		{Node: "renderD129", Path: "/dev/dri/renderD129", Vendor: ""},
	}
	got := selectDevices(EncoderQSV, nodes)
	if strings.Join(got, ",") != "/dev/dri/renderD128,/dev/dri/renderD129" {
		t.Fatalf("qsv without Intel nodes must fall back to all nodes, got %v", got)
	}
}

func TestSelectDevicesNVENCUsesFirstGPUIndex(t *testing.T) {
	got := selectDevices(EncoderNVENC, nil)
	if len(got) != 1 || got[0] != "0" {
		t.Fatalf("nvenc default must be GPU index 0, got %v", got)
	}
}

func TestSelectDevicesNoNodes(t *testing.T) {
	if got := selectDevices(EncoderQSV, nil); len(got) != 0 {
		t.Fatalf("expected no devices without nodes, got %v", got)
	}
}

func TestRunAutoDetectsDeviceWhenUnset(t *testing.T) {
	stubBinaries(t)
	prev := probeDevices
	probeDevices = func(string) []string { return []string{"/dev/dri/renderD129"} }
	t.Cleanup(func() { probeDevices = prev })

	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	fake := &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"}
	res, err := Run(context.Background(), Options{Dir: dir, Encoder: "qsv", Quality: 30}, fake)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Devices != 1 {
		t.Fatalf("expected 1 auto-detected device, got %d", res.Devices)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var dev string
	for _, c := range fake.calls {
		if !strings.HasSuffix(c.bin, "ffmpeg") || hasArg(c.args, "-encoders") {
			continue
		}
		for i := 0; i+1 < len(c.args); i++ {
			if c.args[i] == "-qsv_device" {
				dev = c.args[i+1]
			}
		}
	}
	if dev != "/dev/dri/renderD129" {
		t.Fatalf("auto-detected device not pinned in ffmpeg args, got %q", dev)
	}
}

func TestRunNoDevicePinWhenDetectionEmpty(t *testing.T) {
	stubBinaries(t)
	prev := probeDevices
	probeDevices = func(string) []string { return nil }
	t.Cleanup(func() { probeDevices = prev })

	dir := t.TempDir()
	mkfile(t, dir, "a.mp4")

	fake := &fakeRunner{probeOut: qsvAvailable(), ffprobeOut: "24"}
	res, err := Run(context.Background(), Options{Dir: dir, Encoder: "qsv", Quality: 30}, fake)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("encode must still run without a pinned device, got %#v", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, c := range fake.calls {
		if strings.HasSuffix(c.bin, "ffmpeg") && !hasArg(c.args, "-encoders") && hasArg(c.args, "-qsv_device") {
			t.Fatalf("no device must be pinned when detection finds none: %v", c.args)
		}
	}
}
