package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

// Postgres integration tests run only when PROVENANCE_TEST_PG_URL points at a
// disposable database, e.g.:
//
//	docker run --rm -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=provit \
//	  -p 127.0.0.1::5432 --name prov-pg-it postgres:18.1-alpine
//	PROVENANCE_TEST_PG_URL="postgres://postgres:test@127.0.0.1:$(docker port \
//	  prov-pg-it 5432 | cut -d: -f2)/provit?sslmode=disable" \
//	  go test ./internal/catalog/ -run TestPg -v
//
// Without the env var they skip, keeping `make test` hermetic.

func pgTestStore(t *testing.T) (*PgStore, context.Context) {
	t.Helper()
	url := os.Getenv("PROVENANCE_TEST_PG_URL")
	if url == "" {
		t.Skip("PROVENANCE_TEST_PG_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	s, err := NewPgStore(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s, ctx
}

func pgTestCollection(t *testing.T, s *PgStore, ctx context.Context) string {
	t.Helper()
	name := fmt.Sprintf("it_%d", time.Now().UnixNano())
	if err := s.UpsertCollection(ctx, archive.ArchiveCollection{
		Name:      name,
		VaultRoot: t.TempDir(),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert collection: %v", err)
	}
	t.Cleanup(func() {
		_ = s.DeleteRevisionsByCollection(ctx, name)
		_, _ = s.pool.Exec(ctx, `DELETE FROM archive_collections WHERE name=$1`, name)
	})
	return name
}

func pgTestRevisionID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// TestPgRevisionRoundTrip proves InsertRevision and GetRevision preserve the
// whole revision: entities (with raw metadata and published_at), entity text,
// entity-level and revision-level documents, artifacts, and relations.
func TestPgRevisionRoundTrip(t *testing.T) {
	s, ctx := pgTestStore(t)
	coll := pgTestCollection(t, s, ctx)

	captured := time.Now().UTC().Truncate(time.Microsecond)
	published := captured.Add(-24 * time.Hour)
	rev := &archive.Revision{
		ID:          pgTestRevisionID(coll),
		CapturedAt:  captured,
		Source:      archive.Source{URL: "https://example.test/post", Kind: archive.SourceURL, Reference: "https://example.test/post"},
		Tool:        "provenance",
		ToolVersion: "test-1.0",
		Entities: []archive.Entity{
			{
				ExternalID:  "e1",
				URL:         "https://example.test/post",
				Title:       "Hello World",
				Author:      "alice",
				PublishedAt: &published,
				CapturedAt:  captured,
				Kind:        "post",
				Extractor:   "fixture",
				RawMetadata: json.RawMessage(`{"k":"v"}`),
				Text:        &archive.Document{ExternalID: "e1", Content: "the post body", Format: archive.DocMarkdown},
			},
			{
				ExternalID: "e2",
				URL:        "https://example.test/page",
				Title:      "Second Entity",
				CapturedAt: captured,
				Kind:       "page",
				Extractor:  "fixture",
				Documents:  []archive.Document{{ExternalID: "e2", Content: "imported document", Format: archive.DocPlain}},
				Relations:  []archive.Relation{{From: "e2", To: "e1", Kind: archive.RelReplyTo}},
			},
		},
		Artifacts: []archive.Artifact{
			{Sha256: pgTestRevisionID("artifact"), Path: "a.jpg", Size: 3, MimeType: "image/jpeg", Kind: archive.ArtifactImage},
		},
		Relations: []archive.Relation{
			{From: "e1", To: "e2", Kind: archive.RelContains},
		},
	}
	if err := s.InsertRevision(ctx, rev, coll); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetRevision(ctx, rev.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != rev.ID || got.Tool != rev.Tool || got.ToolVersion != rev.ToolVersion {
		t.Errorf("header mismatch: %+v", got)
	}
	if !got.CapturedAt.Equal(captured) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, captured)
	}
	if got.Source != rev.Source {
		t.Errorf("Source = %+v, want %+v", got.Source, rev.Source)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(got.Entities))
	}
	e1 := got.Entities[0]
	if e1.ExternalID != "e1" || e1.Title != "Hello World" || e1.Author != "alice" {
		t.Errorf("e1 mismatch: %+v", e1)
	}
	if e1.PublishedAt == nil || !e1.PublishedAt.Equal(published) {
		t.Errorf("e1 PublishedAt = %v, want %v", e1.PublishedAt, published)
	}
	var meta map[string]string
	if err := json.Unmarshal(e1.RawMetadata, &meta); err != nil || !reflect.DeepEqual(meta, map[string]string{"k": "v"}) {
		t.Errorf("e1 RawMetadata = %s (err=%v), want {k:v}", e1.RawMetadata, err)
	}
	if e1.Text == nil || e1.Text.Content != "the post body" || e1.Text.Format != archive.DocMarkdown {
		t.Errorf("e1.Text = %+v, want the post body (markdown)", e1.Text)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != rev.Artifacts[0] {
		t.Errorf("Artifacts = %+v, want %+v", got.Artifacts, rev.Artifacts)
	}
	if len(got.Documents) != 2 {
		t.Fatalf("Documents = %+v, want both entity texts", got.Documents)
	}
	wantRels := map[archive.Relation]bool{
		{From: "e1", To: "e2", Kind: archive.RelContains}: true,
		{From: "e2", To: "e1", Kind: archive.RelReplyTo}:  true,
	}
	if len(got.Relations) != len(wantRels) {
		t.Fatalf("Relations = %+v, want %d entries", got.Relations, len(wantRels))
	}
	for _, rel := range got.Relations {
		if !wantRels[rel] {
			t.Errorf("unexpected relation %+v", rel)
		}
	}

	ent, err := s.GetEntity(ctx, rev.ID, "e1")
	if err != nil || ent.Title != "Hello World" {
		t.Errorf("GetEntity = %+v (err=%v), want Hello World", ent, err)
	}
	revs, err := s.ListRevisions(ctx, coll)
	if err != nil || len(revs) != 1 || revs[0].ID != rev.ID {
		t.Errorf("ListRevisions = %+v (err=%v), want the revision", revs, err)
	}
	ids, err := s.ListRevisionIDsByCollection(ctx, coll)
	if err != nil || len(ids) != 1 || ids[0] != rev.ID {
		t.Errorf("ListRevisionIDsByCollection = %v (err=%v), want [%s]", ids, err, rev.ID)
	}

	res, err := s.Search(ctx, "hello", SearchOptions{CollectionName: coll})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total == 0 || res.Hits[0].RevisionID != rev.ID {
		t.Errorf("Search = %+v, want a hit on the revision", res)
	}

	if err := s.DeleteRevisionsByCollection(ctx, coll); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetRevision(ctx, rev.ID); err == nil {
		t.Error("GetRevision after delete must fail")
	}
}

// TestPgGetRevisionPropagatesArtifactErrors proves a broken artifacts query
// surfaces as an error instead of a silently partial revision.
func TestPgGetRevisionPropagatesArtifactErrors(t *testing.T) {
	s, ctx := pgTestStore(t)
	coll := pgTestCollection(t, s, ctx)
	rev := &archive.Revision{
		ID:         pgTestRevisionID(coll),
		CapturedAt: time.Now().UTC().Truncate(time.Microsecond),
		Source:     archive.Source{URL: "https://example.test/x", Kind: archive.SourceURL, Reference: "x"},
		Tool:       "provenance",
	}
	if err := s.InsertRevision(ctx, rev, coll); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE artifacts RENAME COLUMN sha256 TO sha256_backup`); err != nil {
		t.Fatalf("break artifacts schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `ALTER TABLE artifacts RENAME COLUMN sha256_backup TO sha256`)
	})
	if _, err := s.GetRevision(ctx, rev.ID); err == nil {
		t.Fatal("GetRevision must propagate the artifacts query error")
	}
}
