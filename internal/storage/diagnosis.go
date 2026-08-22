package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// DiagnosisRepository mirrors docs/domain/07-diagnosis-and-allergy.md §5.
type DiagnosisRepository interface {
	Add(ctx context.Context, d normalization.Diagnosis) error
	ListByUser(ctx context.Context, userID string) ([]normalization.Diagnosis, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Diagnosis, error)
	// ReplaceForDocument atomically deletes every existing Diagnosis for
	// documentID and inserts diagnoses in its place — the document-scoped
	// replace invariant every domain doc describes (see e.g.
	// docs/domain/07-diagnosis-and-allergy.md §4), implemented as one
	// transaction so a caller never observes a partially-replaced set.
	ReplaceForDocument(ctx context.Context, documentID string, diagnoses []normalization.Diagnosis) error
}

type sqliteDiagnosisRepository struct {
	db *sql.DB
}

// NewDiagnosisRepository builds a DiagnosisRepository backed by s.
func NewDiagnosisRepository(s *Store) DiagnosisRepository {
	return &sqliteDiagnosisRepository{db: s.db}
}

func (r *sqliteDiagnosisRepository) Add(ctx context.Context, d normalization.Diagnosis) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO diagnoses (id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.DocumentID, d.Name, d.Code, d.CodeSystem, nullUnix(d.DiagnosedAt), d.Status, d.Notes,
		nullUnix(d.ExpectedResolutionFrom), nullUnix(d.ExpectedResolutionTo), d.StatusReasoning,
	)
	if err != nil {
		return fmt.Errorf("storage: add diagnosis: %w", err)
	}
	return nil
}

func (r *sqliteDiagnosisRepository) ListByUser(ctx context.Context, userID string) ([]normalization.Diagnosis, error) {
	rows, err := r.db.QueryContext(ctx, diagnosisSelectColumns+` FROM diagnoses WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list diagnoses by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDiagnoses(rows)
}

func (r *sqliteDiagnosisRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.Diagnosis, error) {
	rows, err := r.db.QueryContext(ctx, diagnosisSelectColumns+` FROM diagnoses WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list diagnoses by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDiagnoses(rows)
}

func (r *sqliteDiagnosisRepository) ReplaceForDocument(ctx context.Context, documentID string, diagnoses []normalization.Diagnosis) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace diagnoses: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM diagnoses WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace diagnoses: delete: %w", err)
	}
	for _, d := range diagnoses {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO diagnoses (id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.UserID, documentID, d.Name, d.Code, d.CodeSystem, nullUnix(d.DiagnosedAt), d.Status, d.Notes,
			nullUnix(d.ExpectedResolutionFrom), nullUnix(d.ExpectedResolutionTo), d.StatusReasoning,
		)
		if err != nil {
			return fmt.Errorf("storage: replace diagnoses: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace diagnoses: commit: %w", err)
	}
	return nil
}

const diagnosisSelectColumns = `SELECT id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning`

func scanDiagnoses(rows *sql.Rows) ([]normalization.Diagnosis, error) {
	var result []normalization.Diagnosis
	for rows.Next() {
		var d normalization.Diagnosis
		var diagnosedAt, resolutionFrom, resolutionTo sql.NullInt64
		if err := rows.Scan(&d.ID, &d.UserID, &d.DocumentID, &d.Name, &d.Code, &d.CodeSystem, &diagnosedAt, &d.Status, &d.Notes, &resolutionFrom, &resolutionTo, &d.StatusReasoning); err != nil {
			return nil, fmt.Errorf("storage: scan diagnosis: %w", err)
		}
		d.DiagnosedAt = timePtr(diagnosedAt)
		d.ExpectedResolutionFrom = timePtr(resolutionFrom)
		d.ExpectedResolutionTo = timePtr(resolutionTo)
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate diagnoses: %w", err)
	}
	return result, nil
}
