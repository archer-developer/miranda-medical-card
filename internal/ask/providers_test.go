package ask_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustDate(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestMedicationProvider_ReturnsAllHistory(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewMedicationRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1", DrugName: "Розувастатин",
		DoseAmount: 10, DoseUnit: "mg", Status: "active", StartedAt: mustDate("2025-05-14"),
	}))

	provider := ask.NewMedicationProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "Розувастатин")
	require.Contains(t, chunks[0].Content, "active")
	require.Equal(t, "doc1", chunks[0].DocumentID)
	require.Equal(t, 1.0, chunks[0].Confidence)
}

func TestPlannedActionProvider_DocumentSourced_FullConfidenceAndDocumentID(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewPlannedActionRepository(s)
	_, err := repo.Add(context.Background(), normalization.PlannedAction{
		UserID: "user1", SourceType: "document", SourceID: "doc1",
		Type: "lab_test", Description: "Повторный анализ глюкозы",
		DueDateFrom: mustDate("2026-06-01"), DueDateTo: mustDate("2026-08-01"),
	})
	require.NoError(t, err)

	provider := ask.NewPlannedActionProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "Повторный анализ глюкозы")
	require.Contains(t, chunks[0].Content, "2026-06-01")
	require.Contains(t, chunks[0].Content, "2026-08-01")
	require.Contains(t, chunks[0].Content, "ожидает")
	require.Equal(t, "doc1", chunks[0].DocumentID)
	require.Empty(t, chunks[0].EventID)
	require.Equal(t, 1.0, chunks[0].Confidence)
}

func TestPlannedActionProvider_SelfReported_ReducedConfidenceAndEventID(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewPlannedActionRepository(s)
	_, err := repo.Add(context.Background(), normalization.PlannedAction{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1",
		Type: "vaccination", Description: "Прививка от бешенства",
	})
	require.NoError(t, err)

	provider := ask.NewPlannedActionProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "selfevt_1", chunks[0].EventID)
	require.Empty(t, chunks[0].DocumentID)
	require.Equal(t, 0.9, chunks[0].Confidence)
}

func TestPlannedActionProvider_OverdueIsMentioned(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewPlannedActionRepository(s)
	_, err := repo.Add(context.Background(), normalization.PlannedAction{
		UserID: "user1", SourceType: "document", SourceID: "doc1",
		Type: "lab_test", Description: "Просроченный анализ", DueDateTo: mustDate("2020-01-01"),
	})
	require.NoError(t, err)

	provider := ask.NewPlannedActionProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Contains(t, chunks[0].Content, "Просрочено")
}

func TestPlannedActionProvider_CompletedAndDeclinedStatusInContent(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewPlannedActionRepository(s)
	completed, err := repo.Add(context.Background(), normalization.PlannedAction{
		UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "lab_test", Description: "Готово",
	})
	require.NoError(t, err)
	require.NoError(t, repo.MarkCompleted(context.Background(), completed.ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	declined, err := repo.Add(context.Background(), normalization.PlannedAction{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "other", Description: "Отменено",
	})
	require.NoError(t, err)
	require.NoError(t, repo.MarkDeclined(context.Background(), declined.ID, "user1"))

	provider := ask.NewPlannedActionProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "Collect must include resolved actions too, so the agent can answer 'did I already do this'")

	byTitle := map[string]string{}
	for _, c := range chunks {
		byTitle[c.Title] = c.Content
	}
	require.Contains(t, byTitle["Готово"], "выполнено")
	require.Contains(t, byTitle["Отменено"], "отменено")
}

func TestLabProvider_FiltersByIndicatorName(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 28.3, TakenAt: mustDate("2025-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc1", IndicatorName: "AST", Value: 21.5, TakenAt: mustDate("2025-01-01")}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", IndicatorName: "ALT"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "ALT")
}

func TestLabProvider_QualitativeValueFormattedAsText(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{
		ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "Группа крови",
		QualitativeValue: "A(II) Rh+", TakenAt: mustDate("2025-01-01"),
	}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "Группа крови: A(II) Rh+ (2025-01-01).", chunks[0].Content, "must not fall through to numeric %%g formatting for a qualitative-only result")
}

func TestLabProvider_OrdersMostRecentFirstAndHonorsLimit(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 1, TakenAt: mustDate("2025-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 2, TakenAt: mustDate("2026-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l3", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 3, TakenAt: mustDate("2025-06-01")}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", Limit: 2})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "limit must actually bound the result count")
	require.Contains(t, chunks[0].Content, "2026-01-01", "most recent result must come first")
	require.Contains(t, chunks[1].Content, "2025-06-01")
}

// TestLabProvider_NoLimitStillCapsResultCount is the regression test for a
// real bug: unlike every sibling provider (which all call limitOrDefault
// before hitting storage), LabProvider used to truncate only "if req.Limit
// > 0" — so a bare lab_results call with no indicatorName/limit at all
// (exactly what "show me my lab results" produces) fell through to
// ListByUser's entire, unbounded history. A large history like this one
// used to come back as a single oversized RoleTool message, which compounds
// across a multi-turn conversation into a big enough request to make the
// agent_provider return an empty completion (see format.go's
// maxToolResultBytes doc comment for the mechanism this bug fed).
func TestLabProvider_NoLimitStillCapsResultCount(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	for i := 0; i < 40; i++ {
		require.NoError(t, repo.Add(context.Background(), normalization.LabResult{
			ID: fmt.Sprintf("l%d", i), UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: float64(i),
			TakenAt: mustDate("2025-01-01"),
		}))
	}

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Less(t, len(chunks), 40, "an omitted limit must still fall back to a bounded default, not the entire history")
}

// TestLabProvider_IndicatorNameWithLimitReturnsMostRecent is the regression
// test for a real bug: HistoryByIndicator (the code path this hits, since
// IndicatorName is set) used to re-sort its results oldest-first, so
// "history of ALT, most recent 2" silently returned the *oldest* 2 instead.
// The sibling test above only exercises the no-IndicatorName ListByUser
// path, which was never broken — this one specifically covers the branch
// that was.
func TestLabProvider_IndicatorNameWithLimitReturnsMostRecent(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 1, TakenAt: mustDate("2025-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 2, TakenAt: mustDate("2026-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l3", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 3, TakenAt: mustDate("2025-06-01")}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", IndicatorName: "ALT", Limit: 2})
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	require.Contains(t, chunks[0].Content, "2026-01-01", "most recent result must come first")
	require.Contains(t, chunks[1].Content, "2025-06-01", "must be the 2nd-most-recent, not the oldest")
}

func TestLabProvider_FiltersByDateRange(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 1, TakenAt: mustDate("2026-07-24")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc1", IndicatorName: "AST", Value: 2, TakenAt: mustDate("2026-07-24")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l3", UserID: "user1", DocumentID: "doc2", IndicatorName: "ALT", Value: 3, TakenAt: mustDate("2025-01-01")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l4", UserID: "user1", DocumentID: "doc1", IndicatorName: "Группа крови", QualitativeValue: "A(II)"}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", From: mustDate("2026-07-24"), To: mustDate("2026-07-24")})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "must return every indicator taken on that date, and only that date — an undated row must not slip through")
}

func TestLabProvider_FiltersByDocumentID(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 1, TakenAt: mustDate("2026-07-24")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc1", IndicatorName: "AST", Value: 2, TakenAt: mustDate("2026-07-24")}))
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l3", UserID: "user1", DocumentID: "doc2", IndicatorName: "Мочевина", Value: 3, TakenAt: mustDate("2025-01-01")}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", DocumentID: "doc1"})
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	require.ElementsMatch(t, []string{"ALT", "AST"}, []string{chunks[0].Title, chunks[1].Title})
}

func TestLabProvider_DocumentIDNeverLeaksAnotherUsersResults(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewLabResultRepository(s)
	// Same documentId, but owned by a different user — e.g. a hallucinated
	// or guessed id must never let one household member read another's
	// results (ListByDocument itself has no user filter — see
	// LabProvider.Collect's filterByOwner call).
	require.NoError(t, repo.Add(context.Background(), normalization.LabResult{ID: "l1", UserID: "other-user", DocumentID: "shared-doc-id", IndicatorName: "ALT", Value: 1, TakenAt: mustDate("2026-07-24")}))

	provider := ask.NewLabProvider(repo)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", DocumentID: "shared-doc-id"})
	require.NoError(t, err)
	require.Empty(t, chunks, "a document owned by another user must never be returned")
}

func TestInstrumentalFindingProvider_RequiresBothStructureAndParameter(t *testing.T) {
	s := newTestStore(t)
	repo := storage.NewInstrumentalFindingRepository(s)
	require.NoError(t, repo.Add(context.Background(), normalization.InstrumentalFinding{
		ID: "f1", UserID: "user1", DocumentID: "doc1", Structure: "Печень", Parameter: "правая доля КВР",
		Value: 157, Unit: "мм", MeasuredAt: mustDate("2026-07-23"),
	}))

	provider := ask.NewInstrumentalFindingProvider(repo)

	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", Structure: "Печень"})
	require.NoError(t, err)
	require.Empty(t, chunks, "structure alone, no parameter, must return nothing rather than everything")

	chunks, err = provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", Structure: "Печень", Parameter: "правая доля КВР"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "157")
}

func TestProfileProvider_ReturnsBuiltProfileSections(t *testing.T) {
	s := newTestStore(t)
	profileStore := profile.NewStore(storage.NewProfileRepository(s))
	require.NoError(t, profileStore.Replace(context.Background(), profile.Profile{
		UserID:            "user1",
		ActiveMedications: []profile.MedicationSummary{{DrugName: "Розувастатин", DoseAmount: 10, DoseUnit: "mg"}},
		Allergies:         []profile.AllergySummary{{Substance: "Пенициллин"}},
		RebuiltAt:         time.Now(),
	}))

	provider := ask.NewProfileProvider(profileStore)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "one chunk for medications, one for allergies — diagnoses section skipped since empty")

	var medsChunk, allergiesChunk *ask.KnowledgeChunk
	for i := range chunks {
		if chunks[i].Title == "Текущие лекарства" {
			medsChunk = &chunks[i]
		}
		if chunks[i].Title == "Аллергии" {
			allergiesChunk = &chunks[i]
		}
	}
	require.NotNil(t, medsChunk)
	require.Contains(t, medsChunk.Content, "Розувастатин")
	require.NotNil(t, allergiesChunk)
	require.Contains(t, allergiesChunk.Content, "Пенициллин")
}

func TestProfileProvider_IncludesLatestLabResultsAndVitalSigns(t *testing.T) {
	s := newTestStore(t)
	profileStore := profile.NewStore(storage.NewProfileRepository(s))
	require.NoError(t, profileStore.Replace(context.Background(), profile.Profile{
		UserID: "user1",
		LatestLabResults: []profile.LabResultSummary{
			{IndicatorName: "Холестерин-ЛПНП", Value: 3.2, Unit: "ммоль/л", TakenAt: mustDate("2026-05-08")},
		},
		LatestVitalSigns: []profile.VitalSignSummary{
			{Type: "blood_pressure", Systolic: 120, Diastolic: 80, MeasuredAt: mustDate("2026-05-08")},
		},
		RebuiltAt: time.Now(),
	}))

	provider := ask.NewProfileProvider(profileStore)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 2, "one chunk for lab results, one for vital signs")

	var labsChunk, vitalsChunk *ask.KnowledgeChunk
	for i := range chunks {
		if chunks[i].Title == "Последние результаты анализов" {
			labsChunk = &chunks[i]
		}
		if chunks[i].Title == "Последние жизненные показатели" {
			vitalsChunk = &chunks[i]
		}
	}
	require.NotNil(t, labsChunk, "profile.LatestLabResults must be rendered, not silently dropped")
	require.Contains(t, labsChunk.Content, "Холестерин-ЛПНП: 3.2 ммоль/л (2026-05-08)")
	require.NotNil(t, vitalsChunk, "profile.LatestVitalSigns must be rendered, not silently dropped")
	require.Contains(t, vitalsChunk.Content, "Давление: 120/80 мм рт.ст. (2026-05-08)")
}

func TestProfileProvider_LabResultDocumentTitleDisambiguatesSameNamedIndicators(t *testing.T) {
	s := newTestStore(t)
	profileStore := profile.NewStore(storage.NewProfileRepository(s))
	require.NoError(t, profileStore.Replace(context.Background(), profile.Profile{
		UserID: "user1",
		LatestLabResults: []profile.LabResultSummary{
			{IndicatorName: "Белок", Value: 72, Unit: "г/л", TakenAt: mustDate("2026-05-08"), DocumentTitle: "Общий анализ крови"},
		},
		RebuiltAt: time.Now(),
	}))

	provider := ask.NewProfileProvider(profileStore)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "[Общий анализ крови]", "without the source document named, a blood-panel 'Белок' reads identically to a urinalysis 'Белок' and is misleading")
}

func TestProfileProvider_NoProfileYetReturnsNoChunks(t *testing.T) {
	s := newTestStore(t)
	provider := ask.NewProfileProvider(profile.NewStore(storage.NewProfileRepository(s)))
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Empty(t, chunks)
}

func TestDocumentProvider_SearchesFTS(t *testing.T) {
	s := newTestStore(t)
	fts := storage.NewFTSRepository(s)
	require.NoError(t, fts.IndexDocument(context.Background(), "user1", "doc1", "Выписка", "жалобы на бессонницу"))

	provider := ask.NewDocumentProvider(fts, storage.NewDocumentRepository(s))
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", Query: "бессонниц"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "doc1", chunks[0].DocumentID)
}

func TestDocumentProvider_EmptyQueryReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	provider := ask.NewDocumentProvider(storage.NewFTSRepository(s), storage.NewDocumentRepository(s))
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Empty(t, chunks)
}

func TestDocumentProvider_FetchesFullSummaryByDocumentID(t *testing.T) {
	s := newTestStore(t)
	docs := storage.NewDocumentRepository(s)
	_, err := docs.Add(context.Background(), storage.MedicalDocument{
		ID: "doc1", UserID: "user1", Status: storage.DocumentStatusReady, Title: "Анализ крови",
		Summary: "Клинический анализ крови. Рекомендации: снизить потребление соли; повторить анализ через месяц.",
	})
	require.NoError(t, err)

	provider := ask.NewDocumentProvider(storage.NewFTSRepository(s), docs)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", DocumentID: "doc1"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Contains(t, chunks[0].Content, "снизить потребление соли", "must return the full Summary, not a truncated FTS snippet")
	require.Equal(t, "doc1", chunks[0].DocumentID)
}

func TestDocumentProvider_UnknownOrForeignDocumentIDReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	docs := storage.NewDocumentRepository(s)
	_, err := docs.Add(context.Background(), storage.MedicalDocument{
		ID: "doc1", UserID: "other-user", Status: storage.DocumentStatusReady, Title: "Чужой документ",
		Summary: "Не должно быть видно.",
	})
	require.NoError(t, err)

	provider := ask.NewDocumentProvider(storage.NewFTSRepository(s), docs)
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1", DocumentID: "doc1"})
	require.NoError(t, err, "an unknown or another user's document id must resolve to no results, not an error")
	require.Empty(t, chunks)
}
