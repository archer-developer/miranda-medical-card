package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestMedicationRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1",
		DrugName: "периндоприл", DoseAmount: 5, DoseUnit: "mg", Status: "active",
	}))
	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_doc1_1", UserID: "user1", DocumentID: "doc1",
		DrugName: "бисопролол", Status: "discontinued",
	}))

	all, err := repo.ListByUser(ctx, "user1", storage.MedicationFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestMedicationRepository_ListByUser_FiltersByStatus(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "A", Status: "active"}))
	require.NoError(t, repo.Add(ctx, normalization.Medication{ID: "med_2", UserID: "user1", DocumentID: "doc1", DrugName: "B", Status: "discontinued"}))

	active, err := repo.ListByUser(ctx, "user1", storage.MedicationFilter{Status: "active"})
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "A", active[0].DrugName)
}

func TestMedicationRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Old"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "New"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].DrugName)
}
