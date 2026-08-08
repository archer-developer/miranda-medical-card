package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ExtractionRecord mirrors docs/domain/03-files-and-documents.md §4 — the
// raw, versioned JSON produced by Structured Extraction for one document.
// Named ExtractionRecord (not Extraction) to avoid colliding with
// internal/extraction's own exported Result type: that package is the code
// that calls the LLM, this struct is what gets persisted afterward. Raw
// carries internal/extraction.Result's marshaled JSON but is typed
// json.RawMessage here rather than importing that package, since this
// layer doesn't need — and shouldn't need — to know Extraction's schema to
// store and retrieve it verbatim (Normalization is what actually
// interprets Raw's shape).
type ExtractionRecord struct {
	ID            string
	DocumentID    string
	Version       int
	Active        bool
	PromptVersion string
	ModelUsed     string
	Raw           json.RawMessage
	CreatedAt     time.Time
}

// ExtractionRepository mirrors docs/domain/03-files-and-documents.md §4
// "Repository".
type ExtractionRepository interface {
	// Add inserts e as a new version. If e.ID is empty, a new id is
	// generated (prefixed extr_) and returned on the result. Active is
	// always stored as false regardless of what e.Active is set to —
	// making Activate the one and only code path that ever sets a row
	// active enforces this package's "exactly one active version,
	// switched atomically" invariant (docs/domain/03-files-and-documents.md
	// §4 "Инварианты") by construction, rather than trusting every future
	// Add caller to remember to deactivate the previous version
	// themselves. Callers wanting the new version to become active call
	// Activate immediately afterward.
	Add(ctx context.Context, e ExtractionRecord) (ExtractionRecord, error)
	// ListVersions returns every version for documentId, oldest first.
	ListVersions(ctx context.Context, documentID string) ([]ExtractionRecord, error)
	// GetActive returns the one active version for documentId, or
	// ErrNotFound if none has been activated yet.
	GetActive(ctx context.Context, documentID string) (ExtractionRecord, error)
	// Activate marks id's row active and every other version of the same
	// document inactive, in one transaction.
	Activate(ctx context.Context, id string) error
}

type sqliteExtractionRepository struct {
	db *sql.DB
}

// NewExtractionRepository builds an ExtractionRepository backed by s.
func NewExtractionRepository(s *Store) ExtractionRepository {
	return &sqliteExtractionRepository{db: s.db}
}

const extractionRecordSelectColumns = `SELECT id, document_id, version, active, prompt_version, model_used, raw, created_at`

func (r *sqliteExtractionRepository) Add(ctx context.Context, e ExtractionRecord) (ExtractionRecord, error) {
	if e.ID == "" {
		e.ID = "extr_" + uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	e.Active = false // see Add's doc comment
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO extraction_records (id, document_id, version, active, prompt_version, model_used, raw, created_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?)`,
		e.ID, e.DocumentID, e.Version, e.PromptVersion, e.ModelUsed, string(e.Raw), e.CreatedAt.Unix(),
	)
	if err != nil {
		return ExtractionRecord{}, fmt.Errorf("storage: add extraction record: %w", err)
	}
	return e, nil
}

func (r *sqliteExtractionRepository) ListVersions(ctx context.Context, documentID string) ([]ExtractionRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		extractionRecordSelectColumns+` FROM extraction_records WHERE document_id = ? ORDER BY version ASC`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list extraction versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []ExtractionRecord
	for rows.Next() {
		e, err := scanExtractionRecordRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate extraction versions: %w", err)
	}
	return result, nil
}

func (r *sqliteExtractionRepository) GetActive(ctx context.Context, documentID string) (ExtractionRecord, error) {
	row := r.db.QueryRowContext(ctx,
		extractionRecordSelectColumns+` FROM extraction_records WHERE document_id = ? AND active = 1 LIMIT 1`, documentID)
	e, err := scanExtractionRecordFrom(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtractionRecord{}, ErrNotFound
	}
	return e, err
}

func (r *sqliteExtractionRepository) Activate(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: activate extraction record: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var documentID string
	err = tx.QueryRowContext(ctx, `SELECT document_id FROM extraction_records WHERE id = ?`, id).Scan(&documentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("storage: activate extraction record: find document: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE extraction_records SET active = 0 WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: activate extraction record: deactivate others: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE extraction_records SET active = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("storage: activate extraction record: activate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: activate extraction record: commit: %w", err)
	}
	return nil
}

func scanExtractionRecordRow(rows *sql.Rows) (ExtractionRecord, error) {
	return scanExtractionRecordFrom(rows)
}

func scanExtractionRecordFrom(s rowScanner) (ExtractionRecord, error) {
	var e ExtractionRecord
	var active int
	var raw string
	var createdAt int64
	err := s.Scan(&e.ID, &e.DocumentID, &e.Version, &active, &e.PromptVersion, &e.ModelUsed, &raw, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExtractionRecord{}, err
		}
		return ExtractionRecord{}, fmt.Errorf("storage: scan extraction record: %w", err)
	}
	e.Active = active == 1
	e.Raw = json.RawMessage(raw)
	e.CreatedAt = time.Unix(createdAt, 0).UTC()
	return e, nil
}
