package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// Compile-time check that sqliteCanonicalUnitRepository's CanonicalUnit
// method actually satisfies normalization.CanonicalUnitResolver — Go's
// structural typing means no explicit adapter is otherwise required, but
// nothing else would catch a signature drift between the two packages.
var _ normalization.CanonicalUnitResolver = (*sqliteCanonicalUnitRepository)(nil)

// CanonicalUnitRepository persists the "first unit ever seen wins" cache
// described in docs/domain/09-lab-result-and-vital-sign.md §7 — the
// per-(user, indicator) canonical unit that LabResult/InstrumentalFinding
// normalization converts everything else to (see
// internal/normalization/units.go's CanonicalUnitResolver).
//
// Read and write are deliberately two separate methods rather than one
// "get or set" call: CanonicalUnit satisfies
// normalization.CanonicalUnitResolver directly (Go's structural typing
// means this type needs no adapter), and that interface is read-only by
// design — Normalize only ever looks up an existing canonical unit, never
// decides on its own whether a new one should be recorded. Persisting a
// newly-established canonical unit is the caller's job (the Pipeline, after
// Normalize returns), exactly mirroring the pattern already used and
// documented in normalization's own tests (see
// internal/normalization/normalization_test.go's fakeResolver.recordUnits).
type CanonicalUnitRepository interface {
	// CanonicalUnit implements normalization.CanonicalUnitResolver.
	CanonicalUnit(ctx context.Context, userID, indicatorName string) (unit string, found bool, err error)
	// SetIfAbsent records unit as canonical for (userID, indicatorName) if
	// and only if nothing is recorded yet — an atomic "first write wins"
	// upsert (INSERT OR IGNORE), not a read-then-write, so two concurrent
	// callers racing to establish the canonical unit for the same
	// indicator can't both "win" with different units.
	SetIfAbsent(ctx context.Context, userID, indicatorName, unit string) error
	// Set unconditionally overwrites the canonical unit for
	// (userID, indicatorName). Unlike SetIfAbsent, this is not part of the
	// live Pipeline's "first write wins" contract — its only caller is the
	// one-off cmd/renormalize-labs migration tool, which needs to
	// re-establish a canonical unit after LabResults were regrouped under a
	// new indicator_name by the alias dictionary (indicator_aliases.go),
	// where the old per-old-name canonical_units row is no longer correct.
	Set(ctx context.Context, userID, indicatorName, unit string) error
}

type sqliteCanonicalUnitRepository struct {
	db *sql.DB
}

// NewCanonicalUnitRepository builds a CanonicalUnitRepository backed by s.
func NewCanonicalUnitRepository(s *Store) CanonicalUnitRepository {
	return &sqliteCanonicalUnitRepository{db: s.db}
}

func (r *sqliteCanonicalUnitRepository) CanonicalUnit(ctx context.Context, userID, indicatorName string) (string, bool, error) {
	var unit string
	err := r.db.QueryRowContext(ctx,
		`SELECT unit FROM canonical_units WHERE user_id = ? AND indicator_name = ?`,
		userID, indicatorName,
	).Scan(&unit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: canonical unit: %w", err)
	}
	return unit, true, nil
}

func (r *sqliteCanonicalUnitRepository) SetIfAbsent(ctx context.Context, userID, indicatorName, unit string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO canonical_units (user_id, indicator_name, unit) VALUES (?, ?, ?)`,
		userID, indicatorName, unit,
	)
	if err != nil {
		return fmt.Errorf("storage: set canonical unit: %w", err)
	}
	return nil
}

func (r *sqliteCanonicalUnitRepository) Set(ctx context.Context, userID, indicatorName, unit string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO canonical_units (user_id, indicator_name, unit) VALUES (?, ?, ?)
		 ON CONFLICT(user_id, indicator_name) DO UPDATE SET unit = excluded.unit`,
		userID, indicatorName, unit,
	)
	if err != nil {
		return fmt.Errorf("storage: overwrite canonical unit: %w", err)
	}
	return nil
}
