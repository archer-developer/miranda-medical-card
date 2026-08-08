package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DocumentStatus values mirror docs/domain/11-value-objects.md §7 — the
// Pipeline's overall progress for one MedicalDocument, monotonically
// PENDING -> RUNNING -> (READY | FAILED), with a reprocess run returning it
// to RUNNING (see docs/domain/03-files-and-documents.md §3 "Инварианты").
const (
	DocumentStatusPending = "PENDING"
	DocumentStatusRunning = "RUNNING"
	DocumentStatusReady   = "READY"
	DocumentStatusFailed  = "FAILED"
)

// MedicalDocument mirrors docs/domain/03-files-and-documents.md §3.
// RecognizedText is the full OCR/Vision output — internal, never returned
// through MCP (see that doc's field table), same caveat as
// File.StoragePath: this package doesn't enforce that itself.
type MedicalDocument struct {
	ID             string
	UserID         string
	FileID         string
	Status         string
	DocumentType   string
	DocumentDate   *time.Time
	Title          string
	Organization   string
	Doctor         string
	RecognizedText string
	Summary        string
	UploadedAt     time.Time
}

// DocumentFilter narrows DocumentRepository.List. FileID exists
// specifically so a caller can check "has this File already been imported
// as a MedicalDocument" (docs/mcp/03-documents.md §4's
// DOCUMENT_ALREADY_IMPORTED) before creating a new one — see
// MedicationFilter's doc comment for the general "add fields only when a
// real caller needs them" reasoning this follows.
type DocumentFilter struct {
	Status string
	FileID string
}

// DocumentRepository mirrors docs/domain/03-files-and-documents.md §3
// "Repository". Deliberately does not cascade-delete Timeline/Domain
// Entities/Extraction/etc. on Remove — docs/architecture/06-storage.md §9's
// top-down deletion order spans repositories this package deliberately
// keeps independent of each other (see this package's doc comment and
// docs/architecture/06-storage.md §2 "Независимость уровней"), so running
// that order is a Pipeline/application-layer responsibility that calls
// each repository in turn, not something baked into one repository's
// Remove.
type DocumentRepository interface {
	// Add inserts d. If d.ID is empty, a new id is generated (prefixed
	// doc_) and returned on the result.
	Add(ctx context.Context, d MedicalDocument) (MedicalDocument, error)
	// Get returns the MedicalDocument with the given id, if it belongs to
	// userID — see FileRepository.Get's doc comment for why a wrong userID
	// and a nonexistent id both return ErrNotFound.
	Get(ctx context.Context, id, userID string) (MedicalDocument, error)
	List(ctx context.Context, userID string, filter DocumentFilter) ([]MedicalDocument, error)
	UpdateStatus(ctx context.Context, id, userID, status string) error
	// UpdateExtracted sets the fields Pipeline only learns once
	// Extraction/Normalization complete. Kept separate from UpdateStatus
	// (a narrower, single-purpose transition made earlier in the same run,
	// before any of this is known) and from a full MedicalDocument
	// replace, which would let a caller accidentally clobber id/fileId/
	// status/uploadedAt through the same call.
	UpdateExtracted(ctx context.Context, id, userID string, update DocumentExtractedUpdate) error
	Remove(ctx context.Context, id, userID string) error
}

// DocumentExtractedUpdate carries the MedicalDocument fields only known
// after Extraction/Normalization complete — see
// DocumentRepository.UpdateExtracted.
type DocumentExtractedUpdate struct {
	DocumentType   string
	DocumentDate   *time.Time
	Title          string
	Organization   string
	Doctor         string
	RecognizedText string
	Summary        string
}

type sqliteDocumentRepository struct {
	db *sql.DB
}

// NewDocumentRepository builds a DocumentRepository backed by s.
func NewDocumentRepository(s *Store) DocumentRepository {
	return &sqliteDocumentRepository{db: s.db}
}

const documentSelectColumns = `SELECT id, user_id, file_id, status, document_type, document_date, title, organization, doctor, recognized_text, summary, uploaded_at`

func (r *sqliteDocumentRepository) Add(ctx context.Context, d MedicalDocument) (MedicalDocument, error) {
	if d.ID == "" {
		d.ID = "doc_" + uuid.New().String()
	}
	if d.UploadedAt.IsZero() {
		d.UploadedAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = DocumentStatusPending
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO medical_documents (id, user_id, file_id, status, document_type, document_date, title, organization, doctor, recognized_text, summary, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.FileID, d.Status, d.DocumentType, nullUnix(d.DocumentDate), d.Title, d.Organization, d.Doctor, d.RecognizedText, d.Summary, d.UploadedAt.Unix(),
	)
	if err != nil {
		return MedicalDocument{}, fmt.Errorf("storage: add document: %w", err)
	}
	return d, nil
}

func (r *sqliteDocumentRepository) Get(ctx context.Context, id, userID string) (MedicalDocument, error) {
	row := r.db.QueryRowContext(ctx, documentSelectColumns+` FROM medical_documents WHERE id = ? AND user_id = ?`, id, userID)
	return scanDocument(row)
}

func (r *sqliteDocumentRepository) List(ctx context.Context, userID string, filter DocumentFilter) ([]MedicalDocument, error) {
	query := documentSelectColumns + ` FROM medical_documents WHERE user_id = ?`
	args := []any{userID}
	if filter.Status != "" {
		query += ` AND status = ?`
		args = append(args, filter.Status)
	}
	if filter.FileID != "" {
		query += ` AND file_id = ?`
		args = append(args, filter.FileID)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []MedicalDocument
	for rows.Next() {
		d, err := scanDocumentRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate documents: %w", err)
	}
	return result, nil
}

func (r *sqliteDocumentRepository) UpdateStatus(ctx context.Context, id, userID, status string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE medical_documents SET status = ? WHERE id = ? AND user_id = ?`, status, id, userID)
	if err != nil {
		return fmt.Errorf("storage: update document status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update document status: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteDocumentRepository) UpdateExtracted(ctx context.Context, id, userID string, update DocumentExtractedUpdate) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE medical_documents
		SET document_type = ?, document_date = ?, title = ?, organization = ?, doctor = ?, recognized_text = ?, summary = ?
		WHERE id = ? AND user_id = ?`,
		update.DocumentType, nullUnix(update.DocumentDate), update.Title, update.Organization, update.Doctor, update.RecognizedText, update.Summary,
		id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: update document extracted fields: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update document extracted fields: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteDocumentRepository) Remove(ctx context.Context, id, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM medical_documents WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("storage: remove document: %w", err)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting Get and
// List/scanDocumentRow share one Scan call shape.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDocument(row *sql.Row) (MedicalDocument, error) {
	d, err := scanDocumentFrom(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MedicalDocument{}, ErrNotFound
	}
	return d, err
}

func scanDocumentRow(rows *sql.Rows) (MedicalDocument, error) {
	return scanDocumentFrom(rows)
}

func scanDocumentFrom(s rowScanner) (MedicalDocument, error) {
	var d MedicalDocument
	var documentDate sql.NullInt64
	var uploadedAt int64
	err := s.Scan(&d.ID, &d.UserID, &d.FileID, &d.Status, &d.DocumentType, &documentDate, &d.Title, &d.Organization, &d.Doctor, &d.RecognizedText, &d.Summary, &uploadedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MedicalDocument{}, err
		}
		return MedicalDocument{}, fmt.Errorf("storage: scan document: %w", err)
	}
	d.DocumentDate = timePtr(documentDate)
	d.UploadedAt = time.Unix(uploadedAt, 0).UTC()
	return d, nil
}
