package storage_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestProfileRepository_GetNotFoundInitially(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProfileRepository(newTestStore(t))

	_, _, found, err := repo.Get(ctx, "user1")
	require.NoError(t, err)
	require.False(t, found)
}

func TestProfileRepository_ReplaceThenGet(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProfileRepository(newTestStore(t))

	data := json.RawMessage(`{"userId":"user1","activeMedications":[]}`)
	rebuiltAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Replace(ctx, "user1", data, rebuiltAt))

	got, gotRebuiltAt, found, err := repo.Get(ctx, "user1")
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, string(data), string(got))
	require.True(t, rebuiltAt.Equal(gotRebuiltAt))
}

func TestProfileRepository_ReplaceOverwritesPreviousData(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProfileRepository(newTestStore(t))

	require.NoError(t, repo.Replace(ctx, "user1", json.RawMessage(`{"v":1}`), time.Now()))
	require.NoError(t, repo.Replace(ctx, "user1", json.RawMessage(`{"v":2}`), time.Now()))

	got, _, found, err := repo.Get(ctx, "user1")
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, `{"v":2}`, string(got))
}
