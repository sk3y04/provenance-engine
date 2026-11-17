// Package importers provides technical knowledge importers for PDFs, Git repos,
// static documentation sites, and OpenAPI specifications into the archive vault.
package importers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
	"github.com/sk3y04/provenance-engine/internal/blobstore"
	"github.com/sk3y04/provenance-engine/internal/event"
	"github.com/sk3y04/provenance-engine/internal/redact"
)

// legacyPrefixPattern matches the tool prefix used by historical CLI narration,
// so the machine-readable Detail can be derived without the presentation text.
var legacyPrefixPattern = regexp.MustCompile(`^\[[^\]]+\] `)

// narratef emits a structured event when the operation carries an event sink on
// ctx, and otherwise preserves the legacy CLI narration byte-for-byte on stderr.
// fact is the stable, redactable value an event may carry; when empty it is
// derived from the legacy line with its tool prefix removed.
func narratef(ctx context.Context, kind event.Kind, reason, url, fact, legacyFormat string, args ...any) {
	line := fmt.Sprintf(legacyFormat, args...)
	detail := fact
	if detail == "" {
		detail = legacyPrefixPattern.ReplaceAllString(line, "")
	}
	if sink := event.SinkFrom(ctx); sink != nil {
		event.Emit(ctx, sink, event.Event{
			Kind:   kind,
			URL:    redact.URL(url),
			Reason: reason,
			Detail: redact.Detail(detail),
		})
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, legacyFormat, args...)
}

func newRevision(srcURL string, tool, toolVersion string) *archive.Revision {
	return &archive.Revision{
		CapturedAt: time.Now().UTC(),
		Source: archive.Source{
			URL:       srcURL,
			Kind:      archive.SourceImport,
			Reference: srcURL,
		},
		Tool:        tool,
		ToolVersion: toolVersion,
	}
}

func storeFile(bs *blobstore.Store, path string) (string, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("stat: %w", err)
	}
	hash, err := bs.Put(path)
	if err != nil && err != blobstore.ErrExists {
		return "", 0, fmt.Errorf("blob put: %w", err)
	}
	return hash, fi.Size(), nil
}

func persistRevision(vaultRoot string, rev *archive.Revision) error {
	revID, err := computeID(rev)
	if err != nil {
		return fmt.Errorf("compute revision id: %w", err)
	}
	rev.ID = revID
	return archive.WriteRevision(vaultRoot, rev)
}

func computeID(rev *archive.Revision) (string, error) {
	data, err := json.Marshal(rev)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

func safeTruncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	for i := maxLen; i > maxLen-4 && i > 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return s[:i]
		}
	}
	return s[:maxLen]
}
