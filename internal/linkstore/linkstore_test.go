package linkstore

// This file is package linkstore (white-box), unlike a typical
// internal/*_test.go package under this repo's own conventions —
// Store's expiry test needs direct access to its unexported now clock
// seam, which an external test package can't reach (mirrors
// internal/ask/session_test.go's own reason for the same choice).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMintAndResolve(t *testing.T) {
	ctx := context.Background()
	s := New(storage.NewEphemeralLinkRepository(newTestStore(t)))

	id, expiresAt, err := s.Mint(ctx, []byte(`{"foo":"bar"}`), "application/json", "profile.json", 5*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), expiresAt, 5*time.Second)

	content, contentType, filename, err := s.Resolve(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"foo":"bar"}`), content)
	require.Equal(t, "application/json", contentType)
	require.Equal(t, "profile.json", filename)
}

func TestResolve_UnknownIDReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s := New(storage.NewEphemeralLinkRepository(newTestStore(t)))

	_, _, _, err := s.Resolve(ctx, "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestResolve_ExpiredReturnsErrNotFound advances a fake clock past the
// link's TTL rather than sleeping or hand-writing a past timestamp via SQL
// — see internal/ask/session_test.go's TestSessionStore_IdleExpiry for the
// same pattern applied to session idle expiry.
func TestResolve_ExpiredReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := &Store{repo: storage.NewEphemeralLinkRepository(newTestStore(t)), now: func() time.Time { return now }}

	id, expiresAt, err := s.Mint(ctx, []byte("data"), "text/plain", "f.txt", 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, now.Add(5*time.Minute), expiresAt)

	_, _, _, err = s.Resolve(ctx, id)
	require.NoError(t, err, "well within TTL, Resolve must succeed")

	now = now.Add(5*time.Minute + time.Second)
	_, _, _, err = s.Resolve(ctx, id)
	require.ErrorIs(t, err, ErrNotFound, "past TTL, Resolve must behave exactly like an unknown id")
}

func TestDeleteExpired(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := &Store{repo: storage.NewEphemeralLinkRepository(newTestStore(t)), now: func() time.Time { return now }}

	expiredID, _, err := s.Mint(ctx, []byte("old"), "text/plain", "old.txt", time.Minute)
	require.NoError(t, err)
	freshID, _, err := s.Mint(ctx, []byte("new"), "text/plain", "new.txt", time.Hour)
	require.NoError(t, err)

	now = now.Add(5 * time.Minute)
	n, err := s.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	_, _, _, err = s.Resolve(ctx, expiredID)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, _, err = s.Resolve(ctx, freshID)
	require.NoError(t, err)
}
