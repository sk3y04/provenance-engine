package dispatcher

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSplitBatchURLs pins the fast/slow routing: native-extractor hosts must
// take the full Dispatch path, generic/patreon hosts the yt-dlp fast path,
// and unclassifiable URLs the slow path (so the error is typed there).
func TestSplitBatchURLs(t *testing.T) {
	urls := []string{
		"https://x.com/user/status/1",
		"https://twitter.com/user/status/2",
		"https://www.reddit.com/r/pics/comments/abc/",
		"https://www.instagram.com/p/abc/",
		"https://example.com/video.mp4",
		"https://www.patreon.com/posts/123",
		"http://[::1", // unparseable -> slow path types the error
	}
	fast, slow := splitBatchURLs(urls)
	wantFast := []string{"https://example.com/video.mp4", "https://www.patreon.com/posts/123"}
	wantSlow := []string{
		"https://x.com/user/status/1",
		"https://twitter.com/user/status/2",
		"https://www.reddit.com/r/pics/comments/abc/",
		"https://www.instagram.com/p/abc/",
		"http://[::1",
	}
	if !reflect.DeepEqual(fast, wantFast) {
		t.Errorf("fast = %v, want %v", fast, wantFast)
	}
	if !reflect.DeepEqual(slow, wantSlow) {
		t.Errorf("slow = %v, want %v", slow, wantSlow)
	}
}

func TestBatchDispatchRejectsEmptyAndCommentOnlyFiles(t *testing.T) {
	for name, content := range map[string]string{
		"empty":   "",
		"comment": "# only a comment\n\n   \n",
	} {
		path := filepath.Join(t.TempDir(), name+".txt")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := BatchDispatch(t.Context(), path, Options{})
		if err == nil || !strings.Contains(err.Error(), "no URLs") {
			t.Errorf("%s file: err = %v, want 'no URLs' error", name, err)
		}
	}
}

// TestURLArchiveRoundTrip proves the on-disk archive survives close/reopen,
// keeps the latest status per URL, treats perm-fail as retryable, and stays
// compatible with bare-URL legacy lines.
func TestURLArchiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.txt")
	legacy := "# comment\nhttps://legacy.example/old\nnot-a-url-line\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openURLArchive(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !a.hasSuccessful("https://legacy.example/old") {
		t.Error("bare legacy URL must count as successful")
	}
	if a.hasSuccessful("not-a-url-line") {
		t.Error("garbage line must be ignored")
	}
	a.recordStatus("https://x/1", archiveStatusOK)
	a.recordStatus("https://x/2", archiveStatusPermFail)
	a.close()

	b, err := openURLArchive(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.close()
	if !b.hasSuccessful("https://x/1") {
		t.Error("ok status lost across reopen")
	}
	if b.hasSuccessful("https://x/2") {
		t.Error("perm-fail must stay retryable (not successful)")
	}
	// Latest status wins: a later success overrides perm-fail.
	b.recordStatus("https://x/2", archiveStatusOK)
	if !b.hasSuccessful("https://x/2") {
		t.Error("later ok status must override perm-fail")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ok:https://x/1", "perm-fail:https://x/2", "ok:https://x/2"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("archive file missing %q:\n%s", want, data)
		}
	}
}
