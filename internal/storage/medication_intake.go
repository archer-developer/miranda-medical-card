package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MedicationIntake mirrors docs/domain/12-self-reported-events.md §4 — a
// single instantaneous "took a dose" fact, distinct from Medication (a
// prescribed course with a status).
type MedicationIntake struct {
	ID         string
	UserID     string
	SourceType string // "self_reported" | "document"
	SourceID   string
	DrugName   string
	DoseAmount float64
	DoseUnit   string
	TakenAt    time.Time
	Reason     string
}

// DateRange narrows MedicationIntakeRepository.ListByUser, mirroring
// docs/domain/11-value-objects.md §5.
type DateRange struct {
	From *time.Time
	To   *time.Time
}

// MedicationIntakeRepository mirrors docs/domain/12-self-reported-events.md
// §7.
type MedicationIntakeRepository interface {
	Add(ctx context.Context, i MedicationIntake) (MedicationIntake, error)
	ListByUser(ctx context.Context, userID string, filter DateRange) ([]MedicationIntake, error)
	RemoveBySource(ctx context.Context, sourceType, sourceID string) error
}

type sqliteMedicationIntakeRepository struct {
	db *sql.DB
}

// NewMedicationIntakeRepository builds a MedicationIntakeRepository backed
// by s.
func NewMedicationIntakeRepository(s *Store) MedicationIntakeRepository {
	return &sqliteMedicationIntakeRepository{db: s.db}
}

const medicationIntakeSelectColumns = `SELECT id, user_id, source_type, source_id, drug_name, dose_amount, dose_unit, taken_at, reason`

func (r *sqliteMedicationIntakeRepository) Add(ctx context.Context, i MedicationIntake) (MedicationIntake, error) {
	if i.ID == "" {
		i.ID = "intake_" + uuid.New().String()
	}
	if i.TakenAt.IsZero() {
		i.TakenAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO medication_intakes (id, user_id, source_type, source_id, drug_name, dose_amount, dose_unit, taken_at, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.UserID, i.SourceType, i.SourceID, i.DrugName, i.DoseAmount, i.DoseUnit, i.TakenAt.Unix(), i.Reason,
	)
	if err != nil {
		return MedicationIntake{}, fmt.Errorf("storage: add medication intake: %w", err)
	}
	return i, nil
}

func (r *sqliteMedicationIntakeRepository) ListByUser(ctx context.Context, userID string, filter DateRange) ([]MedicationIntake, error) {
	query := medicationIntakeSelectColumns + ` FROM medication_intakes WHERE user_id = ?`
	args := []any{userID}
	if filter.From != nil {
		query += ` AND taken_at >= ?`
		args = append(args, filter.From.Unix())
	}
	if filter.To != nil {
		query += ` AND taken_at <= ?`
		args = append(args, filter.To.Unix())
	}
	query += ` ORDER BY taken_at ASC`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list medication intakes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []MedicationIntake
	for rows.Next() {
		var i MedicationIntake
		var takenAt int64
		if err := rows.Scan(&i.ID, &i.UserID, &i.SourceType, &i.SourceID, &i.DrugName, &i.DoseAmount, &i.DoseUnit, &takenAt, &i.Reason); err != nil {
			return nil, fmt.Errorf("storage: scan medication intake: %w", err)
		}
		i.TakenAt = time.Unix(takenAt, 0).UTC()
		result = append(result, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate medication intakes: %w", err)
	}
	return result, nil
}

func (r *sqliteMedicationIntakeRepository) RemoveBySource(ctx context.Context, sourceType, sourceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM medication_intakes WHERE source_type = ? AND source_id = ?`, sourceType, sourceID)
	if err != nil {
		return fmt.Errorf("storage: remove medication intake by source: %w", err)
	}
	return nil
}
