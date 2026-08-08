package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// ProcedureRepository mirrors docs/domain/08-procedure.md §5.
type ProcedureRepository interface {
	Add(ctx context.Context, p normalization.Procedure) error
	ListByUser(ctx context.Context, userID string) ([]normalization.Procedure, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Procedure, error)
	ReplaceForDocument(ctx context.Context, documentID string, procedures []normalization.Procedure) error
	// ListVaccinations returns every Procedure with Type == "vaccination"
	// for userID — used by ProfileBuilder (see
	// docs/domain/05-medical-profile.md §2, docs/domain/08-procedure.md §3).
	ListVaccinations(ctx context.Context, userID string) ([]normalization.Procedure, error)
}

type sqliteProcedureRepository struct {
	db *sql.DB
}

// NewProcedureRepository builds a ProcedureRepository backed by s.
func NewProcedureRepository(s *Store) ProcedureRepository {
	return &sqliteProcedureRepository{db: s.db}
}

const procedureSelectColumns = `SELECT id, user_id, document_id, type, name, performed_at, performed_by, notes`

func (r *sqliteProcedureRepository) Add(ctx context.Context, p normalization.Procedure) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO procedures (id, user_id, document_id, type, name, performed_at, performed_by, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserID, p.DocumentID, p.Type, p.Name, nullUnix(p.PerformedAt), p.PerformedBy, p.Notes,
	)
	if err != nil {
		return fmt.Errorf("storage: add procedure: %w", err)
	}
	return nil
}

func (r *sqliteProcedureRepository) ListByUser(ctx context.Context, userID string) ([]normalization.Procedure, error) {
	rows, err := r.db.QueryContext(ctx, procedureSelectColumns+` FROM procedures WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list procedures by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProcedures(rows)
}

func (r *sqliteProcedureRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.Procedure, error) {
	rows, err := r.db.QueryContext(ctx, procedureSelectColumns+` FROM procedures WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list procedures by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProcedures(rows)
}

func (r *sqliteProcedureRepository) ReplaceForDocument(ctx context.Context, documentID string, procedures []normalization.Procedure) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace procedures: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM procedures WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace procedures: delete: %w", err)
	}
	for _, p := range procedures {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO procedures (id, user_id, document_id, type, name, performed_at, performed_by, notes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.UserID, documentID, p.Type, p.Name, nullUnix(p.PerformedAt), p.PerformedBy, p.Notes,
		)
		if err != nil {
			return fmt.Errorf("storage: replace procedures: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace procedures: commit: %w", err)
	}
	return nil
}

func (r *sqliteProcedureRepository) ListVaccinations(ctx context.Context, userID string) ([]normalization.Procedure, error) {
	rows, err := r.db.QueryContext(ctx, procedureSelectColumns+` FROM procedures WHERE user_id = ? AND type = 'vaccination'`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list vaccinations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProcedures(rows)
}

func scanProcedures(rows *sql.Rows) ([]normalization.Procedure, error) {
	var result []normalization.Procedure
	for rows.Next() {
		var p normalization.Procedure
		var performedAt sql.NullInt64
		if err := rows.Scan(&p.ID, &p.UserID, &p.DocumentID, &p.Type, &p.Name, &performedAt, &p.PerformedBy, &p.Notes); err != nil {
			return nil, fmt.Errorf("storage: scan procedure: %w", err)
		}
		p.PerformedAt = timePtr(performedAt)
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate procedures: %w", err)
	}
	return result, nil
}
