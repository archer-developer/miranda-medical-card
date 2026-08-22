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

// TestUploadDocument_LabReportRawTextNotFTSIndexed covers
// pipeline.documentTypesWithoutFreeTextContent: traced back to a real
// medical.ask failure where a lab report's raw OCR text — boilerplate
// reference-range citations, not doctor's advice — false-positive matched
// an FTS search for "рекомендации" and sent the agent chasing a document
// that had nothing relevant in it. lab_report's entire expected content is
// already structured (LabResult rows), so its raw FullText shouldn't be
// FTS-searchable at all; Summary must still be, regardless.
func TestUploadDocument_LabReportRawTextNotFTSIndexed(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Референсные значения по [рекомендации] ВОЗ, 2011 см.комм БОЙЛЕРПЛЕЙТСЛОВО."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, result.Status)

	fts := storage.NewFTSRepository(s)

	byRawText, err := fts.SearchDocuments(ctx, "user1", "БОЙЛЕРПЛЕЙТСЛОВО", 10)
	require.NoError(t, err)
	require.Empty(t, byRawText, "a lab report's raw OCR text must not be FTS-searchable")

	bySummary, err := fts.SearchDocuments(ctx, "user1", "Лабораторное", 10)
	require.NoError(t, err)
	require.Len(t, bySummary, 1, "Summary must stay FTS-indexed regardless of document type")
}

// TestUploadDocument_ConsultationRawTextStaysFTSIndexed is the converse of
// TestUploadDocument_LabReportRawTextNotFTSIndexed: a document type not in
// documentTypesWithoutFreeTextContent (consultation notes carry genuine
// free-text doctor's remarks beyond what's structured) must keep its raw
// OCR text searchable.
func TestUploadDocument_ConsultationRawTextStaysFTSIndexed(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Жалобы на бессонницу и раздражительность в течение месяца."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"Инсомния"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "note.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, result.Status)

	fts := storage.NewFTSRepository(s)
	byRawText, err := fts.SearchDocuments(ctx, "user1", "раздражительность", 10)
	require.NoError(t, err)
	require.Len(t, byRawText, 1, "a consultation's raw OCR text (genuine free-text remarks) must stay FTS-searchable")
}

// TestUploadDocument_StudyTitleFromExtractionBecomesDocumentTitle covers
// extraction.Schema's studyTitle field (added because the fixed
// documentType label alone — "Лабораторное исследование" for every lab
// report, "Инструментальное исследование" for every imaging study — gives
// a user nothing to reference a specific document by in a medcard
// question). When Extraction transcribes a printed title, buildTitle must
// prefer it over the generic label.
func TestUploadDocument_StudyTitleFromExtractionBecomesDocumentTitle(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := scriptedLabReportProvider(`{"documentType":"lab_report","organization":"Хеликс","studyTitle":"Общий анализ мочи","labResults":[{"name":"Белок","qualitativeValue":"не обнаружен"}]}`)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "urine.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)

	doc, err := storage.NewDocumentRepository(s).Get(ctx, result.DocumentID, "user1")
	require.NoError(t, err)
	require.Equal(t, "Общий анализ мочи — Хеликс", doc.Title)
}

// TestUploadDocument_MissingStudyTitleFallsBackToDocumentTypeLabel confirms
// the pre-existing behavior is unchanged when Extraction has nothing to
// transcribe for studyTitle (schema field omitted, e.g. no clear printed
// heading) — same "don't fabricate" fallback Organization/Doctor already
// have, not a regression.
func TestUploadDocument_MissingStudyTitleFallsBackToDocumentTypeLabel(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := scriptedLabReportProvider(`{"documentType":"lab_report","organization":"Инвитро","labResults":[{"name":"АЛТ","value":28.3}]}`)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)

	doc, err := storage.NewDocumentRepository(s).Get(ctx, result.DocumentID, "user1")
	require.NoError(t, err)
	require.Equal(t, "Лабораторное исследование — Инвитро", doc.Title)
}

// TestBackfillStudyTitle_UpdatesTitleFromReplayedStructuredCall covers the
// migration path for documents uploaded before extraction.Schema had
// studyTitle (see pipeline.Pipeline.BackfillStudyTitle's doc comment):
// re-running only Stage 2a against the already-stored RecognizedText (no
// OCR, no new Extraction version) must pick up a studyTitle that wasn't
// available the first time and update Title, without touching
// DocumentType/Organization/RecognizedText/Summary.
func TestBackfillStudyTitle_UpdatesTitleFromReplayedStructuredCall(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12. Лаборатория Инвитро."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","organization":"Инвитро","labResults":[{"name":"АЛТ","value":28.3}]}`)},                                   // upload, stage 2a — no studyTitle yet
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},                                                                                                         // upload, stage 2b
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","organization":"Инвитро","studyTitle":"Общий анализ крови","labResults":[{"name":"АЛТ","value":28.3}]}`)}, // backfill replay
	)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)

	before, err := storage.NewDocumentRepository(s).Get(ctx, result.DocumentID, "user1")
	require.NoError(t, err)
	require.Equal(t, "Лабораторное исследование — Инвитро", before.Title, "sanity check: no studyTitle on first extraction")

	changed, newTitle, err := p.BackfillStudyTitle(ctx, "user1", result.DocumentID)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Общий анализ крови — Инвитро", newTitle)

	after, err := storage.NewDocumentRepository(s).Get(ctx, result.DocumentID, "user1")
	require.NoError(t, err)
	require.Equal(t, "Общий анализ крови — Инвитро", after.Title)
	require.Equal(t, before.DocumentType, after.DocumentType)
	require.Equal(t, before.Organization, after.Organization)
	require.Equal(t, before.RecognizedText, after.RecognizedText)
	require.Equal(t, before.Summary, after.Summary, "backfill must only touch Title")
}

// TestBackfillStudyTitle_NoStudyTitleInReplayIsNotAnError covers the
// "nothing to backfill" outcome — the replayed call still doesn't produce a
// studyTitle (e.g. a genuinely untitled document) — which must report
// changed=false, not an error.
func TestBackfillStudyTitle_NoStudyTitleInReplayIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)}, // backfill replay, still no studyTitle
	)
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	result, err := p.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)

	changed, newTitle, err := p.BackfillStudyTitle(ctx, "user1", result.DocumentID)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, newTitle)
}

// TestReindexDocumentFTS_DropsLabReportRawTextButKeepsSummary simulates a
// document imported before documentTypesWithoutFreeTextContent existed
// (pipeline.go) — its FTS entry still has raw OCR boilerplate indexed —
// and checks ReindexDocumentFTS brings it in line purely from already-
// persisted data, no LLM call involved.
func TestReindexDocumentFTS_DropsLabReportRawTextButKeepsSummary(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	docs := storage.NewDocumentRepository(s)
	fts := storage.NewFTSRepository(s)

	doc, err := docs.Add(ctx, storage.MedicalDocument{
		ID: "doc1", UserID: "user1", Status: storage.DocumentStatusReady, DocumentType: "lab_report",
		Title:          "Лабораторное исследование",
		RecognizedText: "Референсные значения по [рекомендации] ВОЗ, 2011 см.комм БОЙЛЕРПЛЕЙТСЛОВО.",
		Summary:        "Лабораторное исследование.",
	})
	require.NoError(t, err)
	// Simulate the pre-fix indexed content: raw text + Summary, as run()
	// built it before documentTypesWithoutFreeTextContent existed.
	require.NoError(t, fts.IndexDocument(ctx, "user1", doc.ID, doc.Title, doc.RecognizedText+"\n"+doc.Summary))

	byRawText, err := fts.SearchDocuments(ctx, "user1", "БОЙЛЕРПЛЕЙТСЛОВО", 10)
	require.NoError(t, err)
	require.Len(t, byRawText, 1, "pre-fix state: the boilerplate must still be indexed")

	p := pipeline.New(llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)
	require.NoError(t, p.ReindexDocumentFTS(ctx, "user1", doc.ID))

	byRawText, err = fts.SearchDocuments(ctx, "user1", "БОЙЛЕРПЛЕЙТСЛОВО", 10)
	require.NoError(t, err)
	require.Empty(t, byRawText, "after reindexing, the raw OCR boilerplate must no longer be indexed")

	bySummary, err := fts.SearchDocuments(ctx, "user1", "Лабораторное", 10)
	require.NoError(t, err)
	require.Len(t, bySummary, 1, "Summary must still be indexed after reindexing")
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

// TestUploadDocument_RetryAfterFailureReusesSameDocumentAndCanSucceed covers
// the bug found from a real production failure (2024-02-06.pdf's OCR
// reliably truncating, see extraction.Extract's OCR-retry-against-escalation
// fix): UploadDocument's own "already imported" check filtered existing
// documents by fileID alone, ignoring status — so a FAILED document (a
// previous attempt that genuinely broke, not a successful import) blocked
// every subsequent retry with the same misleading DOCUMENT_ALREADY_IMPORTED,
// forever, with no way to actually retry short of separately discovering the
// document id and calling medical.reprocess_document. A retry must instead
// go through, reusing the same (now FAILED) document row — not creating an
// orphan duplicate — and succeed if the underlying problem is gone.
func TestUploadDocument_RetryAfterFailureReusesSameDocumentAndCanSucceed(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	failingProvider := llmtest.New("fake", llmtest.Response{Err: errors.New("boom: provider unavailable")})
	p := pipeline.New(failingProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)

	_, err = p.UploadDocument(ctx, "user1", file.ID)
	require.Error(t, err)
	require.NotErrorIs(t, err, pipeline.ErrAlreadyImported, "the first attempt is a genuine failure, not a duplicate")

	docs, err := storage.NewDocumentRepository(s).List(ctx, "user1", storage.DocumentFilter{FileID: file.ID})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	firstDocID := docs[0].ID
	require.Equal(t, storage.DocumentStatusFailed, docs[0].Status)

	workingProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)
	p2 := pipeline.New(workingProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	result, err := p2.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err, "retrying the same file after a FAILED attempt must not bounce off ErrAlreadyImported")
	require.Equal(t, firstDocID, result.DocumentID, "the retry must reuse the same document row, not create a new one")
	require.Equal(t, storage.DocumentStatusReady, result.Status)

	allDocs, err := storage.NewDocumentRepository(s).List(ctx, "user1", storage.DocumentFilter{FileID: file.ID})
	require.NoError(t, err)
	require.Len(t, allDocs, 1, "must not leave an orphan duplicate document row behind")
}

// TestUploadDocument_SuspiciouslyEmptyStructuredResultMarksDocumentFailed
// covers the failure mode found on doc_3bdc49d9-7064-48d7-bc5b-9c2b0db38c05
// in production (2026-08-09): OCR succeeded (substantial fullText) but
// Structured Extraction returned labResults:[] on every attempt for a
// lab_report — extraction.StructuredWithRetry itself never turns this into
// an error (see its doc comment), so before this fix the document was
// silently marked READY with zero entities, indistinguishable from a
// legitimately empty document. Pipeline must now mark it FAILED instead —
// see internal/pipeline/pipeline.go's stillSuspicious handling in run.
func TestUploadDocument_SuspiciouslyEmptyStructuredResultMarksDocumentFailed(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	longFullText := "Общий анализ крови. Дата: 2026-03-12. Лаборатория Инвитро. " +
		"Результаты приведены ниже в виде таблицы с референсными значениями для каждого показателя. " +
		"Данный текст намеренно длиннее порога minFullTextForSuspicion, чтобы имитировать содержательный документ, " +
		"из которого структурированное извлечение не смогло получить ни одной записи ни за одну из попыток."
	require.Greater(t, len(longFullText), 300, "test fixture must exceed minFullTextForSuspicion or isSuspiciouslyEmpty never fires")

	emptyLabReport := `{"documentType":"lab_report","labResults":[]}`
	provider := llmtest.New("fake", llmtest.Response{Text: longFullText}).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(emptyLabReport)}, // primary attempt 1
		llmtest.StructuredResponse{JSON: json.RawMessage(emptyLabReport)}, // primary attempt 2 (maxStructuredRetries+1)
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	// No escalation provider configured — the primary's own attempts are
	// exhausted and still suspicious, same as the production case (where
	// escalation was configured but its own call failed — see
	// extraction.StructuredWithRetry's doc comment: an unreachable
	// escalation provider falls back to the primary's suspicious result
	// rather than hard-failing, so the end state is the same either way).
	p := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := p.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)

	_, err = p.UploadDocument(ctx, "user1", file.ID)
	require.Error(t, err, "a suspiciously-empty structured result must surface as a pipeline error, not a silent success")

	docs, err := storage.NewDocumentRepository(s).List(ctx, "user1", storage.DocumentFilter{FileID: file.ID})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, storage.DocumentStatusFailed, docs[0].Status, "must not be marked READY when nothing usable was extracted")

	labResults, err := storage.NewLabResultRepository(s).ListByDocument(ctx, docs[0].ID)
	require.NoError(t, err)
	require.Empty(t, labResults)
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

// TestReextractDocument_SkipsOCRReusesStoredRecognizedTextAndAddsNewVersion
// mirrors TestReprocessDocument_AddsNewExtractionVersionAndReplacesEntities
// but through ReextractDocument: the second provider is scripted with only
// Structured (Stage 2a/2b) responses, no Chat (OCR) response at all — if
// ReextractDocument tried to OCR again instead of reusing
// MedicalDocument.RecognizedText, llmtest would panic on an unscripted Chat
// call, so a passing test is itself proof OCR was skipped.
func TestReextractDocument_SkipsOCRReusesStoredRecognizedTextAndAddsNewVersion(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)

	firstProvider := scriptedLabReportProvider(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)
	first := pipeline.New(firstProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	file, err := first.UploadFile(ctx, "user1", "cbc.pdf", "application/pdf", []byte("pdf bytes"))
	require.NoError(t, err)
	firstResult, err := first.UploadDocument(ctx, "user1", file.ID)
	require.NoError(t, err)
	require.Equal(t, 1, firstResult.ExtractedCounts.LabResults)

	secondProvider := llmtest.New("fake").WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3},{"name":"АСТ","value":21.5}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	second := pipeline.New(secondProvider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	secondResult, err := second.ReextractDocument(ctx, "user1", firstResult.DocumentID)
	require.NoError(t, err)
	require.Equal(t, 2, secondResult.ExtractedCounts.LabResults)
	require.Equal(t, firstResult.DocumentID, secondResult.DocumentID)

	versions, err := storage.NewExtractionRepository(s).ListVersions(ctx, firstResult.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2, "ReextractDocument must add a new Extraction version, same as ReprocessDocument")
}

// TestReextractDocument_NoStoredRecognizedTextFails covers a document that
// was never through a full run (e.g. a row created directly, or one that
// predates ReextractDocument) — ReprocessDocument, not ReextractDocument, is
// the only way to populate RecognizedText for the first time.
func TestReextractDocument_NoStoredRecognizedTextFails(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	doc, err := storage.NewDocumentRepository(s).Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file1"})
	require.NoError(t, err)

	_, err = p.ReextractDocument(ctx, "user1", doc.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, storage.ErrNotFound))
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
