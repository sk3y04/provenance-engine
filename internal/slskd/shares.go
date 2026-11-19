package slskd

// Curated share management.
//
// slskd has no REST CRUD for share paths: shares live in slskd.yml, discovered
// on a scan. Provenance therefore materialises the content it is allowed to
// share into one isolated staging root (mounted :ro into slskd) and only asks
// slskd to rescan.
//
// Hardlinks only, never symlinks. The 0.26.x share scanner follows symlinks and
// indexes their targets: with the share root mounted read-only, symlinks to
// /etc/hostname and /etc/shadow were both scanned and advertised to the network
// (docs/findings-slskd-0.26-contract.md §6). Same-filesystem hardlinks are
// indexed as ordinary files, which is exactly what this design needs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sk3y04/provenance-engine/internal/atomicfile"
	"github.com/sk3y04/provenance-engine/internal/filelock"
)

// ShareFormat identifies the share-rule record.
const ShareFormat = "provenance/slskd-share+1"

// DefaultDenyPatterns is the hardened denylist applied before anything is linked
// into the share root. It is deliberately broad: sharing a private key, a
// database or an editor config by accident is not recoverable.
var DefaultDenyPatterns = []string{
	`(^|/|\\)\.`, // dotfiles and dot-directories
	`(?i)\.ini$`,
	`(?i)thumbs\.db$`,
	`(?i)\.ds_store$`,
	`(?i)\.(db|sqlite|sqlite3|env|key|pem|yml|yaml|json|pdf|docx|xlsx)$`,
	`(?i)(^|/|\\)(id_rsa|id_ed25519|\.netrc|\.git-credentials)$`,
}

// DefaultDeny compiles the default denylist.
func DefaultDeny() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(DefaultDenyPatterns))
	for _, p := range DefaultDenyPatterns {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

// ShareRule is one opted-in collection materialised into the share root.
type ShareRule struct {
	Format       string    `json:"format"`
	ID           string    `json:"id"`
	SourcePath   string    `json:"sourcePath"`
	AliasFolder  string    `json:"aliasFolder"`
	Enabled      bool      `json:"enabled"`
	FileCount    int       `json:"fileCount"`
	TotalBytes   int64     `json:"totalBytes"`
	Excluded     int       `json:"excluded"`
	LastSyncedAt time.Time `json:"lastSyncedAt"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// ShareStore persists share rules under one root.
type ShareStore struct {
	root string
	now  func() time.Time
}

const sharesDirName = ".provenance/slskd/shares"

// OpenShareStore opens (creating if needed) the share-rule store under root.
func OpenShareStore(root string) (*ShareStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("share store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(sharesDirName)), 0o755); err != nil {
		return nil, err
	}
	return &ShareStore{root: abs, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *ShareStore) dir() string { return filepath.Join(s.root, filepath.FromSlash(sharesDirName)) }

func (s *ShareStore) lock() (*filelock.Lock, error) {
	path := filepath.Join(s.root, filepath.FromSlash(".provenance/slskd/shares.lock"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return filelock.Acquire(context.Background(), path)
}

// List returns every rule, sorted by alias.
func (s *ShareStore) List() ([]ShareRule, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]ShareRule, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var rule ShareRule
		if err := json.Unmarshal(data, &rule); err != nil {
			return nil, fmt.Errorf("parse share rule %s: %w", e.Name(), err)
		}
		if rule.Format != "" && rule.Format != ShareFormat {
			return nil, fmt.Errorf("share rule %s has unsupported format %q", e.Name(), rule.Format)
		}
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AliasFolder < out[j].AliasFolder })
	return out, nil
}

// Get returns one rule by id.
func (s *ShareStore) Get(id string) (ShareRule, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(), sanitizeShareID(id)+".json"))
	if err != nil {
		return ShareRule{}, err
	}
	var rule ShareRule
	if err := json.Unmarshal(data, &rule); err != nil {
		return ShareRule{}, err
	}
	return rule, nil
}

// Put stores a rule.
func (s *ShareStore) Put(rule ShareRule) (ShareRule, error) {
	if strings.TrimSpace(rule.ID) == "" || !validShareID(rule.ID) {
		return ShareRule{}, fmt.Errorf("invalid share rule id %q", rule.ID)
	}
	lk, err := s.lock()
	if err != nil {
		return ShareRule{}, err
	}
	defer func() { _ = lk.Release() }()

	now := s.now()
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = now
	}
	rule.UpdatedAt = now
	rule.Format = ShareFormat
	data, err := json.MarshalIndent(rule, "", "  ")
	if err != nil {
		return ShareRule{}, err
	}
	if err := atomicfile.WriteFile(filepath.Join(s.dir(), rule.ID+".json"), append(data, '\n'), 0o600); err != nil {
		return ShareRule{}, err
	}
	return rule, nil
}

// Delete removes a rule record.
func (s *ShareStore) Delete(id string) error {
	lk, err := s.lock()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Release() }()
	return os.Remove(filepath.Join(s.dir(), sanitizeShareID(id)+".json"))
}

var shareIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func validShareID(id string) bool { return shareIDPattern.MatchString(id) }

func sanitizeShareID(id string) string {
	lower := strings.ToLower(strings.TrimSpace(id))
	var b strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ShareFile is one file the preview would expose.
type ShareFile struct {
	RelPath string
	Size    int64
}

// Preview is the pre-share confirmation: exact counts, bytes and what was
// excluded, before anything is linked or scanned.
type Preview struct {
	SourcePath string
	Files      []ShareFile
	TotalBytes int64
	Excluded   int
	Symlinks   int
	Extensions map[string]int
	Sample     []string
}

// ShareManager owns the curated share root.
type ShareManager struct {
	// Root is the curated share root (mounted :ro into slskd).
	Root string
	// Store persists the opt-in rules; optional for a pure preview.
	Store *ShareStore
	// Client performs the rescan request; optional for a pure preview.
	Client *Client
	// Deny overrides the default denylist.
	Deny []*regexp.Regexp
	// MaxFiles bounds one share; 0 means the default cap.
	MaxFiles int
	// link is a seam for tests (EXDEV is otherwise unreachable in a unit test).
	link func(oldname, newname string) error
	now  func() time.Time
}

func (m *ShareManager) deny() []*regexp.Regexp {
	if m.Deny != nil {
		return m.Deny
	}
	return DefaultDeny()
}

func (m *ShareManager) linker() func(string, string) error {
	if m.link != nil {
		return m.link
	}
	return os.Link
}

// Preview walks source and reports exactly what would be shared.
func (m *ShareManager) Preview(source string) (Preview, error) {
	src, err := filepath.Abs(source)
	if err != nil {
		return Preview{}, err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return Preview{}, fmt.Errorf("share source %s is not accessible: %w", source, err)
	}
	if !fi.IsDir() {
		return Preview{}, fmt.Errorf("share source %s is not a directory", source)
	}
	preview := Preview{SourcePath: src, Extensions: map[string]int{}}
	maxFiles := m.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 100000
	}
	deny := m.deny()

	err = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Never followed: a symlink can point outside the source tree, and
			// slskd would happily index the target.
			preview.Symlinks++
			preview.Excluded++
			if len(preview.Sample) < 10 {
				preview.Sample = append(preview.Sample, "symlink: "+filepath.ToSlash(rel))
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			preview.Excluded++
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		if denied(deny, slashRel) {
			preview.Excluded++
			if len(preview.Sample) < 10 {
				preview.Sample = append(preview.Sample, "denied: "+slashRel)
			}
			return nil
		}
		preview.Files = append(preview.Files, ShareFile{RelPath: slashRel, Size: info.Size()})
		preview.TotalBytes += info.Size()
		ext := strings.ToLower(filepath.Ext(path))
		if ext == "" {
			ext = "(none)"
		}
		preview.Extensions[ext]++
		if len(preview.Files) > maxFiles {
			return fmt.Errorf("share source %s has more than %d files; share a narrower folder", source, maxFiles)
		}
		return nil
	})
	if err != nil {
		return Preview{}, err
	}
	sort.Slice(preview.Files, func(i, j int) bool { return preview.Files[i].RelPath < preview.Files[j].RelPath })
	return preview, nil
}

func denied(patterns []*regexp.Regexp, rel string) bool {
	for _, p := range patterns {
		if p.MatchString(rel) {
			return true
		}
	}
	return false
}

// Add materialises a preview into the share root as hardlinks and records the
// rule. The scan request is issued last: slskd must never advertise links that
// do not exist yet.
func (m *ShareManager) Add(ctx context.Context, source, alias string) (ShareRule, Preview, error) {
	preview, err := m.Preview(source)
	if err != nil {
		return ShareRule{}, Preview{}, err
	}
	if len(preview.Files) == 0 {
		return ShareRule{}, preview, errors.New("nothing to share: every file was excluded")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		alias = filepath.Base(preview.SourcePath)
	}
	if strings.ContainsAny(alias, `/\`) || alias == "." || alias == ".." {
		return ShareRule{}, preview, fmt.Errorf("alias %q must be a single path element", alias)
	}
	if strings.TrimSpace(m.Root) == "" {
		return ShareRule{}, preview, errors.New("curated share root is not configured")
	}
	root, err := filepath.Abs(m.Root)
	if err != nil {
		return ShareRule{}, preview, err
	}
	// Linking into the source tree would create a share that includes itself.
	if inside(preview.SourcePath, root) || inside(root, preview.SourcePath) {
		return ShareRule{}, preview, fmt.Errorf("the curated share root %s and the share source %s must not contain each other", root, preview.SourcePath)
	}

	dest := filepath.Join(root, alias)
	linked := 0
	for _, f := range preview.Files {
		srcPath := filepath.Join(preview.SourcePath, filepath.FromSlash(f.RelPath))
		destPath := filepath.Join(dest, filepath.FromSlash(f.RelPath))
		if _, err := ensurePathIsInside(dest, destPath); err != nil {
			return ShareRule{}, preview, err
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return ShareRule{}, preview, fmt.Errorf("create share directory: %w", err)
		}
		if err := m.linker()(srcPath, destPath); err != nil {
			if os.IsExist(err) {
				linked++
				continue
			}
			if isCrossDevice(err) {
				return ShareRule{}, preview, fmt.Errorf(
					"cannot hardlink %s into %s: the two paths are on different filesystems. "+
						"Put the curated share root and the provenance media root on the same volume, or share from a path on the share root's volume",
					preview.SourcePath, root)
			}
			return ShareRule{}, preview, fmt.Errorf("link %s: %w", srcPath, err)
		}
		linked++
	}
	if err := m.auditTree(dest); err != nil {
		return ShareRule{}, preview, err
	}

	rule := ShareRule{
		ID:          sanitizeShareID(alias),
		SourcePath:  preview.SourcePath,
		AliasFolder: alias,
		Enabled:     true,
		FileCount:   linked,
		TotalBytes:  preview.TotalBytes,
		Excluded:    preview.Excluded,
	}
	if m.Store != nil {
		rule, err = m.Store.Put(rule)
		if err != nil {
			return ShareRule{}, preview, err
		}
	}
	if m.now != nil {
		rule.LastSyncedAt = m.now()
	} else {
		rule.LastSyncedAt = time.Now().UTC()
	}
	if m.Client != nil {
		if err := m.Client.RescanShares(ctx); err != nil && !IsStatus(err, http.StatusConflict) {
			return rule, preview, fmt.Errorf("links created but slskd rescan failed: %w", err)
		}
	}
	return rule, preview, nil
}

// Remove deletes the materialised links and the rule, then rescans.
func (m *ShareManager) Remove(ctx context.Context, id string) (ShareRule, error) {
	var rule ShareRule
	var err error
	if m.Store != nil {
		rule, err = m.Store.Get(id)
		if err != nil {
			return ShareRule{}, err
		}
	} else {
		rule = ShareRule{ID: id, AliasFolder: id}
	}
	if strings.TrimSpace(m.Root) == "" {
		return rule, errors.New("curated share root is not configured")
	}
	dest := filepath.Join(m.Root, rule.AliasFolder)
	if _, err := ensurePathIsInside(m.Root, dest); err != nil {
		return rule, err
	}
	// Remove the links themselves (Lstat semantics), never their targets.
	if err := os.RemoveAll(dest); err != nil {
		return rule, fmt.Errorf("remove share links: %w", err)
	}
	if m.Store != nil {
		if err := m.Store.Delete(id); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rule, err
		}
	}
	if m.Client != nil {
		if err := m.Client.RescanShares(ctx); err != nil && !IsStatus(err, http.StatusConflict) {
			return rule, fmt.Errorf("links removed but slskd rescan failed: %w", err)
		}
	}
	return rule, nil
}

// AuditResult reports what an audit found.
type AuditResult struct {
	Files    int
	Bytes    int64
	Symlinks []string
}

// Audit walks the share root and refuses a tree containing symlinks: slskd would
// follow them out of the root.
func (m *ShareManager) Audit() (AuditResult, error) {
	if strings.TrimSpace(m.Root) == "" {
		return AuditResult{}, errors.New("curated share root is not configured")
	}
	return auditShareRoot(m.Root)
}

func (m *ShareManager) auditTree(dest string) error {
	res, err := auditShareRoot(dest)
	if err != nil {
		return err
	}
	if len(res.Symlinks) > 0 {
		return fmt.Errorf("the materialised share contains symlinks (%s); slskd follows them out of the share root, so the share is refused",
			strings.Join(res.Symlinks, ", "))
	}
	return nil
}

func auditShareRoot(root string) (AuditResult, error) {
	res := AuditResult{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			res.Symlinks = append(res.Symlinks, path)
			return nil
		}
		if info.Mode().IsRegular() {
			res.Files++
			res.Bytes += info.Size()
		}
		return nil
	})
	return res, err
}

func isCrossDevice(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid cross-device link")
}

func inside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// BrowsePage is one page of the shared tree, fetched from slskd and paginated
// here so a 100k-file share never reaches a terminal in one piece.
type BrowsePage struct {
	TotalFiles int
	TotalBytes int64
	Entries    []string
	Page       int
	PageSize   int
	Pages      int
}

// Browse lists slskd's indexed share contents with a server-side cache, page and
// optional substring filter.
func (m *ShareManager) Browse(ctx context.Context, page, pageSize int, filter string) (BrowsePage, error) {
	if m.Client == nil {
		return BrowsePage{}, errors.New("no slskd client")
	}
	dirs, err := m.Client.ShareContents(ctx)
	if err != nil {
		return BrowsePage{}, err
	}
	type entry struct {
		name string
		size int64
	}
	var entries []entry
	filter = strings.ToLower(strings.TrimSpace(filter))
	var total int64
	for _, d := range dirs {
		for _, f := range d.Files {
			total += f.Size
			name := strings.ReplaceAll(d.Name, `\`, "/") + "/" + f.Filename
			if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
				continue
			}
			entries = append(entries, entry{name: name, size: f.Size})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	if pageSize <= 0 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	}
	pages := (len(entries) + pageSize - 1) / pageSize
	start := (page - 1) * pageSize
	if start > len(entries) {
		start = len(entries)
	}
	end := start + pageSize
	if end > len(entries) {
		end = len(entries)
	}
	out := BrowsePage{TotalFiles: len(entries), TotalBytes: total, Page: page, PageSize: pageSize, Pages: pages}
	for _, e := range entries[start:end] {
		out.Entries = append(out.Entries, fmt.Sprintf("%s  (%d bytes)", e.name, e.size))
	}
	return out, nil
}

// ShareWarningContext is the state needed to spot a silent share misconfiguration.
type ShareWarningContext struct {
	EnabledRules int
	ShareFiles   int
	// OptionsReadable is false when slskd refused /options (a readwrite key on
	// 0.26.2), in which case no option-based diagnosis is possible.
	OptionsReadable bool
	NoShareScan     bool
}

// MisconfigurationWarnings reports the case where rules exist but slskd shares
// nothing, which is what a no_share_scan or a failed scan looks like from here.
func MisconfigurationWarnings(ctx ShareWarningContext) []Warning {
	if ctx.EnabledRules == 0 || ctx.ShareFiles > 0 {
		return nil
	}
	msg := "share rules are active but slskd reports zero shared files: the share root is empty, excluded, or the scan never ran"
	if ctx.OptionsReadable && ctx.NoShareScan {
		msg = "share rules are active but slskd reports zero shared files and flags.no_share_scan is set, so the share index is never rebuilt (#1663). Set no_share_scan: false, or rescan manually."
	}
	return []Warning{{Code: "#1663", Message: msg}}
}

// ShareStatus summarises one share as slskd reports it.
type ShareStatus struct {
	Alias       string
	RemotePath  string
	LocalPath   string
	IsExcluded  bool
	Files       int
	Directories int
}

// Status returns slskd's view of the configured shares.
func (m *ShareManager) Status(ctx context.Context) ([]ShareStatus, error) {
	if m.Client == nil {
		return nil, errors.New("no slskd client")
	}
	shares, err := m.Client.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	var out []ShareStatus
	for _, list := range shares {
		for _, s := range list {
			out = append(out, ShareStatus{
				Alias: s.Alias, RemotePath: s.RemotePath, LocalPath: s.LocalPath,
				IsExcluded: s.IsExcluded, Files: s.Files, Directories: s.Directories,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// Rescan asks slskd to rebuild its share index.
func (m *ShareManager) Rescan(ctx context.Context) error {
	if m.Client == nil {
		return errors.New("no slskd client")
	}
	return m.Client.RescanShares(ctx)
}

// CancelScan aborts a running share scan.
func (m *ShareManager) CancelScan(ctx context.Context) error {
	if m.Client == nil {
		return errors.New("no slskd client")
	}
	return m.Client.CancelShareScan(ctx)
}
