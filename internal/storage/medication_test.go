package storage_test

import (
	"context"
	"testing"
	"time"

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

func TestMedicationRepository_MarkEndedManually(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: "active",
	}))

	endedAt := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.MarkEndedManually(ctx, "med_1", "user1", endedAt))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "completed", got[0].Status)
	require.True(t, endedAt.Equal(*got[0].EndedAt))
	require.True(t, endedAt.Equal(*got[0].ConfirmedEndedAt))
}

func TestMedicationRepository_MarkEndedManually_ScopedToOwningUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: "active",
	}))
	require.NoError(t, repo.MarkEndedManually(ctx, "med_1", "user2", time.Now()))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "active", got[0].Status, "MarkEndedManually for a different user must be a no-op")
}

func TestMedicationRepository_ReplaceForDocument_PreservesConfirmedRow(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: "active",
	}))
	endedAt := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.MarkEndedManually(ctx, "med_1", "user1", endedAt))

	// Reprocess doc1 — a fresh extraction still reports the same medication
	// as active (e.g. the document itself never said it ended), which must
	// not overwrite the user's own confirmation.
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: "active"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 2, "the confirmed row is left alone, and the fresh extraction is inserted as its own row")
	var confirmed, fresh normalization.Medication
	for _, m := range got {
		if m.ConfirmedEndedAt != nil {
			confirmed = m
		} else {
			fresh = m
		}
	}
	require.Equal(t, "completed", confirmed.Status)
	require.True(t, endedAt.Equal(*confirmed.ConfirmedEndedAt))
	require.Equal(t, "active", fresh.Status)
}
