package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// scriptedConsultationWithPlannedAction scripts a consultation document
// whose recommendation ("Повторный анализ глюкозы через полгода") is
// structured into a single plannedActions entry, following the same
// two-Structured-call shape scriptedLabReportProvider uses.
func scriptedConsultationWithPlannedAction() *llmtest.FakeProvider {
	return llmtest.New("fake",
		llmtest.Response{Text: "Жалобы на общее недомогание. Рекомендован повторный анализ глюкозы крови через полгода."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{
			"documentType": "consultation",
			"recommendations": ["Повторный анализ глюкозы крови через полгода"],
			"plannedActions": [{
				"type": "lab_test",
				"description": "Повторный анализ глюкозы крови",
				"referenceText": "Повторный анализ глюкозы крови через полгода",
				"relatedIndicatorName": "Глюкоза",
				"dueAmountMax": 6,
				"dueUnit": "month"
			}]
		}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
}

func TestPlannedAction_AutoCompletesFromLaterDocument_AndRevertsOnReprocessWithoutMatch(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	// 1. Upload document A: a consultation recommending a glucose recheck.
	providerA := scriptedConsultationWithPlannedAction()
	pipelineA := pipeline.New(providerA, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	fileA, err := pipelineA.UploadFile(ctx, "user1", "consult.pdf", "application/pdf", []byte("a"))
	require.NoError(t, err)
	resultA, err := pipelineA.UploadDocument(ctx, "user1", fileA.ID)
	require.NoError(t, err)
	require.Equal(t, 1, resultA.ExtractedCounts.PlannedActions)

	planRepo := storage.NewPlannedActionRepository(s)
	pending, err := planRepo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, normalization.PlannedActionSourceDocument, pending[0].SourceType)
	require.Equal(t, resultA.DocumentID, pending[0].SourceID)
	planID := pending[0].ID

	// 2. Upload document B: a lab report with a matching glucose result —
	// the pending action must auto-complete with a backlink to it.
	providerB := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"Глюкоза","value":5.2,"unit":"ммоль/л"}]}`)
	pipelineB := pipeline.New(providerB, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	fileB, err := pipelineB.UploadFile(ctx, "user1", "glucose.pdf", "application/pdf", []byte("b"))
	require.NoError(t, err)
	resultB, err := pipelineB.UploadDocument(ctx, "user1", fileB.ID)
	require.NoError(t, err)

	all, err := planRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, normalization.PlannedActionStatusCompleted, all[0].Status)
	require.Equal(t, resultB.DocumentID, all[0].MatchedDocumentID)
	require.NotEmpty(t, all[0].MatchedEntityID)
	require.NotNil(t, all[0].MatchedAt)

	pendingAfter, err := planRepo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, pendingAfter)

	// 3. Reprocess document B with a script that no longer contains a
	// glucose result — the completion must revert to pending rather than
	// staying permanently "completed" by data that no longer exists.
	providerB2 := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"Холестерин","value":4.1,"unit":"ммоль/л"}]}`)
	pipelineB2 := pipeline.New(providerB2, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	_, err = pipelineB2.ReprocessDocument(ctx, "user1", resultB.DocumentID)
	require.NoError(t, err)

	reverted, err := planRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, reverted, 1)
	require.Equal(t, planID, reverted[0].ID)
	require.Equal(t, normalization.PlannedActionStatusPending, reverted[0].Status, "reprocessing the matching document without a matching result must revert the completion")
	require.Empty(t, reverted[0].MatchedDocumentID)
}

func TestPlannedAction_CompletedStateSurvivesReprocessOfItsOwnSourceDocument(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	// Document A creates the planned action.
	providerA := scriptedConsultationWithPlannedAction()
	pipelineA := pipeline.New(providerA, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	fileA, err := pipelineA.UploadFile(ctx, "user1", "consult.pdf", "application/pdf", []byte("a"))
	require.NoError(t, err)
	resultA, err := pipelineA.UploadDocument(ctx, "user1", fileA.ID)
	require.NoError(t, err)

	planRepo := storage.NewPlannedActionRepository(s)
	pending, err := planRepo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	planID := pending[0].ID

	// Document B completes it.
	providerB := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"Глюкоза","value":5.2,"unit":"ммоль/л"}]}`)
	pipelineB := pipeline.New(providerB, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	fileB, err := pipelineB.UploadFile(ctx, "user1", "glucose.pdf", "application/pdf", []byte("b"))
	require.NoError(t, err)
	resultB, err := pipelineB.UploadDocument(ctx, "user1", fileB.ID)
	require.NoError(t, err)

	completed, err := planRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, normalization.PlannedActionStatusCompleted, completed[0].Status)

	// Reprocess A itself — same match key (relatedIndicatorName still
	// resolves to "Глюкоза"), just reworded — must not lose the
	// already-established completion.
	providerA2 := llmtest.New("fake",
		llmtest.Response{Text: "Жалобы на общее недомогание. Рекомендуется контроль глюкозы крови в динамике через 6 месяцев."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{
			"documentType": "consultation",
			"plannedActions": [{
				"type": "lab_test",
				"description": "Контроль глюкозы крови в динамике",
				"relatedIndicatorName": "Глюкоза",
				"dueAmountMax": 6,
				"dueUnit": "month"
			}]
		}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	pipelineA2 := pipeline.New(providerA2, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	_, err = pipelineA2.ReprocessDocument(ctx, "user1", resultA.DocumentID)
	require.NoError(t, err)

	afterReprocess, err := planRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, afterReprocess, 1, "must reconcile onto the same row, not duplicate")
	require.Equal(t, planID, afterReprocess[0].ID)
	require.Equal(t, normalization.PlannedActionStatusCompleted, afterReprocess[0].Status, "completion must survive an unrelated reprocess of its own source document")
	require.Equal(t, resultB.DocumentID, afterReprocess[0].MatchedDocumentID)
	require.Equal(t, "Контроль глюкозы крови в динамике", afterReprocess[0].Description, "description should still update to the reworded text")
}
