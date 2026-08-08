package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestProcedureRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProcedureRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Procedure{
		ID: "proc_1", UserID: "user1", DocumentID: "doc1",
		Type: "examination", Name: "УЗИ органов брюшной полости", PerformedAt: mustDate("2026-07-23"),
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "examination", got[0].Type)
}

func TestProcedureRepository_ListVaccinations(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProcedureRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Procedure{ID: "proc_1", UserID: "user1", DocumentID: "doc1", Type: "vaccination", Name: "Грипп"}))
	require.NoError(t, repo.Add(ctx, normalization.Procedure{ID: "proc_2", UserID: "user1", DocumentID: "doc1", Type: "examination", Name: "УЗИ"}))

	vaccinations, err := repo.ListVaccinations(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, vaccinations, 1)
	require.Equal(t, "Грипп", vaccinations[0].Name)
}

func TestProcedureRepository_ReplaceForDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewProcedureRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, normalization.Procedure{ID: "proc_1", UserID: "user1", DocumentID: "doc1", Name: "Old"}))
	require.NoError(t, repo.ReplaceForDocument(ctx, "doc1", []normalization.Procedure{
		{ID: "proc_1", UserID: "user1", DocumentID: "doc1", Name: "New"},
	}))

	got, err := repo.ListByDocument(ctx, "doc1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].Name)
}
