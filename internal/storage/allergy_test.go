package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestAllergyRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAllergyRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Allergy{
		ID: "allergy_1", UserID: "user1", DocumentID: "doc1",
		Substance: "Пенициллин", Reaction: "Rash", Severity: "moderate",
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Пенициллин", got[0].Substance)
}

func TestAllergyRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAllergyRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Allergy{ID: "allergy_1", UserID: "user1", DocumentID: "doc1", Substance: "Old"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Allergy{
		{ID: "allergy_1", UserID: "user1", DocumentID: "doc1", Substance: "New"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].Substance)
}
