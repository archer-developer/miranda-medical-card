package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// VitalSignRepository mirrors docs/domain/09-lab-result-and-vital-sign.md §6.
type VitalSignRepository interface {
	Add(ctx context.Context, v normalization.VitalSign) error
	ListByUser(ctx context.Context, userID string) ([]normalization.VitalSign, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.VitalSign, error)
	ReplaceForDocument(ctx context.Context, documentID string, signs []normalization.VitalSign) error
	// LatestByType returns, for each Type userID has at least one
	// VitalSign for, the single most recent one (by MeasuredAt) — used by
	// ProfileBuilder (see docs/domain/05-medical-profile.md §3).
	LatestByType(ctx context.Context, userID string) (map[string]normalization.VitalSign, error)
}

type sqliteVitalSignRepository struct {
	db *sql.DB
}

// NewVitalSignRepository builds a VitalSignRepository backed by s.
func NewVitalSignRepository(s *Store) VitalSignRepository {
	return &sqliteVitalSignRepository{db: s.db}
}

const vitalSignSelectColumns = `SELECT id, user_id, document_id, type, systolic, diastolic, value, unit, measured_at`

func (r *sqliteVitalSignRepository) Add(ctx context.Context, v normalization.VitalSign) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO vital_signs (id, user_id, document_id, type, systolic, diastolic, value, unit, measured_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.UserID, v.DocumentID, v.Type, v.Systolic, v.Diastolic, v.Value, v.Unit, nullUnix(v.MeasuredAt),
	)
	if err != nil {
		return fmt.Errorf("storage: add vital sign: %w", err)
	}
	return nil
}

func (r *sqliteVitalSignRepository) ListByUser(ctx context.Context, userID string) ([]normalization.VitalSign, error) {
	rows, err := r.db.QueryContext(ctx, vitalSignSelectColumns+` FROM vital_signs WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list vital signs by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVitalSigns(rows)
}

func (r *sqliteVitalSignRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.VitalSign, error) {
	rows, err := r.db.QueryContext(ctx, vitalSignSelectColumns+` FROM vital_signs WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list vital signs by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVitalSigns(rows)
}

func (r *sqliteVitalSignRepository) ReplaceForDocument(ctx context.Context, documentID string, signs []normalization.VitalSign) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace vital signs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM vital_signs WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace vital signs: delete: %w", err)
	}
	for _, v := range signs {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO vital_signs (id, user_id, document_id, type, systolic, diastolic, value, unit, measured_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.ID, v.UserID, documentID, v.Type, v.Systolic, v.Diastolic, v.Value, v.Unit, nullUnix(v.MeasuredAt),
		)
		if err != nil {
			return fmt.Errorf("storage: replace vital signs: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace vital signs: commit: %w", err)
	}
	return nil
}

func (r *sqliteVitalSignRepository) LatestByType(ctx context.Context, userID string) (map[string]normalization.VitalSign, error) {
	all, err := r.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]normalization.VitalSign)
	for _, v := range all {
		current, ok := latest[v.Type]
		if !ok || isAfter(v.MeasuredAt, current.MeasuredAt) {
			latest[v.Type] = v
		}
	}
	return latest, nil
}

func scanVitalSigns(rows *sql.Rows) ([]normalization.VitalSign, error) {
	var result []normalization.VitalSign
	for rows.Next() {
		var v normalization.VitalSign
		var measuredAt sql.NullInt64
		if err := rows.Scan(&v.ID, &v.UserID, &v.DocumentID, &v.Type, &v.Systolic, &v.Diastolic, &v.Value, &v.Unit, &measuredAt); err != nil {
			return nil, fmt.Errorf("storage: scan vital sign: %w", err)
		}
		v.MeasuredAt = timePtr(measuredAt)
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate vital signs: %w", err)
	}
	return result, nil
}
