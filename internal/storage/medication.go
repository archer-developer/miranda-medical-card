package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// MedicationFilter narrows MedicationRepository.ListByUser.
// docs/domain/06-medication.md §6 names a filter parameter without
// specifying its fields; Status is the only filter dimension any
// documented consumer (ProfileBuilder's "current medications") actually
// needs so far — add fields here only when a real caller needs them, not
// speculatively.
type MedicationFilter struct {
	// Status, if non-empty, restricts results to that exact
	// normalization.Medication.Status value (e.g. "active").
	Status string
}

// MedicationRepository mirrors docs/domain/06-medication.md §6.
type MedicationRepository interface {
	Add(ctx context.Context, m normalization.Medication) error
	ListByUser(ctx context.Context, userID string, filter MedicationFilter) ([]normalization.Medication, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Medication, error)
	ReplaceForDocument(ctx context.Context, documentID string, meds []normalization.Medication) error
}

type sqliteMedicationRepository struct {
	db *sql.DB
}

// NewMedicationRepository builds a MedicationRepository backed by s.
func NewMedicationRepository(s *Store) MedicationRepository {
	return &sqliteMedicationRepository{db: s.db}
}

const medicationSelectColumns = `SELECT id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by`

func (r *sqliteMedicationRepository) Add(ctx context.Context, m normalization.Medication) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO medications (id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.UserID, m.DocumentID, m.DrugName, m.DoseAmount, m.DoseUnit, m.Frequency, m.Route,
		nullUnix(m.StartedAt), nullUnix(m.EndedAt), m.Status, m.Reason, m.PrescribedBy,
	)
	if err != nil {
		return fmt.Errorf("storage: add medication: %w", err)
	}
	return nil
}

func (r *sqliteMedicationRepository) ListByUser(ctx context.Context, userID string, filter MedicationFilter) ([]normalization.Medication, error) {
	query := medicationSelectColumns + ` FROM medications WHERE user_id = ?`
	args := []any{userID}
	if filter.Status != "" {
		query += ` AND status = ?`
		args = append(args, filter.Status)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list medications by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMedications(rows)
}

func (r *sqliteMedicationRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.Medication, error) {
	rows, err := r.db.QueryContext(ctx, medicationSelectColumns+` FROM medications WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list medications by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMedications(rows)
}

func (r *sqliteMedicationRepository) ReplaceForDocument(ctx context.Context, documentID string, meds []normalization.Medication) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace medications: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM medications WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace medications: delete: %w", err)
	}
	for _, m := range meds {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO medications (id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.UserID, documentID, m.DrugName, m.DoseAmount, m.DoseUnit, m.Frequency, m.Route,
			nullUnix(m.StartedAt), nullUnix(m.EndedAt), m.Status, m.Reason, m.PrescribedBy,
		)
		if err != nil {
			return fmt.Errorf("storage: replace medications: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace medications: commit: %w", err)
	}
	return nil
}

func scanMedications(rows *sql.Rows) ([]normalization.Medication, error) {
	var result []normalization.Medication
	for rows.Next() {
		var m normalization.Medication
		var startedAt, endedAt sql.NullInt64
		if err := rows.Scan(&m.ID, &m.UserID, &m.DocumentID, &m.DrugName, &m.DoseAmount, &m.DoseUnit, &m.Frequency, &m.Route,
			&startedAt, &endedAt, &m.Status, &m.Reason, &m.PrescribedBy); err != nil {
			return nil, fmt.Errorf("storage: scan medication: %w", err)
		}
		m.StartedAt = timePtr(startedAt)
		m.EndedAt = timePtr(endedAt)
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate medications: %w", err)
	}
	return result, nil
}
