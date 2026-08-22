package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// DiagnosisRepository mirrors docs/domain/07-diagnosis-and-allergy.md §5.
type DiagnosisRepository interface {
	Add(ctx context.Context, d normalization.Diagnosis) error
	ListByUser(ctx context.Context, userID string) ([]normalization.Diagnosis, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.Diagnosis, error)
	// ReplaceForDocument deletes every existing Diagnosis for documentID
	// that's still in its default, untouched-by-the-user state
	// (ActualResolutionAt == nil) and inserts diagnoses in its place, all
	// in one transaction. A diagnosis the user has directly confirmed
	// resolved via medical.resolve_diagnosis (ActualResolutionAt set — see
	// docs/domain/07-diagnosis-and-allergy.md §"actualResolutionAt") is left
	// untouched rather than being silently reset by a reprocess of the
	// document it came from — mirrors PlannedAction.ReplaceForSource's
	// status-based rule (docs/adr/004-planned-actions.md §4), simplified to
	// the same "delete only what's still default" shape rather than
	// reconciling by content key. Deliberately keyed on ActualResolutionAt,
	// not Status directly (unlike MedicationRepository.ReplaceForDocument):
	// Diagnosis.Status can reach "resolved" from Extraction alone, with no
	// user involvement, so a pure status check would wrongly freeze such a
	// diagnosis from ever being corrected by a smarter reprocess.
	// Diagnoses whose (canonicalized, case/whitespace-insensitive via
	// dedupKey) Name still has a surviving row for this document are
	// skipped rather than inserted again — the surviving row already
	// represents that diagnosis for this document, and a fresh extraction
	// re-describing the same diagnosis must not produce a duplicate.
	ReplaceForDocument(ctx context.Context, documentID string, diagnoses []normalization.Diagnosis) error
	// MarkResolved sets id's status to "resolved" and ActualResolutionAt to
	// at — medical.resolve_diagnosis's write, the one way Diagnosis.Status
	// changes outside of Extraction (see docs/domain/07-diagnosis-and-allergy.md).
	// Deliberately leaves ExpectedResolutionFrom/To untouched: they record
	// what was estimated beforehand, ActualResolutionAt records what
	// actually happened, and overwriting one with the other would lose
	// that distinction. reasoning replaces StatusReasoning so it reflects
	// why the status is what it now is, not why some earlier Extraction
	// picked whatever it had before.
	MarkResolved(ctx context.Context, id, userID string, at time.Time, reasoning string) error
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
		INSERT INTO diagnoses (id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning, actual_resolution_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.DocumentID, d.Name, d.Code, d.CodeSystem, nullUnix(d.DiagnosedAt), d.Status, d.Notes,
		nullUnix(d.ExpectedResolutionFrom), nullUnix(d.ExpectedResolutionTo), d.StatusReasoning, nullUnix(d.ActualResolutionAt),
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

	if _, err := tx.ExecContext(ctx, `DELETE FROM diagnoses WHERE document_id = ? AND actual_resolution_at IS NULL`, documentID); err != nil {
		return fmt.Errorf("storage: replace diagnoses: delete: %w", err)
	}

	// Whatever's left for this document (only user-resolved rows) must not
	// be duplicated by the fresh extraction below.
	survivingRows, err := tx.QueryContext(ctx, `SELECT name FROM diagnoses WHERE document_id = ?`, documentID)
	if err != nil {
		return fmt.Errorf("storage: replace diagnoses: list surviving: %w", err)
	}
	surviving := make(map[string]bool)
	for survivingRows.Next() {
		var name string
		if err := survivingRows.Scan(&name); err != nil {
			_ = survivingRows.Close()
			return fmt.Errorf("storage: replace diagnoses: scan surviving: %w", err)
		}
		surviving[dedupKey(name)] = true
	}
	if err := survivingRows.Err(); err != nil {
		_ = survivingRows.Close()
		return fmt.Errorf("storage: replace diagnoses: iterate surviving: %w", err)
	}
	_ = survivingRows.Close()

	for _, d := range diagnoses {
		if surviving[dedupKey(d.Name)] {
			continue
		}
		// Always mint a fresh id, ignoring whatever id normalization
		// assigned (its deterministic "dx_<documentID>_<i>" scheme) — a
		// user-resolved row from the same document may still occupy that
		// same deterministic id (see the delete above), so reusing it here
		// could collide with that surviving row's primary key.
		id := "dx_" + uuid.New().String()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO diagnoses (id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning, actual_resolution_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, d.UserID, documentID, d.Name, d.Code, d.CodeSystem, nullUnix(d.DiagnosedAt), d.Status, d.Notes,
			nullUnix(d.ExpectedResolutionFrom), nullUnix(d.ExpectedResolutionTo), d.StatusReasoning, nullUnix(d.ActualResolutionAt),
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

const diagnosisSelectColumns = `SELECT id, user_id, document_id, name, code, code_system, diagnosed_at, status, notes, expected_resolution_from, expected_resolution_to, status_reasoning, actual_resolution_at`

func scanDiagnoses(rows *sql.Rows) ([]normalization.Diagnosis, error) {
	var result []normalization.Diagnosis
	for rows.Next() {
		var d normalization.Diagnosis
		var diagnosedAt, resolutionFrom, resolutionTo, actualResolutionAt sql.NullInt64
		if err := rows.Scan(&d.ID, &d.UserID, &d.DocumentID, &d.Name, &d.Code, &d.CodeSystem, &diagnosedAt, &d.Status, &d.Notes, &resolutionFrom, &resolutionTo, &d.StatusReasoning, &actualResolutionAt); err != nil {
			return nil, fmt.Errorf("storage: scan diagnosis: %w", err)
		}
		d.DiagnosedAt = timePtr(diagnosedAt)
		d.ExpectedResolutionFrom = timePtr(resolutionFrom)
		d.ExpectedResolutionTo = timePtr(resolutionTo)
		d.ActualResolutionAt = timePtr(actualResolutionAt)
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate diagnoses: %w", err)
	}
	return result, nil
}

func (r *sqliteDiagnosisRepository) MarkResolved(ctx context.Context, id, userID string, at time.Time, reasoning string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE diagnoses SET status = 'resolved', actual_resolution_at = ?, status_reasoning = ? WHERE id = ? AND user_id = ?`,
		at.Unix(), reasoning, id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark diagnosis resolved: %w", err)
	}
	return nil
}
