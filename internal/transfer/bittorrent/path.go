package bittorrent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ErrUnsafePath marks every rejection caused by a hostile or colliding path in
// torrent metadata. Callers can test for it with errors.Is.
var ErrUnsafePath = errors.New("unsafe torrent path")

const (
	// maxComponentLen is the limit most filesystems impose on one name.
	maxComponentLen = 255
	// maxRelPathLen bounds the whole relative path so a crafted metainfo
	// cannot build a multi-megabyte path string.
	maxRelPathLen = 4096
)

// windowsReserved are the DOS device names that are unusable (or alias a
// device) on Windows. A torrent that carries one would fail to extract there,
// and on some systems would write somewhere the operator did not intend.
var windowsReserved = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {},
	"com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {},
	"lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// safeComponent validates one path component from untrusted metainfo.
//
// Metainfo paths are attacker-controlled: a peer can hand us any bytes for a
// magnet's metadata. Everything that could escape the staging root, alias
// another file, or fail on a supported platform is rejected here rather than
// sanitised, so a hostile torrent is refused whole instead of silently
// landing somewhere unexpected.
func safeComponent(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty path component", ErrUnsafePath)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is a traversal component", ErrUnsafePath, name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrUnsafePath, name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrUnsafePath, name)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: %q is not valid UTF-8", ErrUnsafePath, name)
	case len(name) > maxComponentLen:
		return fmt.Errorf("%w: component longer than %d bytes", ErrUnsafePath, maxComponentLen)
	case strings.HasSuffix(name, ":"):
		// "C:" and friends name a drive on Windows; filepath.Join would
		// produce "C:evil" which Windows reads as drive-relative.
		return fmt.Errorf("%w: %q looks like a drive letter", ErrUnsafePath, name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		// Windows silently strips these, which collides two distinct
		// metainfo entries onto one file.
		return fmt.Errorf("%w: %q ends in a dot or space", ErrUnsafePath, name)
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if _, reserved := windowsReserved[strings.ToLower(base)]; reserved {
		return fmt.Errorf("%w: %q is a reserved device name", ErrUnsafePath, name)
	}
	return nil
}

// relPath validates a torrent name plus a file path and returns the
// slash-separated path relative to the staging root.
func relPath(name string, path []string) (string, error) {
	if err := safeComponent(name); err != nil {
		return "", err
	}
	if len(path) == 0 {
		return "", fmt.Errorf("%w: file has no path", ErrUnsafePath)
	}
	parts := make([]string, 0, len(path)+1)
	parts = append(parts, name)
	for _, comp := range path {
		if err := safeComponent(comp); err != nil {
			return "", err
		}
		parts = append(parts, comp)
	}
	rel := strings.Join(parts, "/")
	if len(rel) > maxRelPathLen {
		return "", fmt.Errorf("%w: path longer than %d bytes", ErrUnsafePath, maxRelPathLen)
	}
	// Belt and braces: the joined form must still be a non-empty relative
	// path that filepath.Clean cannot shorten into a traversal.
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean != rel || clean == "" || clean == "." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q does not clean to itself", ErrUnsafePath, rel)
	}
	return rel, nil
}

// withinRoot reports whether target is root or a descendant of it. Both paths
// must already be absolute and clean.
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// joinWithin returns filepath.Join(root, rel) after proving the result stays
// inside root.
func joinWithin(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !withinRoot(root, full) {
		return "", fmt.Errorf("%w: %q escapes the staging root", ErrUnsafePath, rel)
	}
	return full, nil
}

// checkNoSymlink rejects a relative path whose existing components include a
// symbolic link. Staging is ours, but a previous job, a sync tool, or a local
// attacker can leave a link in it, and following one would write or read
// outside the root even though the path looks relative.
func checkNoSymlink(root, rel string) error {
	cur := root
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Nothing below this point exists yet, so there is nothing
				// to follow.
				return nil
			}
			return fmt.Errorf("inspect %s: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q is a symbolic link", ErrUnsafePath, cur)
		}
	}
	return nil
}

// resolveCaptured returns the fully resolved path of a captured file and
// proves that resolution did not leave root (a symlinked parent directory, for
// example). It is the last check before bytes are read for hashing.
func resolveCaptured(root, rel string) (string, error) {
	full, err := joinWithin(root, rel)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve staging root: %w", err)
	}
	resolvedFile, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", rel, err)
	}
	if !withinRoot(resolvedRoot, resolvedFile) {
		return "", fmt.Errorf("%w: %q resolves outside the staging root", ErrUnsafePath, rel)
	}
	return resolvedFile, nil
}
