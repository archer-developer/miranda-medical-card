package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestMedicationIntakeRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationIntakeRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.MedicationIntake{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1",
		DrugName: "ибупрофен", DoseAmount: 400, DoseUnit: "mg", TakenAt: *mustDate("2026-03-01"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, added.ID)

	got, err := repo.ListByUser(ctx, "user1", storage.DateRange{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "ибупрофен", got[0].DrugName)
}

func TestMedicationIntakeRepository_ListByUser_FiltersByDateRange(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationIntakeRepository(newTestStore(t))

	require.NoError(t, addIntake(t, repo, "old", "2025-01-01"))
	require.NoError(t, addIntake(t, repo, "in-range", "2026-06-01"))
	require.NoError(t, addIntake(t, repo, "future", "2027-01-01"))

	from, to := mustDate("2026-01-01"), mustDate("2026-12-31")
	got, err := repo.ListByUser(ctx, "user1", storage.DateRange{From: from, To: to})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "in-range", got[0].DrugName)
}

func addIntake(t *testing.T, repo storage.MedicationIntakeRepository, drugName, date string) error {
	t.Helper()
	_, err := repo.Add(context.Background(), storage.MedicationIntake{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_" + date,
		DrugName: drugName, TakenAt: *mustDate(date),
	})
	return err
}

func TestMedicationIntakeRepository_RemoveBySource(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMedicationIntakeRepository(newTestStore(t))

	_, err := repo.Add(ctx, storage.MedicationIntake{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", DrugName: "A"})
	require.NoError(t, err)
	_, err = repo.Add(ctx, storage.MedicationIntake{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_2", DrugName: "B"})
	require.NoError(t, err)

	require.NoError(t, repo.RemoveBySource(ctx, "self_reported", "selfevt_1"))

	got, err := repo.ListByUser(ctx, "user1", storage.DateRange{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "B", got[0].DrugName)
}
