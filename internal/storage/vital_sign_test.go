package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestVitalSignRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewVitalSignRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.VitalSign{
		ID: "vital_1", UserID: "user1", DocumentID: "doc1",
		Type: "blood_pressure", Systolic: 130, Diastolic: 82, MeasuredAt: mustDate("2026-07-23"),
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 130.0, got[0].Systolic)
	require.Equal(t, 82.0, got[0].Diastolic)
}

func TestVitalSignRepository_LatestByType(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewVitalSignRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.VitalSign{ID: "vital_1", UserID: "user1", DocumentID: "doc1", Type: "weight", Value: 80, Unit: "kg", MeasuredAt: mustDate("2025-01-01")}))
	require.NoError(t, repo.Add(ctx, normalization.VitalSign{ID: "vital_2", UserID: "user1", DocumentID: "doc2", Type: "weight", Value: 78, Unit: "kg", MeasuredAt: mustDate("2026-07-23")}))

	latest, err := repo.LatestByType(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, 78.0, latest["weight"].Value)
}

func TestVitalSignRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewVitalSignRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.VitalSign{ID: "vital_1", UserID: "user1", DocumentID: "doc1", Type: "pulse", Value: 70}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.VitalSign{
		{ID: "vital_1", UserID: "user1", DocumentID: "doc1", Type: "pulse", Value: 72},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 72.0, got[0].Value)
}
