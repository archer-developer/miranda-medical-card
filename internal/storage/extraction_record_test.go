package storage_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestExtractionRepository_Add_AlwaysStartsInactive(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewExtractionRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.ExtractionRecord{
		DocumentID: "doc1", Version: 1, Active: true, // Active: true is deliberately ignored — see Add's doc comment
		Raw: json.RawMessage(`{"documentType":"lab_report"}`),
	})
	require.NoError(t, err)
	require.False(t, added.Active, "Add must never leave a row active — Activate is the only path that does")

	_, err = repo.GetActive(ctx, "doc1")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestExtractionRepository_ActivateSwitchesAtomically(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewExtractionRepository(newTestStore(t))

	v1, err := repo.Add(ctx, storage.ExtractionRecord{DocumentID: "doc1", Version: 1, Raw: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.NoError(t, repo.Activate(ctx, v1.ID))

	active, err := repo.GetActive(ctx, "doc1")
	require.NoError(t, err)
	require.Equal(t, v1.ID, active.ID)

	v2, err := repo.Add(ctx, storage.ExtractionRecord{DocumentID: "doc1", Version: 2, Raw: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.NoError(t, repo.Activate(ctx, v2.ID))

	active, err = repo.GetActive(ctx, "doc1")
	require.NoError(t, err)
	require.Equal(t, v2.ID, active.ID, "activating v2 must deactivate v1")

	versions, err := repo.ListVersions(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	activeCount := 0
	for _, v := range versions {
		if v.Active {
			activeCount++
		}
	}
	require.Equal(t, 1, activeCount, "exactly one version must be active at a time")
}

func TestExtractionRepository_ListVersions_OrderedByVersionAscending(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewExtractionRepository(newTestStore(t))

	_, err := repo.Add(ctx, storage.ExtractionRecord{DocumentID: "doc1", Version: 2, Raw: json.RawMessage(`{}`)})
	require.NoError(t, err)
	_, err = repo.Add(ctx, storage.ExtractionRecord{DocumentID: "doc1", Version: 1, Raw: json.RawMessage(`{}`)})
	require.NoError(t, err)

	versions, err := repo.ListVersions(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, 1, versions[0].Version)
	require.Equal(t, 2, versions[1].Version)
}

func TestExtractionRepository_RawRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewExtractionRepository(newTestStore(t))

	raw := json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"ALT","value":28.3}]}`)
	added, err := repo.Add(ctx, storage.ExtractionRecord{DocumentID: "doc1", Version: 1, Raw: raw})
	require.NoError(t, err)

	versions, err := repo.ListVersions(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.JSONEq(t, string(raw), string(versions[0].Raw))
	require.Equal(t, added.ID, versions[0].ID)
}

func TestExtractionRepository_Activate_UnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewExtractionRepository(newTestStore(t))

	err := repo.Activate(ctx, "extr_does_not_exist")
	require.ErrorIs(t, err, storage.ErrNotFound)
}
