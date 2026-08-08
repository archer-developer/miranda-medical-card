package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// AllergyRepository mirrors docs/domain/07-diagnosis-and-allergy.md §5
// ("структура методов идентична MedicationRepository" minus the filter
// parameter — Allergy has no status field to filter on).
type AllergyRepository interface {
	Add(ctx context.Context, a normalization.Allergy) error
	ListByUser(ctx context.Context, userID string) ([]normalization.Allergy, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Allergy, error)
	ReplaceForDocument(ctx context.Context, documentID string, allergies []normalization.Allergy) error
}

type sqliteAllergyRepository struct {
	db *sql.DB
}

// NewAllergyRepository builds an AllergyRepository backed by s.
func NewAllergyRepository(s *Store) AllergyRepository {
	return &sqliteAllergyRepository{db: s.db}
}

const allergySelectColumns = `SELECT id, user_id, document_id, substance, reaction, severity`

func (r *sqliteAllergyRepository) Add(ctx context.Context, a normalization.Allergy) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO allergies (id, user_id, document_id, substance, reaction, severity)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.DocumentID, a.Substance, a.Reaction, a.Severity,
	)
	if err != nil {
		return fmt.Errorf("storage: add allergy: %w", err)
	}
	return nil
}

func (r *sqliteAllergyRepository) ListByUser(ctx context.Context, userID string) ([]normalization.Allergy, error) {
	rows, err := r.db.QueryContext(ctx, allergySelectColumns+` FROM allergies WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list allergies by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAllergies(rows)
}

func (r *sqliteAllergyRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.Allergy, error) {
	rows, err := r.db.QueryContext(ctx, allergySelectColumns+` FROM allergies WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list allergies by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAllergies(rows)
}

func (r *sqliteAllergyRepository) ReplaceForDocument(ctx context.Context, documentID string, allergies []normalization.Allergy) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace allergies: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM allergies WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace allergies: delete: %w", err)
	}
	for _, a := range allergies {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO allergies (id, user_id, document_id, substance, reaction, severity)
			VALUES (?, ?, ?, ?, ?, ?)`,
			a.ID, a.UserID, documentID, a.Substance, a.Reaction, a.Severity,
		)
		if err != nil {
			return fmt.Errorf("storage: replace allergies: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace allergies: commit: %w", err)
	}
	return nil
}

func scanAllergies(rows *sql.Rows) ([]normalization.Allergy, error) {
	var result []normalization.Allergy
	for rows.Next() {
		var a normalization.Allergy
		if err := rows.Scan(&a.ID, &a.UserID, &a.DocumentID, &a.Substance, &a.Reaction, &a.Severity); err != nil {
			return nil, fmt.Errorf("storage: scan allergy: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate allergies: %w", err)
	}
	return result, nil
}
