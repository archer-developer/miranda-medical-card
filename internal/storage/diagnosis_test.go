package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestDiagnosisRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDiagnosisRepository(newTestStore(t))

	diagnosedAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	d := normalization.Diagnosis{
		ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1",
		Name: "Артериальная гипертензия", Code: "I10", CodeSystem: "icd10",
		DiagnosedAt: &diagnosedAt, Status: "active",
	}
	require.NoError(t, repo.Add(ctx, d))

	byUser, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, byUser, 1)
	require.Equal(t, d.Name, byUser[0].Name)
	require.Equal(t, d.Code, byUser[0].Code)
	require.True(t, diagnosedAt.Equal(*byUser[0].DiagnosedAt))

	byDoc, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, byDoc, 1)

	byOtherUser, err := repo.ListByUser(ctx, "user2")
	require.NoError(t, err)
	require.Empty(t, byOtherUser)
}

func TestDiagnosisRepository_AddWithNilDiagnosedAtRoundTripsAsNil(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDiagnosisRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Diagnosis{ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Undated"}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Nil(t, got[0].DiagnosedAt)
}

func TestDiagnosisRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDiagnosisRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Diagnosis{ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Old diagnosis"}))

	replacement := []normalization.Diagnosis{
		{ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "New diagnosis A"},
		{ID: "dx_doc1_1", UserID: "user1", DocumentID: "doc1", Name: "New diagnosis B"},
	}
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", replacement))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 2, "old set must be fully replaced, not appended to")
	names := []string{got[0].Name, got[1].Name}
	require.ElementsMatch(t, []string{"New diagnosis A", "New diagnosis B"}, names)
}

func TestDiagnosisRepository_ReplaceForDocument_EmptySliceClearsExisting(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewDiagnosisRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Diagnosis{ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Resolved on reprocess"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", nil))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Empty(t, got)
}
