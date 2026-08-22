package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// PlannedActionRepository mirrors docs/domain/14-planned-action.md §5 /
// docs/adr/004-planned-actions.md.
type PlannedActionRepository interface {
	// Add inserts a single PlannedAction — used by the self-reported path
	// (internal/pipeline.LogEvent), where SelfReportedEvent.rawText is
	// immutable (no re-log, only delete+relog per docs/mcp/07-events.md
	// §5), so there's never more than one PlannedAction to persist per
	// call, unlike the document path's ReplaceForSource. Generates a uuid
	// ID when a.ID is empty.
	Add(ctx context.Context, a normalization.PlannedAction) (normalization.PlannedAction, error)
	// ReplaceForSource reconciles (documentID, "document") sourced actions
	// with actions by status, not by matching individual rows — NOT a blind
	// delete+reinsert like every other entity's ReplaceForDocument, because
	// PlannedAction.Status/MatchedDocumentID carries state that must survive
	// a reprocess of the document that created it (see
	// docs/adr/004-planned-actions.md §4). Every still-pending row for
	// (sourceType, sourceID) — i.e. every row still in its default,
	// untouched-by-the-user-or-by-automatic-matching state — is deleted and
	// every action is inserted fresh as a new pending row; a row that's
	// moved past pending (completed/declined) is left alone, untouched,
	// rather than reconciled against the new extraction. This deliberately
	// does not try to match an existing row to a new one by content (e.g.
	// (Type, MatchIndicatorName|MatchProcedureName)) — Structured
	// Extraction's output for the same document isn't guaranteed identical
	// across runs (see docs/architecture/02-processing-pipeline.md §11), so
	// a content key is not a reliable identity to reconcile on; status is.
	// The cost is a rare, harmless-looking duplicate (an old completed row
	// sitting next to a freshly re-extracted pending row for the same
	// recommendation) instead of silent data loss or a fragile match.
	//
	// Also skips creating a new row when userID already has a *different*
	// source's still-pending row with the same key (see
	// docs/adr/005-planned-action-cross-source-dedup.md) — otherwise two
	// documents independently recommending the same thing (e.g. two
	// consultations both extracting to "Консультация эндокринолога") mint
	// two textually-identical pending rows, which broke
	// medical.decline_planned_action's LLM-based text matching in
	// production (it can't tell which of two identical candidates the user
	// means, so it refuses to guess). This dedup is intentionally
	// one-directional: it only prevents ReplaceForSource (the document
	// path) from duplicating a row that already exists from any source; it
	// does not retroactively touch Add's self-reported path (see ADR §3).
	ReplaceForSource(ctx context.Context, sourceType, sourceID string, actions []normalization.PlannedAction) error
	// RemoveBySource deletes every action for (sourceType, sourceID) — used
	// by medical.delete_event to clean up a self-reported PlannedAction the
	// same way it already cleans up a MedicationIntake.
	RemoveBySource(ctx context.Context, sourceType, sourceID string) error
	ListByUser(ctx context.Context, userID string) ([]normalization.PlannedAction, error)
	// ListPending returns userID's pending (not completed/declined)
	// actions — internal/planmatch's candidate set, and
	// medical.decline_planned_action's candidate set.
	ListPending(ctx context.Context, userID string) ([]normalization.PlannedAction, error)
	// ClearMatchesFromDocument reverts every action currently completed by
	// documentID back to pending — called unconditionally at the start of
	// processing any document (upload or reprocess), before rematching, so
	// a reprocess whose new extraction no longer produces the matching
	// result doesn't leave a stale completion behind (see
	// docs/adr/004-planned-actions.md §4). A no-op for a first-time upload.
	ClearMatchesFromDocument(ctx context.Context, documentID string) error
	// MarkCompleted marks id completed with a backlink to whatever
	// normalized entity satisfied it — called only by internal/planmatch's
	// pipeline hook against ids already fetched via ListPending.
	MarkCompleted(ctx context.Context, id, matchedDocumentID, matchedEntityID string, at time.Time) error
	// MarkDeclined marks id declined — medical.decline_planned_action.
	// Scoped by userID like every other write in this domain.
	MarkDeclined(ctx context.Context, id, userID string) error
	// MarkCompletedManually marks id completed by the user's own
	// confirmation in dialogue — medical.complete_planned_action. Unlike
	// MarkCompleted (automatic document matching), this is scoped by userID
	// like every other write in this domain, and leaves matched_document_id/
	// matched_entity_id untouched (there's no closing document/entity, only
	// a confirmation moment in matched_at).
	MarkCompletedManually(ctx context.Context, id, userID string, at time.Time) error
}

type sqlitePlannedActionRepository struct {
	db *sql.DB
}

// NewPlannedActionRepository builds a PlannedActionRepository backed by s.
func NewPlannedActionRepository(s *Store) PlannedActionRepository {
	return &sqlitePlannedActionRepository{db: s.db}
}

const plannedActionSelectColumns = `SELECT id, user_id, source_type, source_id, type, description, reference_text,
	match_indicator_name, match_procedure_name, due_date_from, due_date_to, status,
	matched_document_id, matched_entity_id, matched_at`

func (r *sqlitePlannedActionRepository) Add(ctx context.Context, a normalization.PlannedAction) (normalization.PlannedAction, error) {
	if a.ID == "" {
		a.ID = "plan_" + uuid.New().String()
	}
	if a.Status == "" {
		a.Status = normalization.PlannedActionStatusPending
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO planned_actions (id, user_id, source_type, source_id, type, description, reference_text,
			match_indicator_name, match_procedure_name, due_date_from, due_date_to, status,
			matched_document_id, matched_entity_id, matched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.SourceType, a.SourceID, a.Type, a.Description, a.ReferenceText,
		a.MatchIndicatorName, a.MatchProcedureName, nullUnix(a.DueDateFrom), nullUnix(a.DueDateTo), a.Status,
		a.MatchedDocumentID, a.MatchedEntityID, nullUnix(a.MatchedAt),
	)
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("storage: add planned action: %w", err)
	}
	return a, nil
}

// planActionKey is ReplaceForSource/internal/planmatch's shared
// reconciliation/matching key — see docs/adr/004-planned-actions.md §4 for
// why it must be identical between the two.
func planActionKey(a normalization.PlannedAction) string {
	if a.Type == "lab_test" {
		return "lab_test|" + a.MatchIndicatorName
	}
	return a.Type + "|" + a.MatchProcedureName
}

// hasKeyIdentity reports whether a's reconciliation key carries any actual
// identifying information. An empty MatchIndicatorName/MatchProcedureName
// (extraction produced no canonical name) must never be treated as a dedup
// match against another equally-nameless action — see
// docs/adr/005-planned-action-cross-source-dedup.md §2. Mirrors
// internal/planmatch.Match's identical guard against the same
// false-positive risk.
func hasKeyIdentity(a normalization.PlannedAction) bool {
	if a.Type == "lab_test" {
		return a.MatchIndicatorName != ""
	}
	return a.MatchProcedureName != ""
}

// pendingKeysFromOtherSources returns the planActionKey set of userID's
// currently pending PlannedActions that do NOT belong to
// (sourceType, sourceID) — used by ReplaceForSource to detect when a
// different source already represents the same recommendation, so it isn't
// duplicated. See docs/adr/005-planned-action-cross-source-dedup.md.
func pendingKeysFromOtherSources(ctx context.Context, tx *sql.Tx, userID, sourceType, sourceID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, plannedActionSelectColumns+`
		FROM planned_actions
		WHERE user_id = ? AND status = ? AND NOT (source_type = ? AND source_id = ?)`,
		userID, normalization.PlannedActionStatusPending, sourceType, sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending from other sources: %w", err)
	}
	others, err := scanPlannedActions(rows)
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list pending from other sources: %w", err)
	}
	keys := make(map[string]bool, len(others))
	for _, o := range others {
		if hasKeyIdentity(o) {
			keys[planActionKey(o)] = true
		}
	}
	return keys, nil
}

func (r *sqlitePlannedActionRepository) ReplaceForSource(ctx context.Context, sourceType, sourceID string, actions []normalization.PlannedAction) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace planned actions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Only a still-pending row is in the default state ReplaceForSource is
	// free to discard — a completed/declined row is left untouched as
	// historical fact (see ReplaceForSource's doc comment).
	if _, err := tx.ExecContext(ctx, `DELETE FROM planned_actions WHERE source_type = ? AND source_id = ? AND status = ?`,
		sourceType, sourceID, normalization.PlannedActionStatusPending,
	); err != nil {
		return fmt.Errorf("storage: replace planned actions: delete: %w", err)
	}

	// dedupKeys is userID's other-source pending keys — see
	// docs/adr/005-planned-action-cross-source-dedup.md. Fetched once,
	// lazily, only when there's actually something to insert.
	var dedupKeys map[string]bool

	for _, a := range actions {
		if hasKeyIdentity(a) {
			if dedupKeys == nil {
				dedupKeys, err = pendingKeysFromOtherSources(ctx, tx, a.UserID, sourceType, sourceID)
				if err != nil {
					return fmt.Errorf("storage: replace planned actions: %w", err)
				}
			}
			if dedupKeys[planActionKey(a)] {
				// A different source already has a pending row for this
				// exact recommendation — don't mint a second,
				// textually-indistinguishable one (see ADR 005).
				continue
			}
		}
		a.SourceType, a.SourceID = sourceType, sourceID
		// Always mint a fresh id, ignoring whatever id normalization
		// assigned (its deterministic "plan_<documentID>_<i>" scheme,
		// shared with every other document-scoped entity) — every row this
		// loop inserts is a brand-new pending row, so there's no existing
		// row's id to preserve.
		a.ID = "plan_" + uuid.New().String()
		a.Status = normalization.PlannedActionStatusPending
		_, err := tx.ExecContext(ctx, `
			INSERT INTO planned_actions (id, user_id, source_type, source_id, type, description, reference_text,
				match_indicator_name, match_procedure_name, due_date_from, due_date_to, status,
				matched_document_id, matched_entity_id, matched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.UserID, a.SourceType, a.SourceID, a.Type, a.Description, a.ReferenceText,
			a.MatchIndicatorName, a.MatchProcedureName, nullUnix(a.DueDateFrom), nullUnix(a.DueDateTo), a.Status,
			a.MatchedDocumentID, a.MatchedEntityID, nullUnix(a.MatchedAt),
		)
		if err != nil {
			return fmt.Errorf("storage: replace planned actions: insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace planned actions: commit: %w", err)
	}
	return nil
}

func (r *sqlitePlannedActionRepository) RemoveBySource(ctx context.Context, sourceType, sourceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM planned_actions WHERE source_type = ? AND source_id = ?`, sourceType, sourceID)
	if err != nil {
		return fmt.Errorf("storage: remove planned actions by source: %w", err)
	}
	return nil
}

func (r *sqlitePlannedActionRepository) ListByUser(ctx context.Context, userID string) ([]normalization.PlannedAction, error) {
	rows, err := r.db.QueryContext(ctx, plannedActionSelectColumns+` FROM planned_actions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list planned actions by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanPlannedActions(rows)
}

func (r *sqlitePlannedActionRepository) ListPending(ctx context.Context, userID string) ([]normalization.PlannedAction, error) {
	rows, err := r.db.QueryContext(ctx, plannedActionSelectColumns+` FROM planned_actions WHERE user_id = ? AND status = ?`,
		userID, normalization.PlannedActionStatusPending)
	if err != nil {
		return nil, fmt.Errorf("storage: list pending planned actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanPlannedActions(rows)
}

func (r *sqlitePlannedActionRepository) ClearMatchesFromDocument(ctx context.Context, documentID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE planned_actions
		SET status = ?, matched_document_id = '', matched_entity_id = '', matched_at = NULL
		WHERE matched_document_id = ? AND status = ?`,
		normalization.PlannedActionStatusPending, documentID, normalization.PlannedActionStatusCompleted,
	)
	if err != nil {
		return fmt.Errorf("storage: clear planned action matches: %w", err)
	}
	return nil
}

func (r *sqlitePlannedActionRepository) MarkCompleted(ctx context.Context, id, matchedDocumentID, matchedEntityID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE planned_actions
		SET status = ?, matched_document_id = ?, matched_entity_id = ?, matched_at = ?
		WHERE id = ?`,
		normalization.PlannedActionStatusCompleted, matchedDocumentID, matchedEntityID, at.Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("storage: mark planned action completed: %w", err)
	}
	return nil
}

func (r *sqlitePlannedActionRepository) MarkDeclined(ctx context.Context, id, userID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE planned_actions SET status = ? WHERE id = ? AND user_id = ?`,
		normalization.PlannedActionStatusDeclined, id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark planned action declined: %w", err)
	}
	return nil
}

func (r *sqlitePlannedActionRepository) MarkCompletedManually(ctx context.Context, id, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE planned_actions SET status = ?, matched_at = ? WHERE id = ? AND user_id = ?`,
		normalization.PlannedActionStatusCompleted, at.Unix(), id, userID,
	)
	if err != nil {
		return fmt.Errorf("storage: mark planned action completed manually: %w", err)
	}
	return nil
}

func scanPlannedActions(rows *sql.Rows) ([]normalization.PlannedAction, error) {
	var result []normalization.PlannedAction
	for rows.Next() {
		var a normalization.PlannedAction
		var dueFrom, dueTo, matchedAt sql.NullInt64
		if err := rows.Scan(&a.ID, &a.UserID, &a.SourceType, &a.SourceID, &a.Type, &a.Description, &a.ReferenceText,
			&a.MatchIndicatorName, &a.MatchProcedureName, &dueFrom, &dueTo, &a.Status,
			&a.MatchedDocumentID, &a.MatchedEntityID, &matchedAt); err != nil {
			return nil, fmt.Errorf("storage: scan planned action: %w", err)
		}
		a.DueDateFrom = timePtr(dueFrom)
		a.DueDateTo = timePtr(dueTo)
		a.MatchedAt = timePtr(matchedAt)
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate planned actions: %w", err)
	}
	return result, nil
}
