package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DocumentFTSResult is one FTS5 match over document_fts.
type DocumentFTSResult struct {
	DocumentID string
	Title      string
	// Snippet is a short excerpt around the match (see FTS5's snippet()),
	// suitable for a Knowledge Chunk's Content without passing the entire
	// document text to an LLM — see docs/architecture/03-knowledge-providers.md
	// §12's "Плохой пример" vs "Хороший пример" (small, human-readable
	// fragments, not raw internal structures or entire documents).
	Snippet string
	// Rank is FTS5's bm25() score — lower is a better match. Exposed
	// as-is (not normalized to Confidence here) so the caller (a Knowledge
	// Provider) applies docs/architecture/04-search.md §18's Confidence
	// scale, which this package deliberately knows nothing about.
	Rank float64
}

// EventFTSResult is one FTS5 match over event_fts.
type EventFTSResult struct {
	EventID string
	Snippet string
	Rank    float64
}

// FTSRepository indexes and searches the free text docs/architecture/04-search.md
// §13 says belongs in FTS5 — document text/Summary, and (see
// storage.Embedding's doc comment for the same reasoning) self-reported
// event text. Deliberately does not index structured entities (§13: "Не
// рекомендуется индексировать структурированные сущности").
type FTSRepository interface {
	// IndexDocument (re)indexes one MedicalDocument's searchable text —
	// callers should call this after every successful
	// UploadDocument/ReprocessDocument, replacing whatever was indexed
	// before (FTS5 has no natural document-scoped replace primitive, so
	// this deletes any existing row for documentID before inserting).
	IndexDocument(ctx context.Context, userID, documentID, title, content string) error
	RemoveDocument(ctx context.Context, documentID string) error
	SearchDocuments(ctx context.Context, userID, query string, limit int) ([]DocumentFTSResult, error)

	IndexEvent(ctx context.Context, userID, eventID, content string) error
	RemoveEvent(ctx context.Context, eventID string) error
	SearchEvents(ctx context.Context, userID, query string, limit int) ([]EventFTSResult, error)
}

type sqliteFTSRepository struct {
	db *sql.DB
}

// NewFTSRepository builds an FTSRepository backed by s.
func NewFTSRepository(s *Store) FTSRepository {
	return &sqliteFTSRepository{db: s.db}
}

func (r *sqliteFTSRepository) IndexDocument(ctx context.Context, userID, documentID, title, content string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM document_fts WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: index document fts: delete existing: %w", err)
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO document_fts (document_id, user_id, title, content) VALUES (?, ?, ?, ?)`,
		documentID, userID, title, content,
	)
	if err != nil {
		return fmt.Errorf("storage: index document fts: %w", err)
	}
	return nil
}

func (r *sqliteFTSRepository) RemoveDocument(ctx context.Context, documentID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM document_fts WHERE document_id = ?`, documentID)
	if err != nil {
		return fmt.Errorf("storage: remove document fts: %w", err)
	}
	return nil
}

func (r *sqliteFTSRepository) SearchDocuments(ctx context.Context, userID, query string, limit int) ([]DocumentFTSResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT document_id, title, snippet(document_fts, 3, '[', ']', '...', 40), bm25(document_fts)
		FROM document_fts WHERE document_fts MATCH ? AND user_id = ? ORDER BY bm25(document_fts) LIMIT ?`,
		ftsQuery(query), userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: search document fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []DocumentFTSResult
	for rows.Next() {
		var d DocumentFTSResult
		if err := rows.Scan(&d.DocumentID, &d.Title, &d.Snippet, &d.Rank); err != nil {
			return nil, fmt.Errorf("storage: scan document fts result: %w", err)
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate document fts results: %w", err)
	}
	return result, nil
}

func (r *sqliteFTSRepository) IndexEvent(ctx context.Context, userID, eventID, content string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM event_fts WHERE event_id = ?`, eventID); err != nil {
		return fmt.Errorf("storage: index event fts: delete existing: %w", err)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO event_fts (event_id, user_id, content) VALUES (?, ?, ?)`, eventID, userID, content)
	if err != nil {
		return fmt.Errorf("storage: index event fts: %w", err)
	}
	return nil
}

func (r *sqliteFTSRepository) RemoveEvent(ctx context.Context, eventID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM event_fts WHERE event_id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("storage: remove event fts: %w", err)
	}
	return nil
}

func (r *sqliteFTSRepository) SearchEvents(ctx context.Context, userID, query string, limit int) ([]EventFTSResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT event_id, snippet(event_fts, 2, '[', ']', '...', 12), bm25(event_fts)
		FROM event_fts WHERE event_fts MATCH ? AND user_id = ? ORDER BY bm25(event_fts) LIMIT ?`,
		ftsQuery(query), userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: search event fts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []EventFTSResult
	for rows.Next() {
		var e EventFTSResult
		if err := rows.Scan(&e.EventID, &e.Snippet, &e.Rank); err != nil {
			return nil, fmt.Errorf("storage: scan event fts result: %w", err)
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate event fts results: %w", err)
	}
	return result, nil
}

// ftsQuery wraps each whitespace-separated term in double quotes (escaping
// any literal quote in the term as FTS5 expects, "" for a literal ") so a
// query containing FTS5 special syntax characters (e.g. a lone "-" or an
// unbalanced quote from user input) is treated as a literal per-term phrase
// search rather than parsed as FTS5 query syntax, which would otherwise
// error on arbitrary user text instead of just returning no/fewer matches.
// Space-separated quoted terms are implicitly AND-ed by FTS5's default
// query syntax. With the trigram tokenizer (see storage.go's schema
// comment), a term shorter than 3 characters simply never matches anything
// — not an error, just zero results for that term (and, being AND-ed,
// often the whole query).
func ftsQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return `""`
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}
