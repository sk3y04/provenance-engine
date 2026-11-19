package slskd

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSearchTimeoutIsSentInMilliseconds pins the single contract fact that the
// upstream documentation gets wrong. Both SearchRequest.cs and the 0.26.0
// Swagger claim the value is in seconds; the running binary ended a search 53 ms
// after start for searchTimeout=5 (docs/findings-slskd-0.26-contract.md §2). A
// regression that sends seconds would make every search either 1000x too long or
// trivially short, and no other test would notice.
func TestSearchTimeoutIsSentInMilliseconds(t *testing.T) {
	req := SearchRequest{
		ID:              "11111111-1111-1111-1111-111111111111",
		SearchText:      "beethoven",
		SearchTimeoutMS: 15000,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	got, ok := wire["searchTimeout"]
	if !ok {
		t.Fatalf("searchTimeout missing from wire payload: %s", data)
	}
	if got != float64(15000) {
		t.Fatalf("searchTimeout = %v, want 15000 (milliseconds, not seconds)", got)
	}
	// 15000 seconds would be a 4h10m search; assert the value is not a
	// seconds-shaped default.
	if got == float64(15) {
		t.Fatalf("searchTimeout looks like a seconds value: %v", got)
	}
	for _, key := range []string{"searchText", "fileLimit", "responseLimit", "filterResponses"} {
		if _, ok := wire[key]; !ok && (key == "searchText") {
			t.Fatalf("%s missing from wire payload", key)
		}
	}
}

// TestTransferStateFlagsParsing proves composite states are compared as a set,
// not as literal strings. The serialiser emits "Completed, TimedOut" with a
// comma and a space, but the parser must survive any spacing or casing.
func TestTransferStateFlagsParsing(t *testing.T) {
	cases := []struct {
		state     string
		terminal  bool
		succeeded bool
	}{
		{"Completed, Succeeded", true, true},
		{"Completed,TimedOut", true, false},
		{"Completed, TimedOut", true, false},
		{"COMPLETED ,  errored", true, false},
		{"Completed, Rejected", true, false},
		{"Completed, Aborted", true, false},
		{"Cancelled", true, false},
		{"Completed, Cancelled", true, false},
		{"Queued, Remotely", false, false},
		{"InProgress", false, false},
		{"Initializing", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		terminal, succeeded := transferOutcome(tc.state)
		if terminal != tc.terminal || succeeded != tc.succeeded {
			t.Errorf("transferOutcome(%q) = (%v,%v), want (%v,%v)", tc.state, terminal, succeeded, tc.terminal, tc.succeeded)
		}
	}
}

// TestTerminalFailureReason covers the "0s stall" rule: every completed-but-not-
// succeeded state and a bare Cancelled must classify immediately, with the exact
// upstream string preserved as the reason.
func TestTerminalFailureReason(t *testing.T) {
	for _, state := range []string{
		"Completed, Rejected",
		"Completed, TimedOut",
		"Completed, Errored",
		"Completed, Aborted",
		"Completed, Cancelled",
		"Cancelled",
	} {
		failed, reason := terminalFailure(state)
		if !failed {
			t.Fatalf("terminalFailure(%q) did not classify as failure", state)
		}
		if reason != state {
			t.Fatalf("terminalFailure(%q) reason = %q, want the exact state", state, reason)
		}
	}
	for _, state := range []string{"Queued, Remotely", "InProgress", "Completed, Succeeded", ""} {
		if failed, _ := terminalFailure(state); failed {
			t.Fatalf("terminalFailure(%q) classified as failure", state)
		}
	}
}

// TestVersionParsing pins the dotted-quad version shape slskd reports
// ("0.26.0.0 (0.26.0.0+e42a525d)") and the semver triple a future release might
// use.
func TestVersionParsing(t *testing.T) {
	cases := []struct {
		raw                 string
		major, minor, patch int
		ok                  bool
	}{
		{"0.26.0.0", 0, 26, 0, true},
		{"0.26.0.0 (0.26.0.0+e42a525d)", 0, 26, 0, true},
		{"0.26.2.0", 0, 26, 2, true},
		{"0.26.1", 0, 26, 1, true},
		{"1.0.0-beta.1", 1, 0, 0, true},
		{"", 0, 0, 0, false},
		{"not-a-version", 0, 0, 0, false},
		{"0.25.9.0", 0, 25, 9, true},
	}
	for _, tc := range cases {
		v, err := parseVersion(tc.raw)
		if tc.ok != (err == nil) {
			t.Fatalf("parseVersion(%q) error = %v, want ok=%v", tc.raw, err, tc.ok)
		}
		if err != nil {
			continue
		}
		if v.Major != tc.major || v.Minor != tc.minor || v.Patch != tc.patch {
			t.Errorf("parseVersion(%q) = %+v, want %d.%d.%d", tc.raw, v, tc.major, tc.minor, tc.patch)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	min := Version{0, 26, 0}
	for raw, want := range map[string]bool{
		"0.25.9.0": false,
		"0.26.0.0": true,
		"0.26.2.0": true,
		"0.27.0.0": true,
		"1.0.0.0":  true,
	} {
		v, err := parseVersion(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.AtLeast(min); got != want {
			t.Errorf("%s.AtLeast(0.26.0) = %v, want %v", raw, got, want)
		}
	}
}

// TestPathSegmentValidation pins the Linux NAME_MAX pre-check (#1818): a segment
// above 255 UTF-8 bytes must be refused before an enqueue, and the check counts
// bytes rather than runes so multibyte names are measured the way the kernel
// measures them.
func TestPathSegmentValidation(t *testing.T) {
	// 125 "é" is 250 bytes; adding ".flac" lands exactly on the 255-byte limit.
	atLimit := strings.Repeat("é", 125) + ".flac"
	if len(atLimit) != 255 {
		t.Fatalf("fixture is %d bytes, want 255", len(atLimit))
	}
	if err := ValidateRemotePath("music\\" + atLimit); err != nil {
		t.Fatalf("255-byte segment refused: %v", err)
	}
	long256 := strings.Repeat("é", 126) + ".flac" // 257 bytes
	if len(long256) != 257 {
		t.Fatalf("fixture is %d bytes, want 257", len(long256))
	}
	if err := ValidateRemotePath("music\\" + long256); err == nil {
		t.Fatal("257-byte segment accepted")
	}
	if err := ValidateRemotePath("music\\Album\\track.flac"); err != nil {
		t.Fatalf("ordinary path refused: %v", err)
	}
	for _, bad := range []string{"", "   ", "music\\..\\x.flac"} {
		if err := ValidateRemotePath(bad); err == nil {
			t.Fatalf("ValidateRemotePath(%q) accepted", bad)
		}
	}
}
