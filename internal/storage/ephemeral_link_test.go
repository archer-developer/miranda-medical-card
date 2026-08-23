package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestEphemeralLinkRepository_AddAssignsIDWhenEmpty(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEphemeralLinkRepository(newTestStore(t))

	now := time.Now().UTC().Truncate(time.Second)
	added, err := repo.Add(ctx, storage.EphemeralLink{
		Content: []byte(`{"foo":"bar"}`), ContentType: "application/json",
		Filename: "profile.json", CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	require.NotEmpty(t, added.ID)

	got, err := repo.Get(ctx, added.ID)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"foo":"bar"}`), got.Content)
	require.Equal(t, "application/json", got.ContentType)
	require.Equal(t, "profile.json", got.Filename)
	require.Empty(t, got.FileID)
	require.Equal(t, now, got.CreatedAt)
	require.Equal(t, now.Add(5*time.Minute), got.ExpiresAt)
}

func TestEphemeralLinkRepository_Get_UnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEphemeralLinkRepository(newTestStore(t))

	_, err := repo.Get(ctx, "does-not-exist")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

// TestEphemeralLinkRepository_Get_DoesNotFilterByExpiry guards the split
// documented on EphemeralLinkRepository.Get: this repository returns
// whatever row exists regardless of expires_at — internal/linkstore.Store
// (the TTL-aware clock seam, not this repository) is what turns an expired
// row into a not-found result.
func TestEphemeralLinkRepository_Get_DoesNotFilterByExpiry(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEphemeralLinkRepository(newTestStore(t))

	past := time.Now().Add(-time.Hour).UTC()
	added, err := repo.Add(ctx, storage.EphemeralLink{
		Content: []byte("stale"), ContentType: "text/plain", Filename: "f.txt",
		CreatedAt: past, ExpiresAt: past.Add(time.Minute),
	})
	require.NoError(t, err)

	got, err := repo.Get(ctx, added.ID)
	require.NoError(t, err)
	require.Equal(t, []byte("stale"), got.Content)
}

func TestEphemeralLinkRepository_DeleteExpired(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEphemeralLinkRepository(newTestStore(t))

	now := time.Now().UTC()
	expired, err := repo.Add(ctx, storage.EphemeralLink{
		Content: []byte("old"), ContentType: "text/plain", Filename: "old.txt",
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	})
	require.NoError(t, err)
	fresh, err := repo.Add(ctx, storage.EphemeralLink{
		Content: []byte("new"), ContentType: "text/plain", Filename: "new.txt",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)

	n, err := repo.DeleteExpired(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	_, err = repo.Get(ctx, expired.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
	_, err = repo.Get(ctx, fresh.ID)
	require.NoError(t, err)
}
