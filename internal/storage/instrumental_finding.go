package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// InstrumentalFindingRepository mirrors docs/domain/13-instrumental-finding.md
// §8. Deliberately has no ListByUser, unlike the other repositories in this
// package — InstrumentalFinding doesn't participate in MedicalProfile (see
// that doc's §7), so there's no documented caller that needs "every finding
// for a user" as opposed to one document's or one (structure, parameter)'s.
type InstrumentalFindingRepository interface {
	Add(ctx context.Context, f normalization.InstrumentalFinding) error
	ListByDocument(ctx context.Context, documentID string) ([]normalization.InstrumentalFinding, error)
	ReplaceForDocument(ctx context.Context, documentID string, findings []normalization.InstrumentalFinding) error
	// HistoryByStructureParameter returns every finding userID has for
	// (structure, parameter), oldest first — used by Instrumental
	// Findings Provider (see docs/domain/13-instrumental-finding.md §9) to
	// answer trend questions like "how has liver size changed."
	HistoryByStructureParameter(ctx context.Context, userID, structure, parameter string) ([]normalization.InstrumentalFinding, error)
}

type sqliteInstrumentalFindingRepository struct {
	db *sql.DB
}

// NewInstrumentalFindingRepository builds an InstrumentalFindingRepository
// backed by s.
func NewInstrumentalFindingRepository(s *Store) InstrumentalFindingRepository {
	return &sqliteInstrumentalFindingRepository{db: s.db}
}

const instrumentalFindingSelectColumns = `SELECT id, user_id, document_id, procedure_id, structure, parameter, value, unit, normalized_value, normalized_unit, qualitative_value, reference_low, reference_high, measured_at`

func (r *sqliteInstrumentalFindingRepository) Add(ctx context.Context, f normalization.InstrumentalFinding) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instrumental_findings (id, user_id, document_id, procedure_id, structure, parameter, value, unit, normalized_value, normalized_unit, qualitative_value, reference_low, reference_high, measured_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.UserID, f.DocumentID, f.ProcedureID, f.Structure, f.Parameter, f.Value, f.Unit,
		f.NormalizedValue, f.NormalizedUnit, f.QualitativeValue, f.ReferenceLow, f.ReferenceHigh, nullUnix(f.MeasuredAt),
	)
	if err != nil {
		return fmt.Errorf("storage: add instrumental finding: %w", err)
	}
	return nil
}

func (r *sqliteInstrumentalFindingRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.InstrumentalFinding, error) {
	rows, err := r.db.QueryContext(ctx, instrumentalFindingSelectColumns+` FROM instrumental_findings WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list instrumental findings by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanInstrumentalFindings(rows)
}

func (r *sqliteInstrumentalFindingRepository) ReplaceForDocument(ctx context.Context, documentID string, findings []normalization.InstrumentalFinding) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace instrumental findings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM instrumental_findings WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace instrumental findings: delete: %w", err)
	}
	for _, f := range findings {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO instrumental_findings (id, user_id, document_id, procedure_id, structure, parameter, value, unit, normalized_value, normalized_unit, qualitative_value, reference_low, reference_high, measured_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.UserID, documentID, f.ProcedureID, f.Structure, f.Parameter, f.Value, f.Unit,
			f.NormalizedValue, f.NormalizedUnit, f.QualitativeValue, f.ReferenceLow, f.ReferenceHigh, nullUnix(f.MeasuredAt),
		)
		if err != nil {
			return fmt.Errorf("storage: replace instrumental findings: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace instrumental findings: commit: %w", err)
	}
	return nil
}

func (r *sqliteInstrumentalFindingRepository) HistoryByStructureParameter(ctx context.Context, userID, structure, parameter string) ([]normalization.InstrumentalFinding, error) {
	rows, err := r.db.QueryContext(ctx,
		instrumentalFindingSelectColumns+` FROM instrumental_findings WHERE user_id = ? AND structure = ? AND parameter = ? ORDER BY measured_at ASC`,
		userID, structure, parameter,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: history instrumental findings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanInstrumentalFindings(rows)
}

func scanInstrumentalFindings(rows *sql.Rows) ([]normalization.InstrumentalFinding, error) {
	var result []normalization.InstrumentalFinding
	for rows.Next() {
		var f normalization.InstrumentalFinding
		var measuredAt sql.NullInt64
		if err := rows.Scan(&f.ID, &f.UserID, &f.DocumentID, &f.ProcedureID, &f.Structure, &f.Parameter, &f.Value, &f.Unit,
			&f.NormalizedValue, &f.NormalizedUnit, &f.QualitativeValue, &f.ReferenceLow, &f.ReferenceHigh, &measuredAt); err != nil {
			return nil, fmt.Errorf("storage: scan instrumental finding: %w", err)
		}
		f.MeasuredAt = timePtr(measuredAt)
		result = append(result, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate instrumental findings: %w", err)
	}
	return result, nil
}
