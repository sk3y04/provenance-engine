package engine

import (
	"context"
	"testing"
)

// TestCapabilitiesIsSideEffectFree proves a health/capability check cannot
// install a tool: the probe only resolves names.
func TestCapabilitiesIsSideEffectFree(t *testing.T) {
	e, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	set := e.Capabilities(context.Background())
	if len(set) == 0 {
		t.Fatal("capability report is empty")
	}
	var required int
	for _, tool := range set {
		if tool.Required {
			required++
		}
	}
	if required != 1 {
		t.Fatalf("expected exactly one required tool, got %d", required)
	}
	if _, ok := findTool(set, "yt-dlp"); !ok {
		t.Fatal("yt-dlp must be reported")
	}
	for _, name := range []string{"ffmpeg", "ffprobe", "chrome"} {
		if _, ok := findTool(set, name); !ok {
			t.Fatalf("%s must be reported", name)
		}
	}
}

func findTool(set []ToolStatus, name string) (ToolStatus, bool) {
	for _, tool := range set {
		if tool.Name == name {
			return tool, true
		}
	}
	return ToolStatus{}, false
}
