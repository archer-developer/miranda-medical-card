package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestCanonicalUnitRepository_NotFoundInitially(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewCanonicalUnitRepository(newTestStore(t))

	_, found, err := repo.CanonicalUnit(ctx, "user1", "Гемоглобин")
	require.NoError(t, err)
	require.False(t, found)
}

func TestCanonicalUnitRepository_SetIfAbsentThenFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewCanonicalUnitRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "user1", "Гемоглобин", "г/дл"))

	unit, found, err := repo.CanonicalUnit(ctx, "user1", "Гемоглобин")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "г/дл", unit)
}

func TestCanonicalUnitRepository_SetIfAbsent_SecondCallDoesNotOverride(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewCanonicalUnitRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "user1", "Гемоглобин", "г/дл"))
	require.NoError(t, repo.SetIfAbsent(ctx, "user1", "Гемоглобин", "г/л"), "must not error even though it's a no-op")

	unit, found, err := repo.CanonicalUnit(ctx, "user1", "Гемоглобин")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "г/дл", unit, "first write wins — second call must not override")
}

func TestCanonicalUnitRepository_ScopedPerUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewCanonicalUnitRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "user1", "Гемоглобин", "г/дл"))

	_, found, err := repo.CanonicalUnit(ctx, "user2", "Гемоглобин")
	require.NoError(t, err)
	require.False(t, found, "canonical unit must not leak across users")
}
