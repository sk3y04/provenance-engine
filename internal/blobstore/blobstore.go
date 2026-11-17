// Package blobstore provides SHA-256 content-addressed filesystem storage.
// Files are stored under <root>/blobs/sha256/<first-two-hex>/<full-hex>
// and are deduplicated: identical content (same hash) is stored only once.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrExists = errors.New("blob already exists")

type Store struct {
	Root string
}

func New(root string) *Store {
	return &Store{Root: root}
}

func (s *Store) blobPath(hash string) string {
	if len(hash) < 4 {
		return filepath.Join(s.Root, "blobs", "sha256", "xx", hash)
	}
	return filepath.Join(s.Root, "blobs", "sha256", hash[:2], hash)
}

func (s *Store) Put(srcPath string) (string, error) {
	hash, err := fileHash(srcPath)
	if err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}

	dst := s.blobPath(hash)
	if _, err := os.Stat(dst); err == nil {
		return hash, ErrExists
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("mkdir blob: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = src.Close() }()

	// A unique temp file per attempt: concurrent Puts of the same content
	// must never share (or delete) each other's in-progress file.
	tmpf, err := os.CreateTemp(filepath.Dir(dst), hash+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create blob: %w", err)
	}
	tmp := tmpf.Name()

	// Re-hash while copying so a source that changes between the initial
	// hash and the copy can never store content under the wrong address.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpf, h), src); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy blob: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close blob: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("source changed during put: expected hash %s, copied content hashes to %s", hash, got)
	}

	// If a concurrent Put already stored the same hash, the rename replaces
	// it with byte-identical content, which is harmless.
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename blob: %w", err)
	}

	return hash, nil
}

func (s *Store) Get(hash string) (*os.File, error) {
	path := s.blobPath(hash)
	return os.Open(path)
}

func (s *Store) Exists(hash string) bool {
	_, err := os.Stat(s.blobPath(hash))
	return err == nil
}

func (s *Store) Remove(hash string) error {
	return os.Remove(s.blobPath(hash))
}

func (s *Store) List() ([]string, error) {
	dir := filepath.Join(s.Root, "blobs", "sha256")
	var hashes []string
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 {
			continue
		}
		subPath := filepath.Join(dir, entry.Name())
		subEntries, err := os.ReadDir(subPath)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if sub.IsDir() || !isBlobName(sub.Name()) {
				continue
			}
			hashes = append(hashes, sub.Name())
		}
	}
	return hashes, nil
}

// isBlobName reports whether name is a lowercase sha256 hex digest, so List
// (and the garbage collector built on it) ignores in-progress temp files and
// other stray entries.
func isBlobName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
