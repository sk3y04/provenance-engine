package manifest

import (
	"math"
	"strconv"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"":      0,
		"10":    10,
		"1KB":   1024,
		"1.5MB": 1572864,
		"2gb":   2 * 1024 * 1024 * 1024,
	}
	for input, want := range cases {
		got, err := ParseSize(input)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseSize(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestParseSizeRejectsInvalidValues proves hostile or nonsensical sizes are
// rejected instead of silently converting to a wrapped/negative byte count.
func TestParseSizeRejectsInvalidValues(t *testing.T) {
	// Boundary strings are computed, never hand-typed: 2^63 is exactly the
	// first unrepresentable int64, and float64(math.MaxInt64) rounds up to
	// it, so a strict ">" guard lets it through and int64(bytes) wraps.
	two63 := strconv.FormatUint(1<<63, 10)
	for _, input := range []string{
		"-5mb",
		"nan",
		"inf",
		"1e30gb", // overflows int64
		"10lightyears",
		two63,
		two63 + "b",
		strconv.FormatInt(1<<33, 10) + "gb", // 2^33 GiB = 2^63 bytes
		strconv.FormatInt(1<<53, 10) + "kb", // 2^53 KiB = 2^63 bytes
	} {
		if got, err := ParseSize(input); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", input, got)
		}
	}
}

// TestParseSizeAcceptsLargestRepresentable pins the largest float64 below
// 2^63 so the overflow guard does not over-reject valid sizes.
func TestParseSizeAcceptsLargestRepresentable(t *testing.T) {
	// float64 spacing just below 2^63 is 1024, so 2^63-1024 is the largest
	// representable value that still fits in int64.
	maxRepresentable := int64(math.MaxInt64) - 1023
	got, err := ParseSize(strconv.FormatInt(maxRepresentable, 10))
	if err != nil {
		t.Fatalf("ParseSize largest representable: %v", err)
	}
	if got != maxRepresentable {
		t.Fatalf("ParseSize = %d, want %d", got, maxRepresentable)
	}
}

func TestFilterItems(t *testing.T) {
	items := []Item{
		{URL: "https://x/a.mp4", Title: "bonus video", Filename: "a.mp4", Size: 100},
		{URL: "https://x/b.zip", Title: "archive", Filename: "b.zip", Size: 5000},
		{URL: "https://x/c.jpg", Title: "preview", Filename: "c.jpg", Size: 50},
	}
	filtered, err := FilterItems(items, FilterOptions{
		IncludeExt:  []string{"mp4", "zip"},
		MaxSize:     1000,
		TitleMatch:  "bonus|archive",
		TitleReject: "preview",
	})
	if err != nil {
		t.Fatalf("FilterItems: %v", err)
	}
	if len(filtered) != 1 || filtered[0].URL != "https://x/a.mp4" {
		t.Fatalf("filtered = %+v, want only mp4", filtered)
	}
}

func TestSummary(t *testing.T) {
	m := New("src", "test", []Item{{URL: "u1", Filename: "a.mp4", Size: 10}, {URL: "u2", Filename: "b.jpg", Size: 20}})
	s := m.Summary()
	if s.Count != 2 || s.KnownSize != 30 || s.Extensions["mp4"] != 1 || s.Extensions["jpg"] != 1 {
		t.Fatalf("summary = %+v", s)
	}
}
