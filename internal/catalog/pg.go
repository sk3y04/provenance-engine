package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sk3y04/provenance-engine/internal/archive"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(ctx context.Context, connString string) (*PgStore, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &PgStore{pool: pool}, nil
}

func (s *PgStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PgStore) Init(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, migration001)
	return err
}

func (s *PgStore) UpsertCollection(ctx context.Context, col archive.ArchiveCollection) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO archive_collections (name, vault_root, description, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE SET vault_root=$2, description=$3, updated_at=$5`,
		col.Name, col.VaultRoot, col.Description, col.CreatedAt, time.Now())
	return err
}

func (s *PgStore) GetCollection(ctx context.Context, name string) (*archive.ArchiveCollection, error) {
	var col archive.ArchiveCollection
	err := s.pool.QueryRow(ctx,
		`SELECT name, vault_root, COALESCE(description, ''), created_at, updated_at
		 FROM archive_collections WHERE name=$1`, name).
		Scan(&col.Name, &col.VaultRoot, &col.Description, &col.CreatedAt, &col.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("archive collection %q not found", name)
	}
	return &col, err
}

func (s *PgStore) ListCollections(ctx context.Context) ([]archive.ArchiveCollection, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, vault_root, COALESCE(description, ''), created_at, updated_at
		 FROM archive_collections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []archive.ArchiveCollection
	for rows.Next() {
		var col archive.ArchiveCollection
		if err := rows.Scan(&col.Name, &col.VaultRoot, &col.Description, &col.CreatedAt, &col.UpdatedAt); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

func (s *PgStore) InsertRevision(ctx context.Context, rev *archive.Revision, collectionName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO revisions (id, collection_name, source_url, source_kind, source_ref, tool, tool_version, captured_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rev.ID, collectionName, rev.Source.URL, string(rev.Source.Kind), rev.Source.Reference,
		rev.Tool, rev.ToolVersion, rev.CapturedAt)
	if err != nil {
		return fmt.Errorf("insert revision: %w", err)
	}

	seenDocs := make(map[[2]string]bool)
	seenRels := make(map[[3]string]bool)
	for _, rel := range rev.Relations {
		seenRels[[3]string{rel.From, rel.To, string(rel.Kind)}] = true
	}

	for _, ent := range rev.Entities {
		pubAt := interface{}(nil)
		if ent.PublishedAt != nil {
			pubAt = *ent.PublishedAt
		}
		rawJSON, _ := json.Marshal(ent.RawMetadata)
		if len(rawJSON) == 0 {
			rawJSON = []byte("{}")
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO entities (revision_id, external_id, url, canonical_url, title, author, published_at, captured_at, kind, extractor, raw_metadata)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			 ON CONFLICT (revision_id, external_id) DO NOTHING`,
			rev.ID, ent.ExternalID, ent.URL, ent.CanonicalURL, ent.Title, ent.Author, pubAt, ent.CapturedAt, ent.Kind, ent.Extractor, rawJSON)
		if err != nil {
			return fmt.Errorf("insert entity: %w", err)
		}

		if ent.Text != nil && ent.Text.Content != "" {
			_, err = tx.Exec(ctx,
				`INSERT INTO documents (revision_id, entity_external_id, content, format)
				 VALUES ($1, $2, $3, $4)`,
				rev.ID, ent.ExternalID, ent.Text.Content, string(ent.Text.Format))
			if err != nil {
				return fmt.Errorf("insert document: %w", err)
			}
			seenDocs[[2]string{ent.ExternalID, ent.Text.Content}] = true
		}

		// Importers attach documents and relations per entity; persist them
		// too, deduplicated against the entity text and revision relations.
		for _, doc := range ent.Documents {
			key := [2]string{doc.ExternalID, doc.Content}
			if doc.Content == "" || seenDocs[key] {
				continue
			}
			seenDocs[key] = true
			_, err = tx.Exec(ctx,
				`INSERT INTO documents (revision_id, entity_external_id, content, format)
				 VALUES ($1, $2, $3, $4)`,
				rev.ID, doc.ExternalID, doc.Content, string(doc.Format))
			if err != nil {
				return fmt.Errorf("insert document: %w", err)
			}
		}
		for _, rel := range ent.Relations {
			key := [3]string{rel.From, rel.To, string(rel.Kind)}
			if seenRels[key] {
				continue
			}
			seenRels[key] = true
			_, err = tx.Exec(ctx,
				`INSERT INTO relations (revision_id, from_id, to_id, kind)
				 VALUES ($1, $2, $3, $4)`,
				rev.ID, rel.From, rel.To, string(rel.Kind))
			if err != nil {
				return fmt.Errorf("insert relation: %w", err)
			}
		}
	}

	for _, doc := range rev.Documents {
		key := [2]string{doc.ExternalID, doc.Content}
		if doc.Content == "" || seenDocs[key] {
			continue
		}
		seenDocs[key] = true
		_, err = tx.Exec(ctx,
			`INSERT INTO documents (revision_id, entity_external_id, content, format)
			 VALUES ($1, $2, $3, $4)`,
			rev.ID, doc.ExternalID, doc.Content, string(doc.Format))
		if err != nil {
			return fmt.Errorf("insert document: %w", err)
		}
	}

	for _, art := range rev.Artifacts {
		_, err = tx.Exec(ctx,
			`INSERT INTO artifacts (sha256, revision_id, path, size, mime_type, kind)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (sha256) DO NOTHING`,
			art.Sha256, rev.ID, art.Path, art.Size, art.MimeType, string(art.Kind))
		if err != nil {
			return fmt.Errorf("insert artifact: %w", err)
		}
	}

	for _, rel := range rev.Relations {
		_, err = tx.Exec(ctx,
			`INSERT INTO relations (revision_id, from_id, to_id, kind)
			 VALUES ($1, $2, $3, $4)`,
			rev.ID, rel.From, rel.To, string(rel.Kind))
		if err != nil {
			return fmt.Errorf("insert relation: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetRevision(ctx context.Context, id string) (*archive.Revision, error) {
	rev := &archive.Revision{ID: id}
	var sourceURL, sourceKind, sourceRef string
	err := s.pool.QueryRow(ctx,
		`SELECT source_url, source_kind, source_ref, tool, COALESCE(tool_version, ''), captured_at
		 FROM revisions WHERE id=$1`, id).
		Scan(&sourceURL, &sourceKind, &sourceRef, &rev.Tool, &rev.ToolVersion, &rev.CapturedAt)
	if err != nil {
		return nil, err
	}
	rev.Source = archive.Source{URL: sourceURL, Kind: archive.SourceKind(sourceKind), Reference: sourceRef}

	rows, err := s.pool.Query(ctx,
		`SELECT external_id, url, COALESCE(canonical_url, ''), COALESCE(title, ''), COALESCE(author, ''),
		        published_at, captured_at, COALESCE(kind, ''), COALESCE(extractor, ''),
		        raw_metadata
		 FROM entities WHERE revision_id=$1 ORDER BY captured_at, external_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ent archive.Entity
		var pubAt *time.Time
		var rawJSON []byte
		if err := rows.Scan(&ent.ExternalID, &ent.URL, &ent.CanonicalURL, &ent.Title, &ent.Author,
			&pubAt, &ent.CapturedAt, &ent.Kind, &ent.Extractor, &rawJSON); err != nil {
			return nil, err
		}
		ent.PublishedAt = pubAt
		ent.RawMetadata = rawJSON
		rev.Entities = append(rev.Entities, ent)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	artRows, err := s.pool.Query(ctx,
		`SELECT sha256, path, size, COALESCE(mime_type, ''), kind
		 FROM artifacts WHERE revision_id=$1 ORDER BY sha256`, id)
	if err != nil {
		return nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer artRows.Close()
	for artRows.Next() {
		var art archive.Artifact
		var kind string
		if err := artRows.Scan(&art.Sha256, &art.Path, &art.Size, &art.MimeType, &kind); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		art.Kind = archive.ArtifactKind(kind)
		rev.Artifacts = append(rev.Artifacts, art)
	}
	if err := artRows.Err(); err != nil {
		return nil, fmt.Errorf("artifact rows: %w", err)
	}

	docRows, err := s.pool.Query(ctx,
		`SELECT entity_external_id, content, format
		 FROM documents WHERE revision_id=$1 ORDER BY entity_external_id, id`, id)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer docRows.Close()
	for docRows.Next() {
		var doc archive.Document
		var format string
		if err := docRows.Scan(&doc.ExternalID, &doc.Content, &format); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		doc.Format = archive.DocumentFormat(format)
		rev.Documents = append(rev.Documents, doc)
	}
	if err := docRows.Err(); err != nil {
		return nil, fmt.Errorf("document rows: %w", err)
	}
	// Re-attach the entity text the capture flow stores per entity.
	for i := range rev.Entities {
		for _, doc := range rev.Documents {
			if doc.ExternalID == rev.Entities[i].ExternalID {
				d := doc
				rev.Entities[i].Text = &d
				break
			}
		}
	}

	relRows, err := s.pool.Query(ctx,
		`SELECT from_id, to_id, kind FROM relations WHERE revision_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("query relations: %w", err)
	}
	defer relRows.Close()
	for relRows.Next() {
		var rel archive.Relation
		var kind string
		if err := relRows.Scan(&rel.From, &rel.To, &kind); err != nil {
			return nil, fmt.Errorf("scan relation: %w", err)
		}
		rel.Kind = archive.RelationKind(kind)
		rev.Relations = append(rev.Relations, rel)
	}
	if err := relRows.Err(); err != nil {
		return nil, fmt.Errorf("relation rows: %w", err)
	}

	return rev, nil
}

func (s *PgStore) ListRevisions(ctx context.Context, collectionName string) ([]archive.Revision, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, source_url, source_kind, source_ref, tool, COALESCE(tool_version, ''), captured_at
		 FROM revisions WHERE collection_name=$1 ORDER BY captured_at DESC`, collectionName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revs []archive.Revision
	for rows.Next() {
		var rev archive.Revision
		var sourceURL, sourceKind, sourceRef string
		if err := rows.Scan(&rev.ID, &sourceURL, &sourceKind, &sourceRef, &rev.Tool, &rev.ToolVersion, &rev.CapturedAt); err != nil {
			return nil, err
		}
		rev.Source = archive.Source{URL: sourceURL, Kind: archive.SourceKind(sourceKind), Reference: sourceRef}
		revs = append(revs, rev)
	}
	return revs, rows.Err()
}

func (s *PgStore) ListRevisionIDsByCollection(ctx context.Context, collectionName string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM revisions WHERE collection_name=$1 ORDER BY captured_at DESC`, collectionName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PgStore) DeleteRevisionsByCollection(ctx context.Context, collectionName string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Delete in FK order: relations -> documents -> entities -> artifacts -> revisions
	if _, err := tx.Exec(ctx, `DELETE FROM relations WHERE revision_id IN (SELECT id FROM revisions WHERE collection_name=$1)`, collectionName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM documents WHERE revision_id IN (SELECT id FROM revisions WHERE collection_name=$1)`, collectionName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM entities WHERE revision_id IN (SELECT id FROM revisions WHERE collection_name=$1)`, collectionName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM artifacts WHERE revision_id IN (SELECT id FROM revisions WHERE collection_name=$1)`, collectionName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM revisions WHERE collection_name=$1`, collectionName); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetEntity(ctx context.Context, revisionID, externalID string) (*archive.Entity, error) {
	var ent archive.Entity
	var pubAt *time.Time
	var rawJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT external_id, url, COALESCE(canonical_url, ''), COALESCE(title, ''), COALESCE(author, ''),
		        published_at, captured_at, COALESCE(kind, ''), COALESCE(extractor, ''), raw_metadata
		 FROM entities WHERE revision_id=$1 AND external_id=$2`,
		revisionID, externalID).
		Scan(&ent.ExternalID, &ent.URL, &ent.CanonicalURL, &ent.Title, &ent.Author,
			&pubAt, &ent.CapturedAt, &ent.Kind, &ent.Extractor, &rawJSON)
	if err != nil {
		return nil, err
	}
	ent.PublishedAt = pubAt
	ent.RawMetadata = rawJSON
	return &ent, nil
}

func (s *PgStore) Search(ctx context.Context, query string, opts SearchOptions) (*SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}

	sql := `SELECT
		e.title, e.url, e.external_id,
		e.author, e.kind, e.captured_at,
		r.id AS revision_id,
		c.name AS collection_name,
		COALESCE(ts_rank(e.search_vector, q), 0) + COALESCE(ts_rank(d.search_vector, q), 0) AS rank,
		COALESCE(ts_headline('english', COALESCE(e.title, '') || ' ' || COALESCE(e.author, '') || ' ' || COALESCE(d.content, ''), q, 'StartSel=**bold**, StopSel=**bold**, MaxWords=40, MinWords=15'), COALESCE(e.title, '') || ' ' || COALESCE(d.content, '')) AS headline
	FROM entities e
	JOIN revisions r ON e.revision_id = r.id
	JOIN archive_collections c ON r.collection_name = c.name
	LEFT JOIN documents d ON d.revision_id = e.revision_id AND d.entity_external_id = e.external_id,
	plainto_tsquery('english', $1) q
	WHERE e.search_vector @@ q OR d.search_vector @@ q`

	args := []interface{}{query}
	if opts.CollectionName != "" {
		sql += ` AND c.name = $2`
		args = append(args, opts.CollectionName)
	}
	if opts.Kind != "" {
		idx := len(args) + 1
		sql += fmt.Sprintf(` AND e.kind = $%d`, idx)
		args = append(args, opts.Kind)
	}
	sql += ` ORDER BY rank DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, opts.Limit)
	sql += ` OFFSET $` + fmt.Sprint(len(args)+1)
	args = append(args, opts.Offset)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "42601" {
			return &SearchResult{}, nil
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var capturedAt time.Time
		if err := rows.Scan(&h.Title, &h.URL, &h.EntityID, &h.Author, &h.Kind,
			&capturedAt, &h.RevisionID, &h.CollectionName, &h.Rank, &h.Headline); err != nil {
			return nil, err
		}
		h.CapturedAt = capturedAt
		hits = append(hits, h)
	}
	return &SearchResult{Total: len(hits), Hits: hits}, rows.Err()
}
