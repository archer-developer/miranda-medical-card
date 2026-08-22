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

func TestMedicationRepository_ReplaceForDocument_ReplacesDefaultPrescribedRows(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Old", Status: normalization.MedicationStatusPrescribed,
	}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "New", Status: normalization.MedicationStatusPrescribed},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].DrugName)
}

func TestMedicationRepository_MarkStartedManually(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusPrescribed,
	}))

	startedAt := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.MarkStartedManually(ctx, "med_1", "user1", startedAt))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.MedicationStatusActive, got[0].Status)
	require.True(t, startedAt.Equal(*got[0].StartedAt))
}

func TestMedicationRepository_MarkStartedManually_ScopedToOwningUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusPrescribed,
	}))
	require.NoError(t, repo.MarkStartedManually(ctx, "med_1", "user2", time.Now()))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.MedicationStatusPrescribed, got[0].Status, "MarkStartedManually for a different user must be a no-op")
}

func TestMedicationRepository_MarkEndedManually(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusActive,
	}))

	endedAt := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.MarkEndedManually(ctx, "med_1", "user1", endedAt))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.MedicationStatusCompleted, got[0].Status)
	require.True(t, endedAt.Equal(*got[0].EndedAt))
}

func TestMedicationRepository_MarkEndedManually_ScopedToOwningUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusActive,
	}))
	require.NoError(t, repo.MarkEndedManually(ctx, "med_1", "user2", time.Now()))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.MedicationStatusActive, got[0].Status, "MarkEndedManually for a different user must be a no-op")
}

func TestMedicationRepository_ReplaceForDocument_PreservesNonDefaultStatusRow(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	// The user confirmed intake started (status moved past "prescribed").
	startedAt := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin",
		Status: normalization.MedicationStatusActive, StartedAt: &startedAt,
	}))

	// Reprocess doc1 — a fresh extraction still only knows this drug was
	// prescribed (e.g. the document itself never confirms intake started),
	// which must not overwrite the user's own confirmation nor duplicate the
	// row for the same drug.
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_new", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusPrescribed},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1, "the surviving active row must not be duplicated by a fresh extraction of the same drug")
	require.Equal(t, "med_1", got[0].ID, "the surviving row's id must be untouched")
	require.Equal(t, normalization.MedicationStatusActive, got[0].Status)
	require.True(t, startedAt.Equal(*got[0].StartedAt))
}

func TestMedicationRepository_ReplaceForDocument_DedupIsCaseAndWhitespaceInsensitive(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "  Амоксициллин  ", Status: normalization.MedicationStatusActive,
	}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_new", UserID: "user1", DocumentID: "doc1", DrugName: "амоксициллин", Status: normalization.MedicationStatusPrescribed},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1, "case/whitespace-only differences in drug name must still count as the same drug for dedup")
	require.Equal(t, "med_1", got[0].ID)
}

func TestMedicationRepository_ReplaceForDocument_InsertsDifferentDrugAlongsideSurvivor(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusActive,
	}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Medication{
		{ID: "med_a", UserID: "user1", DocumentID: "doc1", DrugName: "Amoxicillin", Status: normalization.MedicationStatusPrescribed},
		{ID: "med_b", UserID: "user1", DocumentID: "doc1", DrugName: "Ibuprofen", Status: normalization.MedicationStatusPrescribed},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 2, "a genuinely different drug must still be inserted even though another drug in the same document survived")
	names := []string{got[0].DrugName, got[1].DrugName}
	require.ElementsMatch(t, []string{"Amoxicillin", "Ibuprofen"}, names)
}
