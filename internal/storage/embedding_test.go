package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestEmbeddingRepository_AddAndListByUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEmbeddingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, storage.Embedding{
		UserID: "user1", SourceType: "summary", SourceID: "doc1",
		Provider: "gemini", ModelVersion: "gemini-embedding-2", Vector: []float32{0.1, 0.2, 0.3},
	}))

	got, err := repo.ListByUser(ctx, "user1", "gemini-embedding-2")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, []float32{0.1, 0.2, 0.3}, got[0].Vector)
	require.NotEmpty(t, got[0].ID)
}

func TestEmbeddingRepository_ListByUser_ScopedByModelVersion(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEmbeddingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, storage.Embedding{UserID: "user1", SourceType: "summary", SourceID: "doc1", ModelVersion: "v1", Vector: []float32{1}}))
	require.NoError(t, repo.Add(ctx, storage.Embedding{UserID: "user1", SourceType: "summary", SourceID: "doc2", ModelVersion: "v2", Vector: []float32{2}}))

	got, err := repo.ListByUser(ctx, "user1", "v1")
	require.NoError(t, err)
	require.Len(t, got, 1, "embeddings from an old model version must not mix into a v2 search")
}

func TestEmbeddingRepository_RemoveByDocument_OnlyMatchesSummaryType(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEmbeddingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, storage.Embedding{UserID: "user1", SourceType: "summary", SourceID: "doc1", ModelVersion: "v1", Vector: []float32{1}}))
	require.NoError(t, repo.Add(ctx, storage.Embedding{UserID: "user1", SourceType: "self_reported_event", SourceID: "doc1", ModelVersion: "v1", Vector: []float32{2}}))

	require.NoError(t, repo.RemoveByDocument(ctx, "doc1"))

	got, err := repo.ListByUser(ctx, "user1", "v1")
	require.NoError(t, err)
	require.Len(t, got, 1, "RemoveByDocument must only remove the summary-type row, not an unrelated self_reported_event row sharing the same source_id")
	require.Equal(t, "self_reported_event", got[0].SourceType)
}

func TestEmbeddingRepository_RemoveBySource(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewEmbeddingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, storage.Embedding{UserID: "user1", SourceType: "self_reported_event", SourceID: "selfevt_1", ModelVersion: "v1", Vector: []float32{1}}))
	require.NoError(t, repo.RemoveBySource(ctx, "self_reported_event", "selfevt_1"))

	got, err := repo.ListByUser(ctx, "user1", "v1")
	require.NoError(t, err)
	require.Empty(t, got)
}
