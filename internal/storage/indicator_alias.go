package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// Compile-time check that sqliteIndicatorAliasRepository's
// CanonicalIndicatorName method actually satisfies
// normalization.IndicatorAliasResolver — same reasoning as
// canonical_unit.go's equivalent check.
var _ normalization.IndicatorAliasResolver = (*sqliteIndicatorAliasRepository)(nil)

// IndicatorAlias is one row of the indicator_aliases table, returned by
// ListAll for inspection/debugging (e.g. a future admin CLI listing the
// current dictionary).
type IndicatorAlias struct {
	Alias         string
	CanonicalName string
}

// IndicatorAliasRepository persists the global (not per-user) dictionary
// mapping a raw LabResult indicator name to its canonical spelling — see
// internal/normalization/indicator_aliases.go's doc comment for why this
// exists (closing the gap docs/architecture/02-processing-pipeline.md §6
// calls out) and its accuracy posture (exact match only, no fuzzy
// matching, never conflates a %/absolute pair).
//
// Unlike CanonicalUnitRepository (which only the Pipeline ever writes, as
// a side effect of processing documents), this dictionary is meant to be
// edited directly — by an operator via sqlite3, or eventually an LLM
// fallback step deciding a not-yet-seen name is an alias for an existing
// indicator — without requiring a new deploy. The table is seeded once per
// database from normalization.IndicatorAliasSeedGroups (see
// cmd/miranda-medical-card/main.go's startup sequence) via SetIfAbsent, so
// a later edit already in the table is never clobbered by a repeat seed.
type IndicatorAliasRepository interface {
	// CanonicalIndicatorName implements normalization.IndicatorAliasResolver.
	CanonicalIndicatorName(ctx context.Context, alias string) (canonical string, found bool, err error)
	// SetIfAbsent registers alias -> canonicalName only if alias isn't
	// already registered — the seeding operation (see this type's doc
	// comment); never overwrites a later manual or LLM-confirmed edit.
	SetIfAbsent(ctx context.Context, alias, canonicalName string) error
	// Set unconditionally overwrites alias -> canonicalName — for an
	// explicit operator/admin correction or a confirmed LLM decision, as
	// opposed to seeding.
	Set(ctx context.Context, alias, canonicalName string) error
	// ListAll returns every registered alias, ordered by canonical name
	// then alias, for inspection/debugging.
	ListAll(ctx context.Context) ([]IndicatorAlias, error)
}

type sqliteIndicatorAliasRepository struct {
	db *sql.DB
}

// NewIndicatorAliasRepository builds an IndicatorAliasRepository backed by s.
func NewIndicatorAliasRepository(s *Store) IndicatorAliasRepository {
	return &sqliteIndicatorAliasRepository{db: s.db}
}

// normalizeAliasKey is the same case-insensitive, trimmed key every lookup
// and write goes through — matches canonicalizeIndicatorName's own
// strings.ToLower(strings.TrimSpace(...)) so a row seeded here is found by
// exactly the same normalization Normalize applies to a raw extracted name.
func normalizeAliasKey(alias string) string {
	return strings.ToLower(strings.TrimSpace(alias))
}

func (r *sqliteIndicatorAliasRepository) CanonicalIndicatorName(ctx context.Context, alias string) (string, bool, error) {
	var canonical string
	err := r.db.QueryRowContext(ctx,
		`SELECT canonical_name FROM indicator_aliases WHERE alias = ?`,
		normalizeAliasKey(alias),
	).Scan(&canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: canonical indicator name: %w", err)
	}
	return canonical, true, nil
}

func (r *sqliteIndicatorAliasRepository) SetIfAbsent(ctx context.Context, alias, canonicalName string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO indicator_aliases (alias, canonical_name) VALUES (?, ?)`,
		normalizeAliasKey(alias), canonicalName,
	)
	if err != nil {
		return fmt.Errorf("storage: seed indicator alias: %w", err)
	}
	return nil
}

func (r *sqliteIndicatorAliasRepository) Set(ctx context.Context, alias, canonicalName string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO indicator_aliases (alias, canonical_name) VALUES (?, ?)
		 ON CONFLICT(alias) DO UPDATE SET canonical_name = excluded.canonical_name`,
		normalizeAliasKey(alias), canonicalName,
	)
	if err != nil {
		return fmt.Errorf("storage: set indicator alias: %w", err)
	}
	return nil
}

func (r *sqliteIndicatorAliasRepository) ListAll(ctx context.Context) ([]IndicatorAlias, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT alias, canonical_name FROM indicator_aliases ORDER BY canonical_name, alias`)
	if err != nil {
		return nil, fmt.Errorf("storage: list indicator aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []IndicatorAlias
	for rows.Next() {
		var a IndicatorAlias
		if err := rows.Scan(&a.Alias, &a.CanonicalName); err != nil {
			return nil, fmt.Errorf("storage: scan indicator alias: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
