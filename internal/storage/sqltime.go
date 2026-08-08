package storage

import (
	"database/sql"
	"time"
)

// nullUnix converts a possibly-nil *time.Time (as used throughout
// normalization.Result, e.g. Diagnosis.DiagnosedAt) into the sql.NullInt64
// database/sql expects for a nullable INTEGER column.
func nullUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// timePtr is nullUnix's inverse, used when scanning a row back out.
func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}
