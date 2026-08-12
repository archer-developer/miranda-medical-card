package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// LabResultRepository mirrors docs/domain/09-lab-result-and-vital-sign.md §6.
type LabResultRepository interface {
	Add(ctx context.Context, l normalization.LabResult) error
	ListByUser(ctx context.Context, userID string) ([]normalization.LabResult, error)
	ListByDocument(ctx context.Context, documentID string) ([]normalization.LabResult, error)
	ReplaceForDocument(ctx context.Context, documentID string, results []normalization.LabResult) error
	// LatestByIndicator returns, for each indicatorName userID has at
	// least one LabResult for, the single most recent one (by TakenAt) —
	// used by ProfileBuilder (see docs/domain/05-medical-profile.md §3).
	LatestByIndicator(ctx context.Context, userID string) (map[string]normalization.LabResult, error)
	// HistoryByIndicator returns every LabResult userID has whose indicator
	// name matches query, oldest first — used by Lab Provider (see
	// docs/architecture/04-search.md §7) to answer trend questions. A row
	// matches if indicator_name contains query (case-insensitive substring,
	// not exact-match — LabResult.IndicatorName is already canonicalized at
	// ingest time, but a caller's query, e.g. a Planner-supplied
	// indicatorName or searchQuery, rarely spells the full canonical name),
	// or if query matches a registered indicator_aliases row (by alias or
	// canonical_name substring) whose canonical_name equals indicator_name
	// — so a query like "холестерин" finds every "Холестерин-ЛПНП"/
	// "Холестерин-ЛПВП" variant, and a query naming only a registered alias
	// (e.g. "HDL", registered only inside the longer alias string
	// "ЛПВП-холестерин (HDL)", not as a substring of the canonical name
	// itself) still resolves to its canonical indicator.
	HistoryByIndicator(ctx context.Context, userID, query string) ([]normalization.LabResult, error)
}

type sqliteLabResultRepository struct {
	db *sql.DB
}

// NewLabResultRepository builds a LabResultRepository backed by s.
func NewLabResultRepository(s *Store) LabResultRepository {
	return &sqliteLabResultRepository{db: s.db}
}

const labResultSelectColumns = `SELECT id, user_id, document_id, indicator_name, code, code_system, value, qualitative_value, unit, normalized_value, normalized_unit, reference_low, reference_high, taken_at`

func (r *sqliteLabResultRepository) Add(ctx context.Context, l normalization.LabResult) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO lab_results (id, user_id, document_id, indicator_name, code, code_system, value, qualitative_value, unit, normalized_value, normalized_unit, reference_low, reference_high, taken_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.UserID, l.DocumentID, l.IndicatorName, l.Code, l.CodeSystem, l.Value, l.QualitativeValue, l.Unit,
		l.NormalizedValue, l.NormalizedUnit, l.ReferenceLow, l.ReferenceHigh, nullUnix(l.TakenAt),
	)
	if err != nil {
		return fmt.Errorf("storage: add lab result: %w", err)
	}
	return nil
}

// labResultOrderBy sorts most-recent-first (taken_at DESC), with
// undated rows (taken_at IS NULL) pushed last rather than sorting
// unpredictably among the dated ones — used by both listing methods below so
// LabProvider.Collect's Limit truncation (see internal/ask/providers.go)
// actually keeps the most recent results, matching what the lab_results
// tool's own schema promises the model ("most recent first").
const labResultOrderBy = ` ORDER BY taken_at IS NULL, taken_at DESC`

func (r *sqliteLabResultRepository) ListByUser(ctx context.Context, userID string) ([]normalization.LabResult, error) {
	rows, err := r.db.QueryContext(ctx, labResultSelectColumns+` FROM lab_results WHERE user_id = ?`+labResultOrderBy, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list lab results by user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLabResults(rows)
}

func (r *sqliteLabResultRepository) ListByDocument(ctx context.Context, documentID string) ([]normalization.LabResult, error) {
	rows, err := r.db.QueryContext(ctx, labResultSelectColumns+` FROM lab_results WHERE document_id = ?`+labResultOrderBy, documentID)
	if err != nil {
		return nil, fmt.Errorf("storage: list lab results by document: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLabResults(rows)
}

func (r *sqliteLabResultRepository) ReplaceForDocument(ctx context.Context, documentID string, results []normalization.LabResult) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: replace lab results: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM lab_results WHERE document_id = ?`, documentID); err != nil {
		return fmt.Errorf("storage: replace lab results: delete: %w", err)
	}
	for _, l := range results {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO lab_results (id, user_id, document_id, indicator_name, code, code_system, value, qualitative_value, unit, normalized_value, normalized_unit, reference_low, reference_high, taken_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.ID, l.UserID, documentID, l.IndicatorName, l.Code, l.CodeSystem, l.Value, l.QualitativeValue, l.Unit,
			l.NormalizedValue, l.NormalizedUnit, l.ReferenceLow, l.ReferenceHigh, nullUnix(l.TakenAt),
		)
		if err != nil {
			return fmt.Errorf("storage: replace lab results: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: replace lab results: commit: %w", err)
	}
	return nil
}

// LatestByIndicator loads every LabResult for userID and keeps the one with
// the greatest TakenAt per indicatorName in application code, rather than a
// SQL window function — the per-user row count this operates over (a
// personal medical history, not a multi-tenant table) makes that simplicity
// worth more than the marginal query-time cost, and it keeps the "greatest
// TakenAt wins, nil TakenAt never wins over a dated one" tie-break easy to
// read and test.
func (r *sqliteLabResultRepository) LatestByIndicator(ctx context.Context, userID string) (map[string]normalization.LabResult, error) {
	all, err := r.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]normalization.LabResult)
	for _, l := range all {
		current, ok := latest[l.IndicatorName]
		if !ok || isAfter(l.TakenAt, current.TakenAt) {
			latest[l.IndicatorName] = l
		}
	}
	return latest, nil
}

// HistoryByIndicator matches in application code rather than SQL LIKE/LOWER
// — SQLite's built-in LOWER/LIKE case-fold ASCII only, so a Cyrillic query
// like "холестерин" would never match stored "Холестерин-ЛПНП" through
// SQL-side case-insensitive comparison. strings.ToLower is Unicode-aware,
// and (matching LatestByIndicator's own reasoning just above) this table's
// per-user row count is personal-history scale, not something loading it
// entirely into memory need worry about.
func (r *sqliteLabResultRepository) HistoryByIndicator(ctx context.Context, userID, query string) ([]normalization.LabResult, error) {
	all, err := r.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	canonicalMatches, err := r.matchingCanonicalNames(ctx, query)
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(strings.TrimSpace(query))
	var result []normalization.LabResult
	for _, l := range all {
		if strings.Contains(strings.ToLower(l.IndicatorName), needle) || canonicalMatches[l.IndicatorName] {
			result = append(result, l)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		ti, tj := result[i].TakenAt, result[j].TakenAt
		if ti == nil {
			return tj != nil
		}
		if tj == nil {
			return false
		}
		return ti.Before(*tj)
	})
	return result, nil
}

// matchingCanonicalNames returns, as a set, every indicator_aliases
// canonical_name whose registered alias or canonical spelling itself
// contains query — e.g. query "HDL" matches the alias "ЛПВП-холестерин
// (HDL)", registered under canonical "Холестерин-ЛПВП", even though "HDL"
// isn't a substring of that canonical name itself. Loads the whole
// (global, not per-user) dictionary — see indicator_aliases.go's doc
// comment: it's meant to stay small and human-curated, not grow to a size
// where this needs an indexed query.
func (r *sqliteLabResultRepository) matchingCanonicalNames(ctx context.Context, query string) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT alias, canonical_name FROM indicator_aliases`)
	if err != nil {
		return nil, fmt.Errorf("storage: history lab results by indicator: list aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	needle := strings.ToLower(strings.TrimSpace(query))
	matches := make(map[string]bool)
	for rows.Next() {
		var alias, canonical string
		if err := rows.Scan(&alias, &canonical); err != nil {
			return nil, fmt.Errorf("storage: history lab results by indicator: scan alias: %w", err)
		}
		// alias is already stored lower+trim (see normalizeAliasKey).
		if strings.Contains(alias, needle) || strings.Contains(strings.ToLower(canonical), needle) {
			matches[canonical] = true
		}
	}
	return matches, rows.Err()
}

func scanLabResults(rows *sql.Rows) ([]normalization.LabResult, error) {
	var result []normalization.LabResult
	for rows.Next() {
		var l normalization.LabResult
		var takenAt sql.NullInt64
		if err := rows.Scan(&l.ID, &l.UserID, &l.DocumentID, &l.IndicatorName, &l.Code, &l.CodeSystem, &l.Value, &l.QualitativeValue, &l.Unit,
			&l.NormalizedValue, &l.NormalizedUnit, &l.ReferenceLow, &l.ReferenceHigh, &takenAt); err != nil {
			return nil, fmt.Errorf("storage: scan lab result: %w", err)
		}
		l.TakenAt = timePtr(takenAt)
		result = append(result, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate lab results: %w", err)
	}
	return result, nil
}

// isAfter reports whether a is a later point in time than b, treating nil
// as older than any set time (a nil TakenAt should never win a "latest"
// comparison against a dated one) and as not-after another nil.
func isAfter(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}
