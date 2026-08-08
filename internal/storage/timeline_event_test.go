package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

func TestTimelineRepository_AddAndList(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2026-03-01"), Type: "diagnosis",
		Title: "Гипертония", DocumentID: "doc1", SourceEntityType: "diagnosis", SourceEntityID: "dx_1",
	}))
	require.NoError(t, repo.Add(ctx, timeline.Event{
		ID: "evt_2", UserID: "user1", Date: *mustDate("2026-05-01"), Type: "lab_result",
		Title: "Анализ крови", DocumentID: "doc1",
	}))

	events, err := repo.List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "evt_2", events[0].ID, "newest first by default")
}

func TestTimelineRepository_List_FiltersByDateRange(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_1", UserID: "user1", Date: *mustDate("2025-01-01"), Type: "diagnosis", Title: "Old"}))
	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_2", UserID: "user1", Date: *mustDate("2026-06-01"), Type: "diagnosis", Title: "In range"}))
	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_3", UserID: "user1", Date: *mustDate("2027-01-01"), Type: "diagnosis", Title: "Future"}))

	from, to := mustDate("2026-01-01"), mustDate("2026-12-31")
	events, err := repo.List(ctx, "user1", storage.TimelineFilter{From: from, To: to})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "In range", events[0].Title)
}

func TestTimelineRepository_List_FiltersByType(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_1", UserID: "user1", Date: *mustDate("2026-01-01"), Type: "diagnosis", Title: "A"}))
	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_2", UserID: "user1", Date: *mustDate("2026-01-02"), Type: "lab_result", Title: "B"}))

	events, err := repo.List(ctx, "user1", storage.TimelineFilter{Types: []string{"lab_result"}})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "B", events[0].Title)
}

func TestTimelineRepository_List_RespectsLimit(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	for i, d := range []string{"2026-01-01", "2026-02-01", "2026-03-01"} {
		require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_" + d, UserID: "user1", Date: *mustDate(d), Type: "diagnosis", Title: string(rune('A' + i))}))
	}

	events, err := repo.List(ctx, "user1", storage.TimelineFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, events, 2)
}

func TestTimelineRepository_RemoveByDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_1", UserID: "user1", Date: *mustDate("2026-01-01"), Type: "diagnosis", Title: "A", DocumentID: "doc1"}))
	require.NoError(t, repo.Add(ctx, timeline.Event{ID: "evt_2", UserID: "user1", Date: *mustDate("2026-01-01"), Type: "diagnosis", Title: "B", DocumentID: "doc2"}))

	require.NoError(t, repo.RemoveByDocument(ctx, "doc1"))

	events, err := repo.List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "B", events[0].Title)
}

func TestTimelineRepository_RemoveBySourceEntity(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewTimelineRepository(newTestStore(t))

	require.NoError(t, repo.Add(ctx, timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2026-01-01"), Type: "symptom", Title: "Headache",
		SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_1",
	}))

	require.NoError(t, repo.RemoveBySourceEntity(ctx, "self_reported_event", "selfevt_1"))

	events, err := repo.List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Empty(t, events)
}
