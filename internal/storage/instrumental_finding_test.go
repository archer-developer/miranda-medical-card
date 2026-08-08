package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestInstrumentalFindingRepository_AddAndListByDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewInstrumentalFindingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{
		ID: "finding_1", UserID: "user1", DocumentID: "doc1",
		Structure: "Печень", Parameter: "правая доля КВР",
		Value: 157, Unit: "мм", MeasuredAt: mustDate("2026-07-23"),
	}))
	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{
		ID: "finding_2", UserID: "user1", DocumentID: "doc1",
		Structure: "Печень", Parameter: "контуры", QualitativeValue: "ровные",
		MeasuredAt: mustDate("2026-07-23"),
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 2)

	var numeric, qualitative *normalization.InstrumentalFinding
	for i := range got {
		if got[i].Parameter == "правая доля КВР" {
			numeric = &got[i]
		}
		if got[i].Parameter == "контуры" {
			qualitative = &got[i]
		}
	}
	require.NotNil(t, numeric)
	require.Equal(t, 157.0, numeric.Value)
	require.Empty(t, numeric.QualitativeValue)

	require.NotNil(t, qualitative)
	require.Equal(t, "ровные", qualitative.QualitativeValue)
	require.Zero(t, qualitative.Value)
}

func TestInstrumentalFindingRepository_HistoryByStructureParameter(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewInstrumentalFindingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{
		ID: "finding_1", UserID: "user1", DocumentID: "doc1",
		Structure: "Печень", Parameter: "правая доля КВР", Value: 149, Unit: "мм", MeasuredAt: mustDate("2025-02-10"),
	}))
	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{
		ID: "finding_2", UserID: "user1", DocumentID: "doc2",
		Structure: "Печень", Parameter: "правая доля КВР", Value: 157, Unit: "мм", MeasuredAt: mustDate("2026-07-23"),
	}))
	// Different structure with the same parameter name must not be mixed in
	// — the compound (structure, parameter) key is the whole point of this
	// repository, see docs/domain/13-instrumental-finding.md §3.
	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{
		ID: "finding_3", UserID: "user1", DocumentID: "doc2",
		Structure: "Почка правая", Parameter: "правая доля КВР", Value: 42, Unit: "мм", MeasuredAt: mustDate("2026-07-23"),
	}))

	history, err := repo.HistoryByStructureParameter(ctx, "user1", "Печень", "правая доля КВР")
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, 149.0, history[0].Value, "oldest first")
	require.Equal(t, 157.0, history[1].Value)
}

func TestInstrumentalFindingRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewInstrumentalFindingRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.InstrumentalFinding{ID: "finding_1", UserID: "user1", DocumentID: "doc1", Structure: "Old", Parameter: "p"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.InstrumentalFinding{
		{ID: "finding_1", UserID: "user1", DocumentID: "doc1", Structure: "New", Parameter: "p"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].Structure)
}
