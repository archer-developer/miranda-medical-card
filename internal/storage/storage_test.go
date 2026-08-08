package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// newTestStore opens an in-memory SQLite database — MaxOpenConns(1) means
// exactly one connection ever exists, so ":memory:" behaves as a single
// private database for the lifetime of the Store, no shared-cache DSN
// needed. Closed automatically via t.Cleanup.
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_ReapplyingSchemaAgainstExistingFileIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "medical.db")

	first, err := storage.Open(path)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	// Re-opening the same file re-runs schema (CREATE TABLE/INDEX IF NOT
	// EXISTS throughout) — must not fail with "table already exists".
	second, err := storage.Open(path)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}
