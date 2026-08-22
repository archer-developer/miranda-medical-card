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
	// with actions — NOT a blind delete+reinsert like every other entity's
	// ReplaceForDocument, because PlannedAction.Status/MatchedDocumentID
	// carries state that must survive a reprocess of the document that
	// created it (see docs/adr/004-planned-actions.md §4). Matched by
	// (Type, MatchIndicatorName|MatchProcedureName): a matched existing row
	// keeps its ID/Status/MatchedDocumentID/MatchedEntityID/MatchedAt and
	// only has Description/ReferenceText/DueDateFrom/DueDateTo updated; an
	// existing row whose key no longer appears is deleted only if still
	// pending — a completed row is left alone rather than erased by a
	// merely-reworded re-extraction; a new key is inserted fresh as
	// pending. Two actions in the same call sharing a key degrade to
	// positional matching (documented limitation, see ADR).
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

	existingRows, err := tx.QueryContext(ctx, plannedActionSelectColumns+` FROM planned_actions WHERE source_type = ? AND source_id = ?`, sourceType, sourceID)
	if err != nil {
		return fmt.Errorf("storage: replace planned actions: list existing: %w", err)
	}
	existing, err := scanPlannedActions(existingRows)
	_ = existingRows.Close()
	if err != nil {
		return fmt.Errorf("storage: replace planned actions: list existing: %w", err)
	}

	// byKey queues existing rows per key (front-consumed on match) so two
	// actions sharing a key in the same document degrade to positional
	// matching rather than both colliding on the same row (see
	// ReplaceForSource's doc comment).
	byKey := make(map[string][]normalization.PlannedAction, len(existing))
	for _, e := range existing {
		key := planActionKey(e)
		byKey[key] = append(byKey[key], e)
	}

	consumed := make(map[string]bool, len(existing)) // by ID

	// dedupKeys is userID's other-source pending keys — see
	// docs/adr/005-planned-action-cross-source-dedup.md. Fetched once,
	// lazily, only when there's actually something to insert.
	var dedupKeys map[string]bool

	for _, a := range actions {
		key := planActionKey(a)
		queue := byKey[key]
		if len(queue) > 0 {
			match := queue[0]
			byKey[key] = queue[1:]
			consumed[match.ID] = true
			_, err := tx.ExecContext(ctx, `
				UPDATE planned_actions
				SET description = ?, reference_text = ?, due_date_from = ?, due_date_to = ?
				WHERE id = ?`,
				a.Description, a.ReferenceText, nullUnix(a.DueDateFrom), nullUnix(a.DueDateTo), match.ID,
			)
			if err != nil {
				return fmt.Errorf("storage: replace planned actions: update: %w", err)
			}
			continue
		}
		if hasKeyIdentity(a) {
			if dedupKeys == nil {
				dedupKeys, err = pendingKeysFromOtherSources(ctx, tx, a.UserID, sourceType, sourceID)
				if err != nil {
					return fmt.Errorf("storage: replace planned actions: %w", err)
				}
			}
			if dedupKeys[key] {
				// A different source already has a pending row for this
				// exact recommendation — don't mint a second,
				// textually-indistinguishable one (see ADR 005).
				continue
			}
		}
		a.SourceType, a.SourceID = sourceType, sourceID
		// Always mint a fresh id here, ignoring whatever id normalization
		// assigned (its deterministic "plan_<documentID>_<i>" scheme,
		// shared with every other document-scoped entity) — a stale row
		// occupying that same id may still exist in this table and not be
		// deleted until the cleanup loop below, since reconciliation here
		// is keyed by (Type, MatchIndicatorName|MatchProcedureName), not by
		// id. Reusing the caller's id would then collide with that
		// not-yet-deleted row's primary key. Found via /code-review after
		// reproducing it directly: reprocessing a document whose
		// plannedActions extraction changes key at the same array index
		// (plausible non-determinism, see
		// docs/architecture/02-processing-pipeline.md §11) failed the
		// INSERT with "UNIQUE constraint failed: planned_actions.id" and
		// took down the whole pipeline.run(), not just this step.
		a.ID = "plan_" + uuid.New().String()
		if a.Status == "" {
			a.Status = normalization.PlannedActionStatusPending
		}
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

	// Any existing row never consumed by a match above is gone from the
	// fresh extraction — delete it only if it's still pending; a completed
	// row is left as historical fact (see ReplaceForSource's doc comment).
	for _, e := range existing {
		if consumed[e.ID] {
			continue
		}
		if e.Status != normalization.PlannedActionStatusPending {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM planned_actions WHERE id = ?`, e.ID); err != nil {
			return fmt.Errorf("storage: replace planned actions: delete stale: %w", err)
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
