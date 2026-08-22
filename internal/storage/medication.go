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
	// whose Status isn't one of MedicationStatusActive/Discontinued/Completed
	// and inserts meds in its place, all in one transaction — the complement
	// of those three protected statuses, not an equality check against
	// MedicationStatusPrescribed, so a legacy empty-Status row (data written
	// before that default existed) is just as replaceable as a properly
	// defaulted "prescribed" one. A medication that HAS moved past
	// "prescribed" (whether Extraction itself said so, or a user confirmed
	// it via medical.start_medication/medical.complete_medication) is left
	// untouched rather than being silently reset by a reprocess of the
	// document it came from — mirrors DiagnosisRepository/PlannedActionRepository's
	// own reprocess-replace rules, keyed on Status directly rather than a
	// separate provenance field (Medication's Status already carries enough
	// signal: nothing legitimately reverts to "prescribed" once past it).
	// meds whose (canonicalized) DrugName still has a surviving row for this
	// document are skipped rather than inserted again — the surviving row
	// already represents that drug for this document, and a fresh
	// extraction re-describing the same drug must not produce a duplicate.
	ReplaceForDocument(ctx context.Context, documentID string, meds []normalization.Medication) error
	// MarkStartedManually sets id's status to MedicationStatusActive and
	// StartedAt to at — medical.start_medication's write, confirming actual
	// intake began (as opposed to merely being prescribed) at a moment that
	// may not match whatever date the source document recorded.
	MarkStartedManually(ctx context.Context, id, userID string, at time.Time) error
	// MarkEndedManually sets id's status to MedicationStatusCompleted and
	// EndedAt to at — medical.complete_medication's write.
	MarkEndedManually(ctx context.Context, id, userID string, at time.Time) error
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
		nullUnix(m.StartedAt), nullUnix(m.EndedAt), string(m.Status), m.Reason, m.PrescribedBy,
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

	// Deletable = anything NOT already past "prescribed" — deliberately the
	// complement of the three protected statuses, not an equality check
	// against MedicationStatusPrescribed itself: a row from data written
	// before this status existed (empty string) must be just as replaceable
	// as a freshly-defaulted "prescribed" one, not accidentally treated as
	// permanently protected because it doesn't literally match "prescribed".
	if _, err := tx.ExecContext(ctx, `DELETE FROM medications WHERE document_id = ? AND status NOT IN (?, ?, ?)`,
		documentID,
		string(normalization.MedicationStatusActive), string(normalization.MedicationStatusDiscontinued), string(normalization.MedicationStatusCompleted),
	); err != nil {
		return fmt.Errorf("storage: replace medications: delete: %w", err)
	}

	// Whatever's left for this document (only rows already past
	// "prescribed") must not be duplicated by the fresh extraction below.
	survivingRows, err := tx.QueryContext(ctx, `SELECT drug_name FROM medications WHERE document_id = ?`, documentID)
	if err != nil {
		return fmt.Errorf("storage: replace medications: list surviving: %w", err)
	}
	surviving := make(map[string]bool)
	for survivingRows.Next() {
		var name string
		if err := survivingRows.Scan(&name); err != nil {
			_ = survivingRows.Close()
			return fmt.Errorf("storage: replace medications: scan surviving: %w", err)
		}
		surviving[dedupKey(name)] = true
	}
	if err := survivingRows.Err(); err != nil {
		_ = survivingRows.Close()
		return fmt.Errorf("storage: replace medications: iterate surviving: %w", err)
	}
	_ = survivingRows.Close()

	for _, m := range meds {
		if surviving[dedupKey(m.DrugName)] {
			continue
		}
		// Always mint a fresh id, ignoring whatever id normalization
		// assigned — a surviving row from the same document may still
		// occupy that same deterministic id (see the delete above), so
		// reusing it here could collide with that surviving row's primary
		// key (mirrors DiagnosisRepository.ReplaceForDocument).
		id := "med_" + uuid.New().String()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO medications (id, user_id, document_id, drug_name, dose_amount, dose_unit, frequency, route, started_at, ended_at, status, reason, prescribed_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, m.UserID, documentID, m.DrugName, m.DoseAmount, m.DoseUnit, m.Frequency, m.Route,
			nullUnix(m.StartedAt), nullUnix(m.EndedAt), string(m.Status), m.Reason, m.PrescribedBy,
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

// MarkStartedManually implements MedicationRepository.MarkStartedManually.
func (r *sqliteMedicationRepository) MarkStartedManually(ctx context.Context, id, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE medications SET status = ?, started_at = ? WHERE id = ? AND user_id = ?`,
		string(normalization.MedicationStatusActive), at.Unix(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark medication started manually: %w", err)
	}
	return nil
}

// MarkEndedManually implements MedicationRepository.MarkEndedManually.
func (r *sqliteMedicationRepository) MarkEndedManually(ctx context.Context, id, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE medications SET status = ?, ended_at = ? WHERE id = ? AND user_id = ?`,
		string(normalization.MedicationStatusCompleted), at.Unix(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark medication ended manually: %w", err)
	}
	return nil
}
