package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestPlannedActionRepository_Add(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1",
		Type: "vaccination", Description: "Прививка от бешенства", DueDateTo: mustDate("2026-07-19"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, stored.ID, "Add must generate an id when none is given")
	require.Equal(t, normalization.PlannedActionStatusPending, stored.Status, "Add must default Status to pending")

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, stored.ID, got[0].ID)
	require.Equal(t, "self_reported", got[0].SourceType)
	require.Equal(t, "selfevt_1", got[0].SourceID)
}

func TestPlannedActionRepository_ListPending_ExcludesResolved(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	_, err := repo.Add(ctx, normalization.PlannedAction{ID: "plan_pending", UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "other", Description: "pending"})
	require.NoError(t, err)
	_, err = repo.Add(ctx, normalization.PlannedAction{ID: "plan_completed", UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "other", Description: "completed", Status: normalization.PlannedActionStatusCompleted})
	require.NoError(t, err)

	pending, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "plan_pending", pending[0].ID)

	all, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestPlannedActionRepository_MarkCompleted(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "lab_test", Description: "glucose"})
	require.NoError(t, err)

	at := *mustDate("2026-06-01")
	require.NoError(t, repo.MarkCompleted(ctx, stored.ID, "doc2", "lab_9", at))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.PlannedActionStatusCompleted, got[0].Status)
	require.Equal(t, "doc2", got[0].MatchedDocumentID)
	require.Equal(t, "lab_9", got[0].MatchedEntityID)
	require.NotNil(t, got[0].MatchedAt)

	pending, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestPlannedActionRepository_MarkDeclined(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "other", Description: "x"})
	require.NoError(t, err)

	require.NoError(t, repo.MarkDeclined(ctx, stored.ID, "user1"))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusDeclined, got[0].Status)
}

func TestPlannedActionRepository_MarkDeclined_ScopedByUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "other", Description: "x"})
	require.NoError(t, err)

	// A different user id cannot decline someone else's action — same
	// "mismatch indistinguishable from not-found" posture as delete_event
	// (docs/mcp/01-overview.md §11): the write silently affects nothing.
	require.NoError(t, repo.MarkDeclined(ctx, stored.ID, "someone-else"))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusPending, got[0].Status)
}

func TestPlannedActionRepository_MarkCompletedManually(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "vaccination", Description: "rabies shot"})
	require.NoError(t, err)

	at := *mustDate("2026-06-01")
	require.NoError(t, repo.MarkCompletedManually(ctx, stored.ID, "user1", at))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusCompleted, got[0].Status)
	require.NotNil(t, got[0].MatchedAt)
	require.Empty(t, got[0].MatchedDocumentID, "manual completion has no closing document")
	require.Empty(t, got[0].MatchedEntityID, "manual completion has no closing entity")

	pending, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestPlannedActionRepository_MarkCompletedManually_ScopedByUser(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "vaccination", Description: "rabies shot"})
	require.NoError(t, err)

	// Same "mismatch indistinguishable from not-found" posture as
	// MarkDeclined — see TestPlannedActionRepository_MarkDeclined_ScopedByUser.
	require.NoError(t, repo.MarkCompletedManually(ctx, stored.ID, "someone-else", *mustDate("2026-06-01")))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusPending, got[0].Status)
}

func TestPlannedActionRepository_ClearMatchesFromDocument(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "lab_test", Description: "glucose"})
	require.NoError(t, err)
	require.NoError(t, repo.MarkCompleted(ctx, stored.ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	require.NoError(t, repo.ClearMatchesFromDocument(ctx, "doc2"))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusPending, got[0].Status)
	require.Empty(t, got[0].MatchedDocumentID)
	require.Empty(t, got[0].MatchedEntityID)
	require.Nil(t, got[0].MatchedAt)
}

func TestPlannedActionRepository_ClearMatchesFromDocument_LeavesDeclinedAlone(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	stored, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "document", SourceID: "doc1", Type: "other", Description: "x"})
	require.NoError(t, err)
	require.NoError(t, repo.MarkDeclined(ctx, stored.ID, "user1"))

	// A declined action was never "matched by" any document — clearing
	// matches for some unrelated document must not resurrect it as pending.
	require.NoError(t, repo.ClearMatchesFromDocument(ctx, "doc2"))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusDeclined, got[0].Status)
}

func TestPlannedActionRepository_RemoveBySource(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	_, err := repo.Add(ctx, normalization.PlannedAction{UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1", Type: "other", Description: "x"})
	require.NoError(t, err)

	require.NoError(t, repo.RemoveBySource(ctx, "self_reported", "selfevt_1"))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestPlannedActionRepository_RemoveBySource_NoopWhenNothingMatches(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))
	require.NoError(t, repo.RemoveBySource(ctx, "self_reported", "does-not-exist"))
}

// --- ReplaceForSource reconciliation ---

func TestPlannedActionRepository_ReplaceForSource_InsertsFreshOnFirstUpload(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.PlannedActionStatusPending, got[0].Status)
}

func TestPlannedActionRepository_ReplaceForSource_PreservesCompletedRowAndInsertsFreshPendingOnReprocess(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "Повторный анализ глюкозы через полгода", MatchIndicatorName: "Глюкоза"},
	}))
	first, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NoError(t, repo.MarkCompleted(ctx, first[0].ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	// Reprocess doc1 with a differently-worded but same-key recommendation.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "Контроль глюкозы крови через 6 месяцев", MatchIndicatorName: "Глюкоза"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 2, "the completed row is left alone, and the new extraction is inserted as its own fresh pending row — no attempt to reconcile them by content")
	completed, pending := got[0], got[1]
	if completed.Status != normalization.PlannedActionStatusCompleted {
		completed, pending = pending, completed
	}
	require.Equal(t, first[0].ID, completed.ID, "the completed row's id must survive untouched")
	require.Equal(t, normalization.PlannedActionStatusCompleted, completed.Status)
	require.Equal(t, "doc2", completed.MatchedDocumentID)
	require.Equal(t, "Повторный анализ глюкозы через полгода", completed.Description, "the completed row's own description is not rewritten by the reprocess")
	require.Equal(t, normalization.PlannedActionStatusPending, pending.Status)
	require.Equal(t, "Контроль глюкозы крови через 6 месяцев", pending.Description)
}

func TestPlannedActionRepository_ReplaceForSource_SkipsExactDescriptionDuplicateOnReprocess(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "Повторный анализ глюкозы через полгода", MatchIndicatorName: "Глюкоза"},
	}))
	first, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NoError(t, repo.MarkCompleted(ctx, first[0].ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	// Reprocess doc1 — the fresh extraction re-describes the exact same
	// recommendation verbatim (only whitespace/case differ), which must not
	// be inserted as a second, indistinguishable pending row.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "  повторный анализ глюкозы через полгода  ", MatchIndicatorName: "Глюкоза"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1, "an exact (case/whitespace-insensitive) description match against the surviving completed row must not be duplicated")
	require.Equal(t, first[0].ID, got[0].ID)
	require.Equal(t, normalization.PlannedActionStatusCompleted, got[0].Status)
}

func TestPlannedActionRepository_ReplaceForSource_InsertsDifferentDescriptionAlongsideSurvivor(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "Повторный анализ глюкозы через полгода", MatchIndicatorName: "Глюкоза"},
	}))
	first, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.NoError(t, repo.MarkCompleted(ctx, first[0].ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "vaccination", Description: "Прививка от бешенства"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 2, "a genuinely different recommendation must still be inserted even though another one from the same source survived")
}

func TestPlannedActionRepository_ReplaceForSource_DeletesStalePendingOnReprocess(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))

	// Reprocess doc1 and this time the recommendation isn't extracted at all.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", nil))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, got, "a still-pending row must be discarded on reprocess regardless of what the new extraction contains")
}

func TestPlannedActionRepository_ReplaceForSource_NeverDeletesCompletedOnReprocess(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))
	first, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.NoError(t, repo.MarkCompleted(ctx, first[0].ID, "doc2", "lab_9", *mustDate("2026-06-01")))

	// Reprocess doc1 and the recommendation is no longer extracted at all —
	// the already-established completed fact must not disappear.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", nil))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, normalization.PlannedActionStatusCompleted, got[0].Status)
}

// TestPlannedActionRepository_ReplaceForSource_NewRowIDNeverCollidesWithStaleRow
// guards id-collision safety for ReplaceForSource's delete-then-insert
// shape: Normalize assigns every document-scoped entity, PlannedAction
// included, a deterministic id ("plan_<documentID>_<index>", see
// normalization.go's PlannedActions loop), but ReplaceForSource always
// mints its own fresh id for every row it inserts rather than trusting the
// caller's — even across a reprocess where Structured Extraction's output
// for the same document changes between runs (plausible non-determinism,
// see docs/architecture/02-processing-pipeline.md §11), so two calls
// passing the same caller-supplied id for unrelated recommendations must
// never collide.
func TestPlannedActionRepository_ReplaceForSource_NewRowIDNeverCollidesWithStaleRow(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	// First run: index 0 is a glucose recheck, gets Normalize's
	// deterministic id "plan_doc1_0".
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{ID: "plan_doc1_0", UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))

	// Reprocess: the extraction is non-deterministic and this time index 0
	// is a *different* recommendation — Normalize still assigns it the same
	// id "plan_doc1_0" since it's still index 0 of the same document.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{ID: "plan_doc1_0", UserID: "user1", Type: "vaccination", Description: "flu shot", MatchProcedureName: "Грипп"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1, "the old glucose row (still pending) must be replaced by the new one, not left duplicated or erroring")
	require.Equal(t, "flu shot", got[0].Description)
	require.Equal(t, "Грипп", got[0].MatchProcedureName)
}

// TestPlannedActionRepository_ReplaceForSource_SkipsDuplicateAcrossDifferentSources
// reproduces the production bug docs/adr/005-planned-action-cross-source-dedup.md
// fixes: two different documents both recommending "Консультация
// эндокринолога" used to mint two textually-identical pending rows, which
// broke medical.decline_planned_action's LLM matching (it couldn't tell the
// two candidates apart, so it refused to pick either).
func TestPlannedActionRepository_ReplaceForSource_SkipsDuplicateAcrossDifferentSources(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "consultation", Description: "Консультация эндокринолога", MatchProcedureName: "Консультация эндокринолога"},
	}))
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc2", []normalization.PlannedAction{
		{UserID: "user1", Type: "consultation", Description: "Консультация эндокринолога", MatchProcedureName: "Консультация эндокринолога"},
	}))

	got, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1, "the second document's recommendation must not duplicate the first's row")
	require.Equal(t, "doc1", got[0].SourceID, "the surviving row stays owned by whichever source created it first")
}

// TestPlannedActionRepository_ReplaceForSource_DedupSkipsAgainstSelfReportedPending
// covers the other direction the ADR documents as in-scope: a document
// recommending the same thing a self-reported chat message already logged
// must not duplicate it either — ReplaceForSource's cross-source check
// isn't limited to "document" sources.
func TestPlannedActionRepository_ReplaceForSource_DedupSkipsAgainstSelfReportedPending(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	_, err := repo.Add(ctx, normalization.PlannedAction{
		UserID: "user1", SourceType: "self_reported", SourceID: "event1",
		Type: "examination", Description: "ЭКГ", MatchProcedureName: "ЭКГ",
	})
	require.NoError(t, err)

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "examination", Description: "ЭКГ", MatchProcedureName: "ЭКГ"},
	}))

	got, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1, "the document's recommendation must not duplicate the already-logged self-reported one")
	require.Equal(t, "self_reported", got[0].SourceType)
}

// TestPlannedActionRepository_ReplaceForSource_DoesNotDedupAcrossUsers guards
// the household-sharing safety concern the ADR calls out explicitly: two
// different users each having a document recommend the same thing must
// produce two separate rows, one per owner — merging them would let one
// user's medical.decline_planned_action call mutate another user's data.
func TestPlannedActionRepository_ReplaceForSource_DoesNotDedupAcrossUsers(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "examination", Description: "ЭКГ", MatchProcedureName: "ЭКГ"},
	}))
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc2", []normalization.PlannedAction{
		{UserID: "user2", Type: "examination", Description: "ЭКГ", MatchProcedureName: "ЭКГ"},
	}))

	got1, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got1, 1)

	got2, err := repo.ListPending(ctx, "user2")
	require.NoError(t, err)
	require.Len(t, got2, 1)
}

// TestPlannedActionRepository_ReplaceForSource_DoesNotDedupWhenKeyHasNoIdentity
// guards docs/adr/005-planned-action-cross-source-dedup.md §2: two
// recommendations that both failed to extract a canonical
// MatchProcedureName must never be treated as duplicates of each other —
// an empty key carries no identifying information, so "matching" on it
// would be a coincidence, not a real match.
func TestPlannedActionRepository_ReplaceForSource_DoesNotDedupWhenKeyHasNoIdentity(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "other", Description: "Уточнить у врача"},
	}))
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc2", []normalization.PlannedAction{
		{UserID: "user1", Type: "other", Description: "Проверить назначение"},
	}))

	got, err := repo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 2, "two unrelated nameless recommendations must not be collapsed into one")
}
