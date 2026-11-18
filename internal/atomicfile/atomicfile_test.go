package atomicfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileContentPermAndNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "store.json")
	if err := WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("content = %q (err=%v)", data, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// TestWriteFileConcurrentWritersNeverCorrupt: many writers sharing one
// destination must never expose a partial or mixed file to readers. A fixed
// shared temp path (the old store pattern) lets one writer rename another
// writer's half-written temp file; unique temp paths make that impossible.
func TestWriteFileConcurrentWritersNeverCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := WriteFile(path, []byte(`{"writer":-1,"seq":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("read during writes: %v", err)
				return
			}
			var v struct {
				Writer int `json:"writer"`
				Seq    int `json:"seq"`
			}
			if err := json.Unmarshal(data, &v); err != nil {
				t.Errorf("corrupt file observed: %v (%q)", err, data)
				return
			}
		}
	}()

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for seq := 0; seq < 40; seq++ {
				data := []byte(fmt.Sprintf(`{"writer":%d,"seq":%d}`, w, seq))
				if err := WriteFile(path, data, 0o600); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-readerDone

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Writer int `json:"writer"`
		Seq    int `json:"seq"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("final file corrupt: %v", err)
	}
	if v.Writer < 0 || v.Writer > 7 || v.Seq != 39 {
		t.Errorf("final file = %+v, want a complete write from one writer", v)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}
