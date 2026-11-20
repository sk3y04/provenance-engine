package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadURL is a parseable but unreachable fixture URL. The engine's dispatch path
// runs end to end without network success, which is what makes the history side
// effect observable hermetically.
const deadURL = "https://example.invalid/x"

// TestFacadeDownloadWritesCLIHistoryByDefault anchors the documented side effect:
// a facade Download appends to the CLI history file. The CLI depends on this, so
// the zero value of Config.DisableHistory must keep writing.
func TestFacadeDownloadWritesCLIHistoryByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	t.Setenv("PROVENANCE_HISTORY_FILE", path)

	eng, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, err := eng.Download(context.Background(), DownloadRequest{URL: deadURL}, NopSink()); err == nil {
		t.Fatal("the dead fixture URL must fail; a run that did not execute proves nothing")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("history file was not written by a default facade call: %v", err)
	}
	if !strings.Contains(string(raw), deadURL) {
		t.Fatalf("history file does not record the run URL:\n%s", raw)
	}
}

// TestFacadeDisableHistoryLeavesHistoryFileUntouched is the Phase 1 adapter gate.
// An embedded caller (the desktop shell, the headless agent) performs a real
// download on the CLI user's behalf, so it must not append to that user's
// history file.
func TestFacadeDisableHistoryLeavesHistoryFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	t.Setenv("PROVENANCE_HISTORY_FILE", path)

	// Pre-seed the file through the default path so "untouched" means
	// byte-identical rather than merely "not created".
	writer, err := New(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new writer engine: %v", err)
	}
	_, _ = writer.Download(context.Background(), DownloadRequest{URL: deadURL}, NopSink())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pre-seed history: %v", err)
	}

	silent, err := New(Config{WorkDir: t.TempDir(), DisableHistory: true})
	if err != nil {
		t.Fatalf("new silent engine: %v", err)
	}
	if !silent.Config().DisableHistory {
		t.Fatal("Config() did not carry DisableHistory")
	}
	if _, err := silent.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/y"}, NopSink()); err == nil {
		t.Fatal("the dead fixture URL must fail; a run that did not execute proves nothing")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history after a DisableHistory run: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("history file changed despite DisableHistory:\nbefore=%s\nafter=%s", before, after)
	}

	// A path that did not exist before must not be created at all.
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	t.Setenv("PROVENANCE_HISTORY_FILE", fresh)
	if _, err := silent.Download(context.Background(), DownloadRequest{URL: "https://example.invalid/z"}, NopSink()); err == nil {
		t.Fatal("the dead fixture URL must fail; a run that did not execute proves nothing")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("history file was created despite DisableHistory: %v", err)
	}
}
