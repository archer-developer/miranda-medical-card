package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SelfReportedEvent mirrors docs/domain/12-self-reported-events.md §3.
type SelfReportedEvent struct {
	ID                 string
	UserID             string
	RawText            string
	OccurredAt         time.Time
	LoggedAt           time.Time
	Status             string // DocumentStatusPending/Running/Ready/Failed
	Category           string
	Description        string
	MedicationIntakeID string
}

// SelfReportedEventRepository mirrors docs/domain/12-self-reported-events.md
// §7 — UpdateStatus additionally takes userID (the documented signature
// omits it, but every other write operation in this domain is scoped by
// owner; treated here as a documentation gap, not a deliberate omission).
type SelfReportedEventRepository interface {
	Add(ctx context.Context, e SelfReportedEvent) (SelfReportedEvent, error)
	Get(ctx context.Context, id, userID string) (SelfReportedEvent, error)
	UpdateStatus(ctx context.Context, id, userID, status string) error
	// UpdateExtracted sets Category/Description/MedicationIntakeID once
	// Structured Extraction completes — mirrors
	// DocumentRepository.UpdateExtracted's reasoning for being separate
	// from UpdateStatus.
	UpdateExtracted(ctx context.Context, id, userID, category, description, medicationIntakeID string) error
	Remove(ctx context.Context, id, userID string) error
}

type sqliteSelfReportedEventRepository struct {
	db *sql.DB
}

// NewSelfReportedEventRepository builds a SelfReportedEventRepository
// backed by s.
func NewSelfReportedEventRepository(s *Store) SelfReportedEventRepository {
	return &sqliteSelfReportedEventRepository{db: s.db}
}

const selfReportedEventSelectColumns = `SELECT id, user_id, raw_text, occurred_at, logged_at, status, category, description, medication_intake_id`

func (r *sqliteSelfReportedEventRepository) Add(ctx context.Context, e SelfReportedEvent) (SelfReportedEvent, error) {
	if e.ID == "" {
		e.ID = "selfevt_" + uuid.New().String()
	}
	if e.LoggedAt.IsZero() {
		e.LoggedAt = time.Now().UTC()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = e.LoggedAt
	}
	if e.Status == "" {
		e.Status = DocumentStatusPending
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO self_reported_events (id, user_id, raw_text, occurred_at, logged_at, status, category, description, medication_intake_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.UserID, e.RawText, e.OccurredAt.Unix(), e.LoggedAt.Unix(), e.Status, e.Category, e.Description, e.MedicationIntakeID,
	)
	if err != nil {
		return SelfReportedEvent{}, fmt.Errorf("storage: add self-reported event: %w", err)
	}
	return e, nil
}

func (r *sqliteSelfReportedEventRepository) Get(ctx context.Context, id, userID string) (SelfReportedEvent, error) {
	row := r.db.QueryRowContext(ctx, selfReportedEventSelectColumns+` FROM self_reported_events WHERE id = ? AND user_id = ?`, id, userID)
	e, err := scanSelfReportedEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SelfReportedEvent{}, ErrNotFound
	}
	return e, err
}

func (r *sqliteSelfReportedEventRepository) UpdateStatus(ctx context.Context, id, userID, status string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE self_reported_events SET status = ? WHERE id = ? AND user_id = ?`, status, id, userID)
	if err != nil {
		return fmt.Errorf("storage: update self-reported event status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update self-reported event status: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteSelfReportedEventRepository) UpdateExtracted(ctx context.Context, id, userID, category, description, medicationIntakeID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE self_reported_events SET category = ?, description = ?, medication_intake_id = ? WHERE id = ? AND user_id = ?`,
		category, description, medicationIntakeID, id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: update self-reported event extracted fields: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update self-reported event extracted fields: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqliteSelfReportedEventRepository) Remove(ctx context.Context, id, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM self_reported_events WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("storage: remove self-reported event: %w", err)
	}
	return nil
}

func scanSelfReportedEvent(row *sql.Row) (SelfReportedEvent, error) {
	var e SelfReportedEvent
	var occurredAt, loggedAt int64
	err := row.Scan(&e.ID, &e.UserID, &e.RawText, &occurredAt, &loggedAt, &e.Status, &e.Category, &e.Description, &e.MedicationIntakeID)
	if err != nil {
		return SelfReportedEvent{}, err
	}
	e.OccurredAt = time.Unix(occurredAt, 0).UTC()
	e.LoggedAt = time.Unix(loggedAt, 0).UTC()
	return e, nil
}
