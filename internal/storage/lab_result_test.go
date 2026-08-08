package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func mustDate(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestLabResultRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewLabResultRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.LabResult{
		ID: "lab_1", UserID: "user1", DocumentID: "doc1",
		IndicatorName: "Гемоглобин", Code: "718-7", CodeSystem: "loinc",
		Value: 150, Unit: "г/л", NormalizedValue: 15, NormalizedUnit: "г/дл",
		ReferenceLow: 130, ReferenceHigh: 170, TakenAt: mustDate("2026-03-14"),
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "718-7", got[0].Code)
	require.Equal(t, 15.0, got[0].NormalizedValue)
	require.Equal(t, "2026-03-14", got[0].TakenAt.Format("2006-01-02"))
}

func TestLabResultRepository_HistoryByIndicator_OrderedOldestFirst(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewLabResultRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_1", UserID: "user1", DocumentID: "doc1", IndicatorName: "Гемоглобин", Value: 150, TakenAt: mustDate("2026-03-14")}))
	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_2", UserID: "user1", DocumentID: "doc2", IndicatorName: "Гемоглобин", Value: 144, TakenAt: mustDate("2025-05-08")}))
	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_3", UserID: "user1", DocumentID: "doc3", IndicatorName: "АЛТ", Value: 28, TakenAt: mustDate("2026-01-01")}))

	history, err := repo.HistoryByIndicator(ctx, "user1", "Гемоглобин")
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, 144.0, history[0].Value, "oldest (2025-05-08) first")
	require.Equal(t, 150.0, history[1].Value)
}

func TestLabResultRepository_LatestByIndicator(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewLabResultRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_1", UserID: "user1", DocumentID: "doc1", IndicatorName: "Гемоглобин", Value: 144, TakenAt: mustDate("2025-05-08")}))
	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_2", UserID: "user1", DocumentID: "doc2", IndicatorName: "Гемоглобин", Value: 150, TakenAt: mustDate("2026-03-14")}))
	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_3", UserID: "user1", DocumentID: "doc3", IndicatorName: "АЛТ", Value: 28, TakenAt: mustDate("2026-01-01")}))

	latest, err := repo.LatestByIndicator(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, 150.0, latest["Гемоглобин"].Value, "the more recent (2026-03-14) reading wins")
	require.Equal(t, 28.0, latest["АЛТ"].Value)
}

func TestLabResultRepository_LatestByIndicator_DatedReadingWinsOverUndated(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewLabResultRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_1", UserID: "user1", DocumentID: "doc1", IndicatorName: "Глюкоза", Value: 5.1, TakenAt: mustDate("2026-01-01")}))
	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_2", UserID: "user1", DocumentID: "doc2", IndicatorName: "Глюкоза", Value: 9.9, TakenAt: nil}))

	latest, err := repo.LatestByIndicator(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, 5.1, latest["Глюкоза"].Value, "an undated reading must never be treated as 'more recent' than a dated one")
}

func TestLabResultRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewLabResultRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.LabResult{ID: "lab_1", UserID: "user1", DocumentID: "doc1", IndicatorName: "Old"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.LabResult{
		{ID: "lab_1", UserID: "user1", DocumentID: "doc1", IndicatorName: "New A"},
		{ID: "lab_2", UserID: "user1", DocumentID: "doc1", IndicatorName: "New B"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 2)
}
