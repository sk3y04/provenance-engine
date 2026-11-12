package encode

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PCI vendor IDs as written by sysfs (/sys/class/drm/card*/device/vendor),
// normalized to lowercase hex without the 0x prefix.
const (
	vendorIntel  = "8086"
	vendorAMD    = "1002"
	vendorAMDAlt = "1022"
)

// renderNode is one /dev/dri/renderD* entry with the PCI vendor of the GPU
// behind it. Vendor is "" when it cannot be determined (no matching card in
// the sysfs tree).
type renderNode struct {
	Node   string // e.g. "renderD129"
	Path   string // e.g. "/dev/dri/renderD129"
	Vendor string
}

// detectRenderNodes enumerates the render nodes under driRoot (e.g.
// /dev/dri) and resolves each node's GPU vendor through the sysfs tree under
// sysRoot (e.g. /sys): sysRoot/class/drm/cardN/device/vendor plus
// sysRoot/class/drm/cardN/device/drm/<node>. The probe is read-only. Results
// are sorted by render node number.
func detectRenderNodes(driRoot, sysRoot string) []renderNode {
	entries, err := os.ReadDir(driRoot)
	if err != nil {
		return nil
	}

	// Map node name -> vendor from the DRM card devices. Note the /sys
	// entries are symlinks (DirEntry.IsDir() is false), so eligibility is
	// tested by whether the vendor file actually reads.
	vendorOf := map[string]string{}
	cards, err := os.ReadDir(filepath.Join(sysRoot, "class", "drm"))
	if err == nil {
		for _, c := range cards {
			deviceDir := filepath.Join(sysRoot, "class", "drm", c.Name(), "device")
			vendor := ""
			if b, err := os.ReadFile(filepath.Join(deviceDir, "vendor")); err == nil {
				vendor = normalizeVendor(string(b))
			}
			if vendor == "" {
				continue
			}
			nodeDirs, err := os.ReadDir(filepath.Join(deviceDir, "drm"))
			if err != nil {
				continue
			}
			for _, n := range nodeDirs {
				if strings.HasPrefix(n.Name(), "renderD") {
					vendorOf[n.Name()] = vendor
				}
			}
		}
	}

	var nodes []renderNode
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "renderD") {
			continue
		}
		nodes = append(nodes, renderNode{
			Node:   e.Name(),
			Path:   filepath.Join(driRoot, e.Name()),
			Vendor: vendorOf[e.Name()],
		})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodeNumber(nodes[i].Node) < nodeNumber(nodes[j].Node)
	})
	return nodes
}

// normalizeVendor turns sysfs vendor text like "0x8086\n" into "8086".
func normalizeVendor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	return s
}

// nodeNumber extracts the numeric suffix of a render node name for sorting.
func nodeNumber(name string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(name, "renderD"))
	return n
}

// selectDevices picks the devices to encode on when the user does not pass
// --devices. QSV pins the Intel render nodes, AMF the AMD nodes, NVENC the
// first GPU index. When no node matches the vendor (or vendors are unknown),
// every node is pinned so the encoder library either works or fails loudly
// instead of guessing a machine-specific default. An empty result means "pin
// nothing and let the encoder library choose".
func selectDevices(encoder string, nodes []renderNode) []string {
	if encoder == EncoderNVENC {
		return []string{"0"}
	}
	var want []string
	if encoder == EncoderAMF {
		want = []string{vendorAMD, vendorAMDAlt}
	} else {
		want = []string{vendorIntel}
	}
	if len(nodes) == 0 {
		return nil
	}
	var matched []string
	for _, n := range nodes {
		for _, v := range want {
			if n.Vendor == v {
				matched = append(matched, n.Path)
			}
		}
	}
	if len(matched) == 0 {
		for _, n := range nodes {
			matched = append(matched, n.Path)
		}
	}
	return matched
}

// probeDevices resolves the default device set for a codec. It is a package
// variable so tests can override it without touching /dev/dri or /sys.
var probeDevices = func(codec string) []string {
	return selectDevices(codec, detectRenderNodes("/dev/dri", "/sys"))
}
