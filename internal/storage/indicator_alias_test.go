package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestIndicatorAliasRepository_NotFoundInitially(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	_, found, err := repo.CanonicalIndicatorName(ctx, "Лейкоциты (WBC)")
	require.NoError(t, err)
	require.False(t, found)
}

func TestIndicatorAliasRepository_SetIfAbsentThenFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Лейкоциты"))

	canonical, found, err := repo.CanonicalIndicatorName(ctx, "Лейкоциты (WBC)")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Лейкоциты", canonical)
}

func TestIndicatorAliasRepository_LookupIsCaseInsensitiveAndTrimmed(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Лейкоциты"))

	canonical, found, err := repo.CanonicalIndicatorName(ctx, "  лейкоциты (wbc)  ")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Лейкоциты", canonical)
}

func TestIndicatorAliasRepository_SetIfAbsent_SecondCallDoesNotOverride(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Лейкоциты"))
	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Something Else"), "must not error even though it's a no-op")

	canonical, found, err := repo.CanonicalIndicatorName(ctx, "Лейкоциты (WBC)")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Лейкоциты", canonical, "seeding must never clobber an existing entry")
}

func TestIndicatorAliasRepository_SetOverwritesExistingValue(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Лейкоциты"))
	require.NoError(t, repo.Set(ctx, "Лейкоциты (WBC)", "Corrected Name"))

	canonical, found, err := repo.CanonicalIndicatorName(ctx, "Лейкоциты (WBC)")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Corrected Name", canonical, "Set must overwrite, unlike SetIfAbsent")
}

func TestIndicatorAliasRepository_ListAll(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewIndicatorAliasRepository(newTestStore(t))

	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты (WBC)", "Лейкоциты"))
	require.NoError(t, repo.SetIfAbsent(ctx, "Лейкоциты", "Лейкоциты"))
	require.NoError(t, repo.SetIfAbsent(ctx, "АлАТ", "АЛТ"))

	all, err := repo.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
}
