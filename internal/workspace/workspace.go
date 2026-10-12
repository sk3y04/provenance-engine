// Package workspace gives an operation its own output root and a record of what
// already existed before the operation started, so a long-lived engine can
// report only the artifacts that the operation itself produced.
//
// A shared work directory is unsafe for concurrent jobs: an operation that
// simply walks the directory after execution would report files created by
// another job, or by an earlier job. Callers should therefore construct one
// engine and one workspace per job, and use Isolated when the work directory may
// be reused.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BookkeepingDirs are directories the engine creates for its own use and never
// reports as artifacts.
var BookkeepingDirs = map[string]bool{
	"_provenance_cache": true,
	".provenance":       true,
}

// partialSuffixes are in-progress files that are not yet artifacts.
var partialSuffixes = []string{".part", ".ytdl"}

// Entry is a file created by the operation.
type Entry struct {
	Path string
	Name string // slash-separated path relative to the workspace root
	Size int64
}

// Workspace is an operation-scoped output root.
type Workspace struct {
	Root     string
	OpID     string
	isolated bool
	before   map[string]int64
}

// New prepares an operation workspace under root. When isolated is true the
// operation gets its own subdirectory, which is the only arrangement that can
// separate two concurrent jobs sharing a parent directory. New creates the
// directory and records the files that already exist.
func New(root string, isolated bool) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	opID, err := newOpID()
	if err != nil {
		return nil, err
	}
	if isolated {
		root = filepath.Join(root, opID)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	w := &Workspace{Root: root, OpID: opID, isolated: isolated, before: map[string]int64{}}
	if err := w.snapshot(w.before); err != nil {
		return nil, err
	}
	return w, nil
}

// Isolated reports whether the operation owns its own subdirectory.
func (w *Workspace) Isolated() bool { return w.isolated }

// Created returns the files that did not exist when the workspace was opened,
// excluding engine bookkeeping directories and in-progress partial files.
func (w *Workspace) Created() ([]Entry, error) {
	now := map[string]int64{}
	if err := w.snapshot(now); err != nil {
		return nil, err
	}
	var out []Entry
	for path, size := range now {
		if _, existed := w.before[path]; existed {
			continue
		}
		rel, err := filepath.Rel(w.Root, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		out = append(out, Entry{Path: path, Name: filepath.ToSlash(rel), Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (w *Workspace) snapshot(into map[string]int64) error {
	return filepath.WalkDir(w.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != w.Root && BookkeepingDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		for _, suffix := range partialSuffixes {
			if strings.HasSuffix(name, suffix) {
				return nil
			}
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		into[path] = info.Size()
		return nil
	})
}

func newOpID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
