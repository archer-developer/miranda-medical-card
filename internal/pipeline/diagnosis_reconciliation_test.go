package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/diagnosisreconcile"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// TestDiagnosisReconciliation_RefiningDiagnosisSupersedesOlderOne is the
// ingestion-time counterpart to TestPlannedAction_AutoCompletesFromLaterDocument
// (planned_action_matching_test.go): document A introduces a general chronic
// diagnosis; document B, processed later, introduces a more specific version
// of the same condition. Diagnosis Reconciliation
// (docs/adr/008-diagnosis-cross-document-reconciliation.md) must mark A
// superseded and leave B (and the profile's chronicConditions) reflecting
// only the newer, more informative diagnosis.
func TestDiagnosisReconciliation_RefiningDiagnosisSupersedesOlderOne(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	// 1. Upload document A: a general chronic diagnosis. No other diagnoses
	// exist yet, so reconcileDiagnosesForDocument finds no candidates and
	// never calls the provider for it — only extraction's own two Structured
	// calls, plus Nutrition Guidance's (this diagnosis leaves it non-empty).
	providerA := llmtest.New("fake",
		llmtest.Response{Text: "Жалобы на периодическую боль в горле. Процесс расценен как хронический."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"Хронический тонзиллит","status":"chronic"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		emptyNutritionResponse,
	)
	pipelineA := pipeline.New(providerA, nil, providerA, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	fileA, err := pipelineA.UploadFile(ctx, "user1", "visit1.pdf", "application/pdf", []byte("a"))
	require.NoError(t, err)
	resultA, err := pipelineA.UploadDocument(ctx, "user1", fileA.ID)
	require.NoError(t, err)
	require.Equal(t, 1, resultA.ExtractedCounts.Diagnoses)

	dxRepo := storage.NewDiagnosisRepository(s)
	beforeAll, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, beforeAll, 1)
	require.Equal(t, "chronic", beforeAll[0].Status)
	oldID := beforeAll[0].ID

	// 2. Upload document B: a more specific version of the same diagnosis.
	// Reconciliation now has exactly one candidate (A's diagnosis) — script
	// its id as the confident "refines" match.
	providerB := llmtest.New("fake",
		llmtest.Response{Text: "Осмотр ЛОР-врача. Хронический тонзиллит, вне обострения."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"Хронический тонзиллит, вне обострения","status":"chronic"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(fmt.Sprintf(`{"targetId":%q,"relation":"refines"}`, oldID))},
		emptyNutritionResponse,
	)
	pipelineB := pipeline.New(providerB, nil, providerB, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	fileB, err := pipelineB.UploadFile(ctx, "user1", "visit2.pdf", "application/pdf", []byte("b"))
	require.NoError(t, err)
	_, err = pipelineB.UploadDocument(ctx, "user1", fileB.ID)
	require.NoError(t, err)

	afterAll, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, afterAll, 2)
	var oldDx, newDx normalization.Diagnosis
	for _, d := range afterAll {
		if d.ID == oldID {
			oldDx = d
		} else {
			newDx = d
		}
	}
	require.Equal(t, "superseded", oldDx.Status)
	require.NotEmpty(t, oldDx.StatusReasoning)
	require.Nil(t, oldDx.ActualResolutionAt, "superseding is not a clinical resolution claim")
	require.Equal(t, "chronic", newDx.Status, "the new, refining diagnosis itself must stay untouched")

	// The superseded diagnosis must disappear from the profile Diagnosis
	// Resolver's output (internal/profile.resolveActiveDiagnoses already
	// filters strictly to active/chronic).
	built, err := pipelineB.GetProfile(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, built.ChronicConditions, 1)
	require.Equal(t, "Хронический тонзиллит, вне обострения", built.ChronicConditions[0].Name)
}

// TestDiagnosisReconciliation_CancellingDiagnosisResolvesOlderOne mirrors the
// refines case above for the "cancels" relation: a later document indicates
// an earlier diagnosis was a diagnostic error, so the earlier one is marked
// resolved via the same MarkResolved path medical.resolve_diagnosis uses.
func TestDiagnosisReconciliation_CancellingDiagnosisResolvesOlderOne(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	providerA := llmtest.New("fake",
		llmtest.Response{Text: "Подозрение на воспалительный процесс, требуется дообследование."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"Подозрение на аппендицит","status":"active"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		emptyNutritionResponse,
	)
	pipelineA := pipeline.New(providerA, nil, providerA, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	fileA, err := pipelineA.UploadFile(ctx, "user1", "visit1.pdf", "application/pdf", []byte("a"))
	require.NoError(t, err)
	_, err = pipelineA.UploadDocument(ctx, "user1", fileA.ID)
	require.NoError(t, err)

	dxRepo := storage.NewDiagnosisRepository(s)
	beforeAll, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, beforeAll, 1)
	oldID := beforeAll[0].ID

	providerB := llmtest.New("fake",
		llmtest.Response{Text: "Дообследование исключило хирургическую патологию."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"Аппендицит не подтверждён при дообследовании","status":"resolved"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(fmt.Sprintf(`{"targetId":%q,"relation":"cancels"}`, oldID))},
	)
	pipelineB := pipeline.New(providerB, nil, providerB, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	fileB, err := pipelineB.UploadFile(ctx, "user1", "visit2.pdf", "application/pdf", []byte("b"))
	require.NoError(t, err)
	_, err = pipelineB.UploadDocument(ctx, "user1", fileB.ID)
	require.NoError(t, err)

	afterAll, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	var oldDx normalization.Diagnosis
	for _, d := range afterAll {
		if d.ID == oldID {
			oldDx = d
		}
	}
	require.Equal(t, "resolved", oldDx.Status)
	require.NotNil(t, oldDx.ActualResolutionAt)
	require.NotEmpty(t, oldDx.StatusReasoning)
}

// TestReconcileDiagnosisHistory_DryRunThenApply exercises the one-off
// backfill (medical-dev's reconcile-diagnoses, docs/adr/008): diagnoses
// inserted directly (as if by an old, pre-reconciliation Pipeline run) are
// replayed in chronological order. dx_old has no DiagnosedAt of its own — its
// sort date must fall back to its source document's own DocumentDate — so
// this also covers ReconcileDiagnosisHistory's fallback-ordering path, not
// just the dry-run/apply distinction.
func TestReconcileDiagnosisHistory_DryRunThenApply(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	dxRepo := storage.NewDiagnosisRepository(s)
	docRepo := storage.NewDocumentRepository(s)

	docDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := docRepo.Add(ctx, storage.MedicalDocument{
		ID: "doc1", UserID: "user1", FileID: "file1", Status: storage.DocumentStatusReady,
		DocumentDate: &docDate, UploadedAt: docDate,
	})
	require.NoError(t, err)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_old", UserID: "user1", DocumentID: "doc1",
		Name: "Хронический тонзиллит", Status: "chronic",
	}))

	newDate := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_new", UserID: "user1", DocumentID: "doc2",
		Name: "Хронический тонзиллит, вне обострения", Status: "chronic", DiagnosedAt: &newDate,
	}))

	// 1. Dry run: reports the transition (dx_old is replayed first, thanks
	// to the document-date fallback placing it before dx_new) without
	// mutating storage.
	dryProvider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"targetId":"dx_old","relation":"refines"}`),
	})
	dryPipeline := pipeline.New(dryProvider, nil, dryProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	changes, err := dryPipeline.ReconcileDiagnosisHistory(ctx, "user1", true)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, "dx_new", changes[0].DiagnosisID)
	require.Equal(t, "dx_old", changes[0].TargetID)
	require.Equal(t, "Хронический тонзиллит", changes[0].TargetName)
	require.Equal(t, diagnosisreconcile.RelationRefines, changes[0].Relation)

	stillChronic, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	for _, d := range stillChronic {
		require.Equal(t, "chronic", d.Status, "dry run must not mutate storage")
	}

	// 2. Apply: the identical replay this time persists the transition.
	applyProvider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"targetId":"dx_old","relation":"refines"}`),
	})
	applyPipeline := pipeline.New(applyProvider, nil, applyProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	changes, err = applyPipeline.ReconcileDiagnosisHistory(ctx, "user1", false)
	require.NoError(t, err)
	require.Len(t, changes, 1)

	after, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	for _, d := range after {
		if d.ID == "dx_old" {
			require.Equal(t, "superseded", d.Status)
			require.NotEmpty(t, d.StatusReasoning)
		} else {
			require.Equal(t, "chronic", d.Status)
		}
	}
}

// TestReconcileDiagnosisHistory_NeverComparesSameDocumentSiblings is a
// regression test for a real bug found running this backfill against
// production data: two diagnoses from the very same document/visit (e.g. a
// checkup listing "Дислипидемия" and "Гиперхолестеринемия" side by side)
// share the same sort date and, without this exclusion, would be compared
// against each other purely because of that — the live pipeline hook never
// does this (reconcileDiagnosesForDocument's current/fresh split already
// excludes the new document's own diagnoses from each other), so the
// backfill replay must not either. llmtest.New with no scripted response at
// all means the test fails loudly if decideDiagnosisRelation calls the
// provider — with only a same-document sibling available as a "candidate",
// it must not.
func TestReconcileDiagnosisHistory_NeverComparesSameDocumentSiblings(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	dxRepo := storage.NewDiagnosisRepository(s)

	sameDate := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_a", UserID: "user1", DocumentID: "doc1",
		Name: "Дислипидемия", Status: "active", DiagnosedAt: &sameDate,
	}))
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_b", UserID: "user1", DocumentID: "doc1",
		Name: "Гиперхолестеринемия", Status: "active", DiagnosedAt: &sameDate,
	}))

	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	changes, err := p.ReconcileDiagnosisHistory(ctx, "user1", true)
	require.NoError(t, err)
	require.Empty(t, changes, "same-document diagnoses must never be reconciled against each other")
}
