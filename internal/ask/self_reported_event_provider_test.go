package ask_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

func TestSelfReportedEventProvider_ReturnsOnlySelfReportedTypes(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2026-03-12"), Type: "symptom",
		Title: "Симптом", Summary: "Головная боль", SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_1",
	}))
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_2", UserID: "user1", Date: *mustDate("2026-03-13"), Type: "medication_taken",
		Title: "Приём", Summary: "Ибупрофен 200мг", SourceEntityType: "medication_intake", SourceEntityID: "intake_1",
	}))
	// A document-derived event of a different type must not leak through —
	// this is exactly the noise self_reported_events exists to filter out
	// of what the generic timeline tool would otherwise return.
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_3", UserID: "user1", Date: *mustDate("2026-03-14"), Type: "lab_result",
		Title: "Анализ крови", DocumentID: "doc1",
	}))

	provider := ask.NewSelfReportedEventProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "must include symptom and medication_taken, exclude lab_result")

	for _, c := range chunks {
		require.Equal(t, "self_reported_events", c.Source)
		require.Equal(t, 0.90, c.Confidence, "self-reported events always score below document-derived facts")
		require.Empty(t, c.DocumentID, "self-reported events have no backing document")
	}
}

func TestSelfReportedEventProvider_FiltersByDateRange(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2025-01-01"), Type: "symptom", Title: "Old",
		SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_1",
	}))
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_2", UserID: "user1", Date: *mustDate("2026-06-01"), Type: "symptom", Title: "In range",
		SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_2",
	}))

	provider := ask.NewSelfReportedEventProvider(repo)
	from, to := mustDate("2026-01-01"), mustDate("2026-12-31")
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", From: from, To: to})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "selfevt_2", chunks[0].EventID)
}

func TestSelfReportedEventProvider_NoEventsReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	provider := ask.NewSelfReportedEventProvider(storage.NewTimelineRepository(s))

	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Empty(t, chunks)
}
