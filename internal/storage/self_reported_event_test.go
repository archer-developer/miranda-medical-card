package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestSelfReportedEventRepository_AddAssignsIDAndDefaults(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewSelfReportedEventRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "Приступ головной боли"})
	require.NoError(t, err)
	require.NotEmpty(t, added.ID)
	require.Equal(t, storage.DocumentStatusPending, added.Status)
	require.False(t, added.LoggedAt.IsZero())
	require.Equal(t, added.LoggedAt, added.OccurredAt, "OccurredAt defaults to LoggedAt when not given")

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, "Приступ головной боли", got.RawText)
}

func TestSelfReportedEventRepository_Get_WrongUserReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewSelfReportedEventRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "text"})
	require.NoError(t, err)

	_, err = repo.Get(ctx, added.ID, "user2")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestSelfReportedEventRepository_UpdateExtracted(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewSelfReportedEventRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "text"})
	require.NoError(t, err)

	require.NoError(t, repo.UpdateExtracted(ctx, added.ID, "user1", "symptom", "Головная боль", "intake_1"))

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, "symptom", got.Category)
	require.Equal(t, "Головная боль", got.Description)
	require.Equal(t, "intake_1", got.MedicationIntakeID)
}

func TestSelfReportedEventRepository_RawTextNeverLostWhenExtractionEmpty(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewSelfReportedEventRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "неструктурируемый текст"})
	require.NoError(t, err)
	require.NoError(t, repo.UpdateStatus(ctx, added.ID, "user1", storage.DocumentStatusReady))

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, got.Status, "status still reaches READY even with no structured fields")
	require.Equal(t, "неструктурируемый текст", got.RawText)
	require.Empty(t, got.Category)
}

func TestSelfReportedEventRepository_Remove(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewSelfReportedEventRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "text"})
	require.NoError(t, err)
	require.NoError(t, repo.Remove(ctx, added.ID, "user1"))

	_, err = repo.Get(ctx, added.ID, "user1")
	require.ErrorIs(t, err, storage.ErrNotFound)
}
