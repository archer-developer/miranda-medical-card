package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestFTSRepository_IndexAndSearchDocuments(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "Общий анализ крови", "Пациент жалуется на бессонницу и усталость."))
	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc2", "Выписка", "Плановая госпитализация, без особенностей."))

	// Trigram matching is substring search, not stemming — see storage.go's
	// schema comment. Querying the shared stem "бессонниц" (not the
	// nominative-case "бессонница", which isn't literally a substring of
	// the stored accusative "бессонницу") is what actually demonstrates its
	// value for Russian: it still finds an inflected form.
	results, err := repo.SearchDocuments(ctx, "user1", "бессонниц", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "doc1", results[0].DocumentID)
	require.Contains(t, results[0].Snippet, "бессонниц")
}

func TestFTSRepository_SearchDocuments_ScopedPerUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "T", "бессонница"))

	results, err := repo.SearchDocuments(ctx, "user2", "бессонница", 10)
	require.NoError(t, err)
	require.Empty(t, results, "must not leak another user's documents")
}

func TestFTSRepository_ReindexingReplacesNotAppends(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "T", "старый текст про бессонницу"))
	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "T", "новый текст без упоминаний"))

	results, err := repo.SearchDocuments(ctx, "user1", "бессонниц", 10)
	require.NoError(t, err)
	require.Empty(t, results, "reindexing the same document must replace, not add to, the previous entry")
}

func TestFTSRepository_RemoveDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "T", "бессонница"))
	require.NoError(t, repo.RemoveDocument(ctx, "doc1"))

	results, err := repo.SearchDocuments(ctx, "user1", "бессонница", 10)
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestFTSRepository_SearchWithSpecialCharactersDoesNotError(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))
	require.NoError(t, repo.IndexDocument(ctx, "user1", "doc1", "T", "some content"))

	_, err := repo.SearchDocuments(ctx, "user1", `unbalanced " quote - and NOT AND OR syntax*`, 10)
	require.NoError(t, err, "arbitrary user text with FTS5 syntax characters must not error the query")
}

func TestFTSRepository_IndexAndSearchEvents(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexEvent(ctx, "user1", "selfevt_1", "Было очень плохо вечером, кружилась голова"))

	results, err := repo.SearchEvents(ctx, "user1", "кружилась голова", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "selfevt_1", results[0].EventID)
}

func TestFTSRepository_RemoveEvent(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFTSRepository(newTestStore(t))

	require.NoError(t, repo.IndexEvent(ctx, "user1", "selfevt_1", "текст"))
	require.NoError(t, repo.RemoveEvent(ctx, "selfevt_1"))

	results, err := repo.SearchEvents(ctx, "user1", "текст", 10)
	require.NoError(t, err)
	require.Empty(t, results)
}
