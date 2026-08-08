package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProfileRepository mirrors docs/domain/05-medical-profile.md §5 — a single
// row per user, always fully replaced, never partially updated (see that
// doc's "Инварианты": "пересобирается полностью (не инкрементально)").
//
// Deliberately typed around json.RawMessage rather than a concrete Profile
// struct: the profile package (internal/profile) already depends on this
// package (for storage.MedicationFilter, see profile.MedicationRepository),
// so this package importing profile's Profile type back would be an import
// cycle. Storing/loading opaque JSON keeps the dependency one-directional —
// internal/profile.Store wraps this repository and does the marshal/
// unmarshal, so every other caller still works with a typed profile.Profile
// and never sees json.RawMessage directly.
type ProfileRepository interface {
	// Get returns the stored profile JSON and when it was last rebuilt, or
	// found=false if no profile has been built yet for userID (see
	// docs/domain/05-medical-profile.md §4 — this is a normal state, not an
	// error, for a user with no READY documents yet).
	Get(ctx context.Context, userID string) (data json.RawMessage, rebuiltAt time.Time, found bool, err error)
	Replace(ctx context.Context, userID string, data json.RawMessage, rebuiltAt time.Time) error
}

type sqliteProfileRepository struct {
	db *sql.DB
}

// NewProfileRepository builds a ProfileRepository backed by s.
func NewProfileRepository(s *Store) ProfileRepository {
	return &sqliteProfileRepository{db: s.db}
}

func (r *sqliteProfileRepository) Get(ctx context.Context, userID string) (json.RawMessage, time.Time, bool, error) {
	var data string
	var rebuiltAt int64
	err := r.db.QueryRowContext(ctx, `SELECT data, rebuilt_at FROM medical_profiles WHERE user_id = ?`, userID).Scan(&data, &rebuiltAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("storage: get profile: %w", err)
	}
	return json.RawMessage(data), time.Unix(rebuiltAt, 0).UTC(), true, nil
}

func (r *sqliteProfileRepository) Replace(ctx context.Context, userID string, data json.RawMessage, rebuiltAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO medical_profiles (user_id, data, rebuilt_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET data = excluded.data, rebuilt_at = excluded.rebuilt_at`,
		userID, string(data), rebuiltAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("storage: replace profile: %w", err)
	}
	return nil
}
