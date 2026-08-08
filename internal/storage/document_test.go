package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestDocumentRepository_AddDefaultsStatusToPending(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)
	require.NotEmpty(t, added.ID)
	require.Equal(t, storage.DocumentStatusPending, added.Status)

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, "file_1", got.FileID)
}

func TestDocumentRepository_Get_WrongUserReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)

	_, err = repo.Get(ctx, added.ID, "user2")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestDocumentRepository_UpdateStatus(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)

	require.NoError(t, repo.UpdateStatus(ctx, added.ID, "user1", storage.DocumentStatusRunning))

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusRunning, got.Status)
}

func TestDocumentRepository_UpdateStatus_WrongUserReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)

	err = repo.UpdateStatus(ctx, added.ID, "user2", storage.DocumentStatusReady)
	require.ErrorIs(t, err, storage.ErrNotFound)

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusPending, got.Status, "wrong-user update must not have taken effect")
}

func TestDocumentRepository_List_FiltersByStatus(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	ready, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1", Status: storage.DocumentStatusReady})
	require.NoError(t, err)
	_, err = repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_2", Status: storage.DocumentStatusFailed})
	require.NoError(t, err)

	readyOnly, err := repo.List(ctx, "user1", storage.DocumentFilter{Status: storage.DocumentStatusReady})
	require.NoError(t, err)
	require.Len(t, readyOnly, 1)
	require.Equal(t, ready.ID, readyOnly[0].ID)

	all, err := repo.List(ctx, "user1", storage.DocumentFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestDocumentRepository_List_FiltersByFileID(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	imported, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)
	_, err = repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_2"})
	require.NoError(t, err)

	got, err := repo.List(ctx, "user1", storage.DocumentFilter{FileID: "file_1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, imported.ID, got[0].ID)

	none, err := repo.List(ctx, "user1", storage.DocumentFilter{FileID: "file_never_imported"})
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestDocumentRepository_UpdateExtracted(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)

	documentDate := mustDate("2026-03-12")
	require.NoError(t, repo.UpdateExtracted(ctx, added.ID, "user1", storage.DocumentExtractedUpdate{
		DocumentType: "lab_report", DocumentDate: documentDate, Title: "Общий анализ крови",
		Organization: "Инвитро", Doctor: "", RecognizedText: "full text here", Summary: "short summary",
	}))

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, "lab_report", got.DocumentType)
	require.Equal(t, "2026-03-12", got.DocumentDate.Format("2006-01-02"))
	require.Equal(t, "Общий анализ крови", got.Title)
	require.Equal(t, "Инвитро", got.Organization)
	require.Equal(t, "full text here", got.RecognizedText)
	require.Equal(t, "short summary", got.Summary)
	require.Equal(t, storage.DocumentStatusPending, got.Status, "UpdateExtracted must not touch status")
}

func TestDocumentRepository_UpdateExtracted_WrongUserReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)

	err = repo.UpdateExtracted(ctx, added.ID, "user2", storage.DocumentExtractedUpdate{Title: "hijacked"})
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestDocumentRepository_Remove(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDocumentRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file_1"})
	require.NoError(t, err)
	require.NoError(t, repo.Remove(ctx, added.ID, "user1"))

	_, err = repo.Get(ctx, added.ID, "user1")
	require.ErrorIs(t, err, storage.ErrNotFound)
}
