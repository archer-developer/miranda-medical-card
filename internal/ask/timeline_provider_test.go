package ask_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

func TestTimelineProvider_DocumentDerivedEventGetsFullConfidence(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2026-03-12"), Type: "lab_result",
		Title: "Общий анализ крови", Summary: "ALT повышен", DocumentID: "doc1",
	}))

	provider := ask.NewTimelineProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, 1.0, chunks[0].Confidence)
	require.Equal(t, "doc1", chunks[0].DocumentID)
	require.Empty(t, chunks[0].EventID)
}

func TestTimelineProvider_SelfReportedEventGetsLowerConfidence(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_1", UserID: "user1", Date: *mustDate("2026-03-12"), Type: "symptom",
		Title: "Симптом", Summary: "Головная боль", SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_1",
	}))

	provider := ask.NewTimelineProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Less(t, chunks[0].Confidence, 1.0, "unverifiable self-reported facts must score lower than document-derived ones")
	require.Equal(t, "selfevt_1", chunks[0].EventID)
	require.Empty(t, chunks[0].DocumentID)
}
