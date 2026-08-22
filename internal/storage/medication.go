package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

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
	// normalization.Medication.Status value.
	Status normalization.MedicationStatus
}

// MedicationRepository mirrors docs/domain/06-medication.md §6.
type MedicationRepository interface {
	Add(ctx context.Context, m normalization.Medication) error
	ListByUser(ctx context.Context, userID string, filter MedicationFilter) ([]normalization.Medication, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Medication, error)
	// ReplaceForDocument deletes every existing Medication for documentID
	// that's still untouched by the user (ConfirmedEndedAt == nil) and
	// inserts meds in its place, all in one transaction. A medication the
	// user has directly confirmed finished via medical.complete_medication
	// (ConfirmedEndedAt set) is left untouched rather than being silently
	// reset by a reprocess of the document it came from — mirrors
	// DiagnosisRepository.ReplaceForDocument's rule.
	ReplaceForDocument(ctx context.Context, documentID string, meds []normalization.Medication) error
	// MarkEndedManually sets id's status to MedicationStatusCompleted and both EndedAt and
	// ConfirmedEndedAt to at — medical.complete_medication's write, the one
	// way Medication.Status changes outside of Extraction. Unlike
	// DiagnosisRepository.MarkResolved there's no reasoning parameter:
	// Medication has no general-purpose free-text field to hold it (Reason
	// documents why the drug was prescribed, not why it ended).
	MarkEndedManually(ctx context.Context, id, userID string, at time.Time) error
}

type sqliteMedicationRepository struct {
	db *sql.DB
}

// NewMedicationRepository builds a MedicationRepository backed by s.
func NewMedicationRepository(s *Store) MedicationRepository {
	return &sqliteMedicationRepository{db: s.db}
}

const medicationSelectColumns = `SELECT id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by, confirmed_ended_at`

func (r *sqliteMedicationRepository) Add(ctx context.Context, m normalization.Medication) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO medications (id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by, confirmed_ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.UserID, m.DocumentID, m.DrugName, m.DoseAmount, m.DoseUnit, m.Frequency, m.Route,
		nullUnix(m.StartedAt), nullUnix(m.EndedAt), string(m.Status), m.Reason, m.PrescribedBy, nullUnix(m.ConfirmedEndedAt),
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
		args = append(args, string(filter.Status))
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

	if _, err := tx.ExecContext(ctx, `DELETE FROM medications WHERE document_id = ? AND confirmed_ended_at IS NULL`, documentID); err != nil {
		return fmt.Errorf("storage: replace medications: delete: %w", err)
	}
	for _, m := range meds {
		// Always mint a fresh id, ignoring whatever id normalization
		// assigned — a user-confirmed row from the same document may still
		// occupy that same deterministic id (see the delete above), so
		// reusing it here could collide with that surviving row's primary
		// key (mirrors DiagnosisRepository.ReplaceForDocument).
		id := "med_" + uuid.New().String()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO medications (id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by, confirmed_ended_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, m.UserID, documentID, m.DrugName, m.DoseAmount, m.DoseUnit, m.Frequency, m.Route,
			nullUnix(m.StartedAt), nullUnix(m.EndedAt), string(m.Status), m.Reason, m.PrescribedBy, nullUnix(m.ConfirmedEndedAt),
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
		var startedAt, endedAt, confirmedEndedAt sql.NullInt64
		if err := rows.Scan(&m.ID, &m.UserID, &m.DocumentID, &m.DrugName, &m.DoseAmount, &m.DoseUnit, &m.Frequency, &m.Route,
			&startedAt, &endedAt, &m.Status, &m.Reason, &m.PrescribedBy, &confirmedEndedAt); err != nil {
			return nil, fmt.Errorf("storage: scan medication: %w", err)
		}
		m.StartedAt = timePtr(startedAt)
		m.EndedAt = timePtr(endedAt)
		m.ConfirmedEndedAt = timePtr(confirmedEndedAt)
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate medications: %w", err)
	}
	return result, nil
}

// MarkEndedManually implements MedicationRepository.MarkEndedManually.
func (r *sqliteMedicationRepository) MarkEndedManually(ctx context.Context, id, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE medications SET status = ?, ended_at = ?, confirmed_ended_at = ? WHERE id = ? AND user_id = ?`,
		string(normalization.MedicationStatusCompleted), at.Unix(), at.Unix(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark medication ended manually: %w", err)
	}
	return nil
}
