package ask_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

// mustDateEndOfDay parses a YYYY-MM-DD string and returns a pointer to the
// last nanosecond of that day in UTC — what decodeKnowledgeRequest produces
// after applying the end-of-day fix to a model-supplied to date.
func mustDateEndOfDay(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	end := t.Add(24*time.Hour - time.Nanosecond)
	return &end
}

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

// TestSelfReportedEventProvider_DistinctChunksForSameDaySameText guards the
// fix for same-day deduplication: multiple events logged on the same calendar
// date with identical Summary text used to produce identical Content strings,
// causing RankChunks' exact-content dedup to collapse them into one. Including
// HH:MM in the Content format makes each event's chunk unique.
func TestSelfReportedEventProvider_DistinctChunksForSameDaySameText(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)

	// Three separate fetal-movement observations on the same day, same wording.
	times := []string{"2026-09-15T09:00:00Z", "2026-09-15T12:38:00Z", "2026-09-15T18:21:00Z"}
	for i, ts := range times {
		t0, err := time.Parse(time.RFC3339, ts)
		require.NoError(t, err)
		require.NoError(t, repo.Add(context.Background(), timeline.Event{
			ID: fmt.Sprintf("evt_%d", i), UserID: "user1", Date: t0, Type: "symptom",
			Title: "Наблюдение", Summary: "Отмечены шевеления ребенка.",
			SourceEntityType: "self_reported_event", SourceEntityID: fmt.Sprintf("selfevt_%d", i),
		}))
	}

	provider := ask.NewSelfReportedEventProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 3, "each event must produce a distinct chunk even with identical Summary text")

	contents := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		require.False(t, contents[c.Content], "duplicate Content %q — HH:MM must distinguish same-day events", c.Content)
		contents[c.Content] = true
	}
}

// TestSelfReportedEventProvider_SameDayFilterIncludesAllHours verifies that
// filtering with a To time of end-of-day (23:59:59.999…) returns events logged
// at any hour of that day, not just at midnight.
func TestSelfReportedEventProvider_SameDayFilterIncludesAllHours(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewTimelineRepository(s)

	sameDayTimes := []string{
		"2026-09-15T04:37:00Z", "2026-09-15T09:00:00Z",
		"2026-09-15T18:58:00Z", "2026-09-15T23:59:59Z",
	}
	for i, ts := range sameDayTimes {
		t0, err := time.Parse(time.RFC3339, ts)
		require.NoError(t, err)
		require.NoError(t, repo.Add(context.Background(), timeline.Event{
			ID: fmt.Sprintf("evt_%d", i), UserID: "user1", Date: t0, Type: "symptom",
			Title: "Шевеление", Summary: fmt.Sprintf("Эпизод %d", i),
			SourceEntityType: "self_reported_event", SourceEntityID: fmt.Sprintf("selfevt_%d", i),
		}))
	}
	// One event the day before — must not appear.
	prev, err := time.Parse(time.RFC3339, "2026-09-14T23:00:00Z")
	require.NoError(t, err)
	require.NoError(t, repo.Add(context.Background(), timeline.Event{
		ID: "evt_prev", UserID: "user1", Date: prev, Type: "symptom",
		Title: "Шевеление", Summary: "Вчера",
		SourceEntityType: "self_reported_event", SourceEntityID: "selfevt_prev",
	}))

	midnight := mustDate("2026-09-15")                                         // 00:00:00 UTC — what time.Parse gives
	endOfDay := mustDateEndOfDay("2026-09-15")                                 // 23:59:59.999… UTC — what the fix produces
	provider := ask.NewSelfReportedEventProvider(repo)

	// Passing midnight as To (the old, broken behaviour) returns nothing on that day.
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{
		UserID: "user1", From: midnight, To: midnight,
	})
	require.NoError(t, err)
	require.Empty(t, chunks, "midnight To must exclude all same-day events (confirming the bug that was fixed)")

	// Passing end-of-day as To (the fixed behaviour) returns all four events.
	chunks, err = provider.Collect(context.Background(), ask.KnowledgeRequest{
		UserID: "user1", From: midnight, To: endOfDay,
	})
	require.NoError(t, err)
	require.Len(t, chunks, 4, "end-of-day To must include all events logged that day")
}
