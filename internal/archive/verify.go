package archive

// Read-only vault verification.
//
// Verify walks committed revision manifests and hashes every referenced blob
// once. It never repairs, rewrites or deletes anything: a missing or corrupt
// blob is reported and the caller decides what to do. A blob referenced by
// several revisions is hashed once and reported with its reference count.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sk3y04/provenance-engine/internal/blobstore"
)

// VerifyStatus is the outcome for one revision manifest or one blob.
type VerifyStatus string

const (
	VerifyOK         VerifyStatus = "ok"
	VerifyMissing    VerifyStatus = "missing"
	VerifyMismatch   VerifyStatus = "mismatch"
	VerifyUnreadable VerifyStatus = "unreadable"
	VerifyInvalid    VerifyStatus = "invalid"
)

// BlobResult is one unique blob check.
type BlobResult struct {
	Sha256 string       `json:"sha256"`
	Status VerifyStatus `json:"status"`
	// References is how many revision/artifact entries name this blob.
	References int `json:"references"`
	// ExpectedSize is the size recorded by the revision manifest, when present.
	ExpectedSize int64 `json:"expected_size,omitempty"`
	ActualSize   int64 `json:"actual_size,omitempty"`
	// ActualSha256 is set only when the bytes hashed to a different digest.
	ActualSha256 string `json:"actual_sha256,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ManifestResult is one revision-level finding that is not a blob finding.
type ManifestResult struct {
	RevisionID string       `json:"revision_id,omitempty"`
	Status     VerifyStatus `json:"status"`
	Error      string       `json:"error,omitempty"`
}

// VerifyReport is the complete read-only result.
type VerifyReport struct {
	Revisions    int              `json:"revisions"`
	UniqueBlobs  int              `json:"unique_blobs"`
	CheckedBlobs int              `json:"checked_blobs"`
	OK           int              `json:"ok"`
	Missing      int              `json:"missing"`
	Mismatch     int              `json:"mismatch"`
	Unreadable   int              `json:"unreadable"`
	Invalid      int              `json:"invalid"`
	Healthy      bool             `json:"healthy"`
	Blobs        []BlobResult     `json:"blobs,omitempty"`
	Manifests    []ManifestResult `json:"manifests,omitempty"`
}

type blobRef struct {
	sha       string
	sizes     map[int64]bool
	reference int
}

// VerifyVault checks every revision manifest under vaultRoot/revisions and
// every blob those manifests reference. The returned report is deterministic:
// revisions are walked in sorted order and blobs are reported in SHA order.
func VerifyVault(vaultRoot string) (*VerifyReport, error) {
	root := strings.TrimSpace(vaultRoot)
	if root == "" {
		return nil, fmt.Errorf("vault root is required")
	}
	report := &VerifyReport{Healthy: true}
	revRoot := filepath.Join(root, "revisions")
	entries, err := os.ReadDir(revRoot)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read revisions: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	refs := make(map[string]*blobRef)
	for _, id := range names {
		manifestPath := filepath.Join(revRoot, id, "manifest.json")
		if !validRevisionID(id) {
			report.Manifests = append(report.Manifests, ManifestResult{
				RevisionID: id,
				Status:     VerifyInvalid,
				Error:      "revision directory name is not a sha256 digest",
			})
			report.Invalid++
			report.Healthy = false
			continue
		}
		data, rerr := os.ReadFile(manifestPath)
		if rerr != nil {
			report.Manifests = append(report.Manifests, ManifestResult{
				RevisionID: id,
				Status:     VerifyUnreadable,
				Error:      "revision manifest is not readable",
			})
			report.Unreadable++
			report.Healthy = false
			continue
		}
		var rev Revision
		if uerr := json.Unmarshal(data, &rev); uerr != nil {
			report.Manifests = append(report.Manifests, ManifestResult{
				RevisionID: id,
				Status:     VerifyUnreadable,
				Error:      "revision manifest is not valid JSON",
			})
			report.Unreadable++
			report.Healthy = false
			continue
		}
		if rev.ID != "" && rev.ID != id {
			report.Manifests = append(report.Manifests, ManifestResult{
				RevisionID: id,
				Status:     VerifyInvalid,
				Error:      fmt.Sprintf("manifest id %s does not match its directory", rev.ID),
			})
			report.Invalid++
			report.Healthy = false
			continue
		}
		report.Revisions++
		inRevision := make(map[string]bool)

		for _, a := range rev.Artifacts {
			sha := strings.ToLower(strings.TrimSpace(a.Sha256))
			if !validBlobHash(sha) {
				report.Manifests = append(report.Manifests, ManifestResult{
					RevisionID: id,
					Status:     VerifyInvalid,
					Error:      "artifact entry has no valid sha256",
				})
				report.Invalid++
				report.Healthy = false
				continue
			}
			ref := refs[sha]
			if ref == nil {
				ref = &blobRef{sha: sha, sizes: make(map[int64]bool)}
				refs[sha] = ref
			}
			ref.reference++
			inRevision[sha] = true
			if a.Size > 0 {
				ref.sizes[a.Size] = true
			}
		}
		for _, ent := range rev.Entities {
			for _, sha := range ent.Artifacts {
				sha = strings.ToLower(strings.TrimSpace(sha))
				if inRevision[sha] {
					continue
				}
				if !validBlobHash(sha) {
					report.Manifests = append(report.Manifests, ManifestResult{
						RevisionID: id,
						Status:     VerifyInvalid,
						Error:      "entity references no valid sha256",
					})
					report.Invalid++
					report.Healthy = false
					continue
				}
				ref := refs[sha]
				if ref == nil {
					ref = &blobRef{sha: sha, sizes: make(map[int64]bool)}
					refs[sha] = ref
				}
				ref.reference++
				inRevision[sha] = true
			}
		}
	}

	shas := make([]string, 0, len(refs))
	for sha := range refs {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	report.UniqueBlobs = len(shas)
	bs := blobstore.New(root)
	for _, sha := range shas {
		ref := refs[sha]
		res := BlobResult{Sha256: sha, References: ref.reference}
		for size := range ref.sizes {
			if size > res.ExpectedSize {
				res.ExpectedSize = size
			}
		}
		actual, size, herr := hashBlob(bs, sha)
		switch {
		case os.IsNotExist(herr):
			res.Status = VerifyMissing
			res.Error = "blob is missing"
			report.Missing++
			report.Healthy = false
		case herr != nil:
			res.Status = VerifyUnreadable
			res.Error = "blob is not readable"
			report.Unreadable++
			report.Healthy = false
		default:
			res.ActualSize = size
			report.CheckedBlobs++
			switch {
			case actual != sha:
				res.Status = VerifyMismatch
				res.ActualSha256 = actual
				res.Error = "blob bytes do not match their address"
				report.Mismatch++
				report.Healthy = false
			case res.ExpectedSize > 0 && !ref.sizes[size]:
				res.Status = VerifyMismatch
				res.Error = fmt.Sprintf("blob size is %d, which no manifest records", size)
				report.Mismatch++
				report.Healthy = false
			default:
				res.Status = VerifyOK
				report.OK++
			}
		}
		report.Blobs = append(report.Blobs, res)
	}

	// Manifest-level findings were appended in walk order, which is already
	// sorted by revision id. Keep them stable for JSON consumers.
	if report.Invalid > 0 || report.Unreadable > 0 {
		report.Healthy = false
	}
	return report, nil
}

// hashBlob reads the blob once and returns its digest and size. Missing files
// are reported with os.ErrNotExist so the caller can distinguish absence from
// an unreadable blob.
func hashBlob(bs *blobstore.Store, sha string) (string, int64, error) {
	f, err := bs.Get(sha)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// validBlobHash is a path-safety check as well as a format check: blobstore
// joins the digest into a path, so anything but a lowercase sha256 is refused
// before any filesystem access.
func validBlobHash(sha string) bool {
	if len(sha) != sha256.Size*2 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
