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

func TestPlannedActionRepository_ReplaceForSource_PreservesCompletedStateOnMatchingKey(t *testing.T) {
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
	require.Len(t, got, 1, "must reconcile onto the same row, not insert a second one")
	require.Equal(t, first[0].ID, got[0].ID, "id must survive the reprocess")
	require.Equal(t, normalization.PlannedActionStatusCompleted, got[0].Status, "completed state must survive an unrelated reprocess")
	require.Equal(t, "doc2", got[0].MatchedDocumentID)
	require.Equal(t, "Контроль глюкозы крови через 6 месяцев", got[0].Description, "description itself should still update")
}

func TestPlannedActionRepository_ReplaceForSource_DeletesStalePendingWhoseKeyDisappeared(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))

	// Reprocess doc1 and this time the recommendation isn't extracted at all.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", nil))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, got, "a still-pending row whose key vanished must be deleted")
}

func TestPlannedActionRepository_ReplaceForSource_NeverDeletesCompletedWhoseKeyDisappeared(t *testing.T) {
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
// guards against a bug /code-review found and this test reproduced directly
// against the real repository: Normalize assigns every document-scoped
// entity, PlannedAction included, a deterministic id
// ("plan_<documentID>_<index>", see normalization.go's PlannedActions
// loop). ReplaceForSource reconciles by (Type, MatchIndicatorName/
// MatchProcedureName), not by id, and only deletes stale rows *after* the
// insert/update loop — so if a reprocess's new item at some index gets a
// different key than the old item that occupied that same index (plausible
// Structured Extraction non-determinism, see
// docs/architecture/02-processing-pipeline.md §11), the insert branch used
// to reuse that deterministic id, colliding with the still-present old
// row's primary key and failing the whole INSERT (and, through
// matchPlannedActions, the whole pipeline.run()). Fixed by always minting a
// fresh id for a newly-inserted row instead of trusting the caller's id.
func TestPlannedActionRepository_ReplaceForSource_NewRowIDNeverCollidesWithStaleRow(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewPlannedActionRepository(newTestStore(t))

	// First run: index 0 is a glucose recheck, gets Normalize's
	// deterministic id "plan_doc1_0".
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{ID: "plan_doc1_0", UserID: "user1", Type: "lab_test", Description: "glucose", MatchIndicatorName: "Глюкоза"},
	}))

	// Reprocess: the extraction is non-deterministic and this time index 0
	// is a *different* recommendation (different key) — Normalize still
	// assigns it the same id "plan_doc1_0" since it's still index 0 of the
	// same document.
	require.NoError(t, repo.ReplaceForSource(ctx, "document", "doc1", []normalization.PlannedAction{
		{ID: "plan_doc1_0", UserID: "user1", Type: "vaccination", Description: "flu shot", MatchProcedureName: "Грипп"},
	}))

	got, err := repo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, got, 1, "the old glucose row must be replaced by the new one, not left duplicated or erroring")
	require.Equal(t, "flu shot", got[0].Description)
	require.Equal(t, "Грипп", got[0].MatchProcedureName)
}
