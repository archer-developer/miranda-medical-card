package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// newTestBackend opens a fresh in-memory Store and a temp-dir filestore,
// returning both so a test can build one or more Pipeline instances on top
// (e.g. reprocessing tests build a second Pipeline with a different
// scripted provider, sharing the same underlying data) and inspect
// persisted rows directly through the same repository types Pipeline uses
// internally.
func newTestBackend(t *testing.T) (*storage.Store, *filestore.Store) {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	fs, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	return s, fs
}

// scriptedLabReportProvider scripts exactly the calls extraction.Extract
// makes for a non-imaging document: one OCR (Chat) call, then Structured
// (Stage 2a) and Structured (Stage 2b instrumental, expectFindings=false so
// exactly one attempt) — see extraction.Extract's doc comment.
func scriptedLabReportProvider(labResultsJSON string) *llmtest.FakeProvider {
	return llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12. Лаборатория Инвитро. Результаты приведены ниже в виде таблицы с референсными значениями для каждого показателя."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(labResultsJSON)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
}

func TestUploadFile_DedupsByUserAndSHA256(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	data := []byte("pdf bytes")
	first, err := p.UploadFile(ctx, "user1", "a.pdf", "application/pdf", data)
	require.NoError(t, err)

	second, err := p.UploadFile(ctx, "user1", "a-renamed-copy.pdf", "application/pdf", data)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "identical bytes for the same user must dedup to the same File")

	third, err := p.UploadFile(ctx, "user2", "a.pdf", "application/pdf", data)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, third.ID, "dedup must not cross users")
}

func TestUploadDocument_HappyPath(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := scriptedLabReportProvider(`{"documentType":"lab_report","organization":"Инвитро","documentDate":"2026-03-12","labResults":[{"name":"АЛТ","value":28.3,"unit":"Ед/л","referenceHigh":41}]}`)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)

	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, result.Status)
	require.Equal(t, 1, result.ExtractedCounts.LabResults)
	require.Contains(t, result.Summary, "Лабораторное исследование")

	doc, err := storage.NewDocumentRepository(s).Get(ctx, result.DocumentID, "user1")
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, doc.Status)
	require.Equal(t, "lab_report", doc.DocumentType)
	require.Equal(t, "Инвитро", doc.Organization)
	require.NotEmpty(t, doc.RecognizedText)
	require.NotNil(t, doc.DocumentDate)

	labResults, err := storage.NewLabResultRepository(s).ListByDocument(ctx, result.DocumentID)
	require.NoError(t, err)
	require.Len(t, labResults, 1)
	require.Equal(t, "АЛТ", labResults[0].IndicatorName)
	require.Equal(t, 28.3, labResults[0].Value)
	require.Equal(t, "Ед/л", labResults[0].NormalizedUnit, "first-ever measurement of this indicator: its own unit becomes canonical")

	active, err := storage.NewExtractionRepository(s).GetActive(ctx, result.DocumentID)
	require.NoError(t, err)
	require.Equal(t, 1, active.Version)

	events, err := storage.NewTimelineRepository(s).List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, events, 1, "the single lab result should have produced one grouped Timeline event")
	require.Equal(t, "lab_result", events[0].Type)

	builtProfile, found, err := profile.NewStore(storage.NewProfileRepository(s)).Get(ctx, "user1")
	require.NoError(t, err)
	require.True(t, found, "MedicalProfile must be rebuilt after a successful upload")
	require.Len(t, builtProfile.LatestLabResults, 1)
	require.Equal(t, "АЛТ", builtProfile.LatestLabResults[0].IndicatorName)

	ftsResults, err := storage.NewFTSRepository(s).SearchDocuments(ctx, "user1", "Лабораторное", 10)
	require.NoError(t, err)
	require.Len(t, ftsResults, 1, "the document must be FTS-indexed")

	embeddings, err := storage.NewEmbeddingRepository(s).ListByUser(ctx, "user1", "fake-model")
	require.NoError(t, err)
	require.Len(t, embeddings, 1, "a summary embedding must be generated")
	require.Equal(t, "summary", embeddings[0].SourceType)
}

func TestUploadDocument_SecondCallSameFileReturnsAlreadyImported(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)

	_, err = p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)

	_, err = p.UploadDocument(ctx, "user1", file.ID)
	require.ErrorIs(t, err, pipeline.ErrAlreadyImported)
}

func TestUploadDocument_ExtractionFailureMarksDocumentFailed(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake", llmtest.Response{Err: errors.New("boom: provider unavailable")})
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)

	_, err = p.UploadDocument(ctx, "user1", file.ID)
	require.Error(t, err)

	docs, err := storage.NewDocumentRepository(s).List(ctx, "user1", storage.DocumentFilter{FileID: file.ID})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, storage.DocumentStatusFailed, docs[0].Status)
}

func TestReprocessDocument_AddsNewExtractionVersionAndReplacesEntities(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	firstProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)
	first := pipeline.New(firstProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := first.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	firstResult, err := first.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)
	require.Equal(t, 1, firstResult.ExtractedCounts.LabResults)

	// Reprocessing re-runs the whole Pipeline against the same File, using
	// a fresh scripted provider — models the LLM producing a different
	// (here, more complete) result on retry, see docs/mcp/03-documents.md
	// §6. A real caller would reuse the same long-lived Pipeline/provider;
	// two Pipeline instances sharing one Store/filestore here is purely a
	// test convenience for scripting two independent LLM runs.
	secondProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3},{"name":"АСТ","value":21.5}]}`)
	second := pipeline.New(secondProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	secondResult, err := second.ReprocessDocument(ctx, "user1", firstResult.DocumentID)
	require.NoError(t, err)
	require.Equal(t, 2, secondResult.ExtractedCounts.LabResults)
	require.Equal(t, firstResult.DocumentID, secondResult.DocumentID)

	versions, err := storage.NewExtractionRepository(s).ListVersions(ctx, firstResult.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.False(t, versions[0].Active, "the old version must be deactivated, not deleted — see docs/mcp/03-documents.md §6")
	require.True(t, versions[1].Active)

	labResults, err := storage.NewLabResultRepository(s).ListByDocument(ctx, firstResult.DocumentID)
	require.NoError(t, err)
	require.Len(t, labResults, 2, "document-scoped replace: the old single result must be gone, not appended to")
}

func TestUploadDocument_SecondDocumentConvertsToFirstDocumentsCanonicalUnit(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	firstProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"Гемоглобин","value":14.4,"unit":"г/дл"}]}`)
	firstPipeline := pipeline.New(firstProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	file1, err := firstPipeline.UploadFile(ctx, "user1", "a.pdf", "application/pdf", []byte("first pdf"))
	require.NoError(t, err)
	_, err = firstPipeline.UploadDocument(ctx, "user1", file1.ID)
	require.NoError(t, err)

	secondProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"Гемоглобин","value":150,"unit":"г/л"}]}`)
	secondPipeline := pipeline.New(secondProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	file2, err := secondPipeline.UploadFile(ctx, "user1", "b.pdf", "application/pdf", []byte("second pdf"))
	require.NoError(t, err)
	secondResult, err := secondPipeline.UploadDocument(ctx, "user1", file2.ID)
	require.NoError(t, err)

	labResults, err := storage.NewLabResultRepository(s).ListByDocument(ctx, secondResult.DocumentID)
	require.NoError(t, err)
	require.Len(t, labResults, 1)
	require.Equal(t, "г/дл", labResults[0].NormalizedUnit, "must convert to the first document's unit, not stay г/л")
	require.InDelta(t, 15.0, labResults[0].NormalizedValue, 0.001)
}
