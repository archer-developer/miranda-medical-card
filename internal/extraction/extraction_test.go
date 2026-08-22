package extraction_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/gemini"
	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/planning"
)

// These are regression tests against the real Gemini API, using the
// anonymized fixtures under testdata/ (see testdata's own note below). They
// are NOT run by a plain `go test ./...` — they cost real LLM quota and are
// inherently non-deterministic (that's the exact property they're guarding
// against regressing further, see docs/architecture/02-processing-pipeline.md
// §5) — and require GEMINI_API_KEY_1 (or more) in the environment. Run
// explicitly when validating a prompt/schema change:
//
//	GEMINI_API_KEY_1=... go test ./internal/extraction/... -run TestFixtures -v
//
// Fixture provenance: testdata/*.txt is real OCR output from real documents
// collected during development, with every identifying detail (patient
// name, DOB, doctor names, clinic names, phone/address, order numbers)
// replaced by obviously-fake placeholders before being committed — see each
// file's content. testdata/*_expected.json is the corresponding
// Structured/InstrumentalStructured output, hand-verified against the
// original document at the time it was collected.
func skipUnlessLive(t *testing.T) {
	t.Helper()
	if os.Getenv("GEMINI_API_KEY_1") == "" {
		t.Skip("GEMINI_API_KEY_1 not set — skipping live extraction regression test (see this file's doc comment)")
	}
}

func testProvider(t *testing.T) *gemini.Provider {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p, err := gemini.New(context.Background(), "gemini-test", "gemini-3.6-flash",
		[]string{"GEMINI_API_KEY_1", "GEMINI_API_KEY_2", "GEMINI_API_KEY_3"},
		gemini.ToolsConfig{},
		gemini.RotationConfig{CooldownSeconds: 10, MaxRetryCycles: 1},
		logger,
	)
	require.NoError(t, err)
	return p
}

func loadText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(data)
}

func loadExpected(t *testing.T, name string) extraction.Result {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var result extraction.Result
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

// assertCategoryCountsMatch is the primary regression signal: every
// category's count must match the hand-verified fixture exactly. This is
// what would have caught the "all categories silently empty" and "one
// category dropped" failures found during development — see
// docs/architecture/02-processing-pipeline.md §5.
func assertCategoryCountsMatch(t *testing.T, expected, got extraction.Result) {
	t.Helper()
	require.Equal(t, expected.DocumentType, got.DocumentType, "documentType")
	require.Len(t, got.Diagnoses, len(expected.Diagnoses), "diagnoses count")
	require.Len(t, got.Medications, len(expected.Medications), "medications count")
	require.Len(t, got.LabResults, len(expected.LabResults), "labResults count")
	require.Len(t, got.Procedures, len(expected.Procedures), "procedures count")
	require.Len(t, got.Allergies, len(expected.Allergies), "allergies count")
	require.Len(t, got.VitalSigns, len(expected.VitalSigns), "vitalSigns count")
	require.Len(t, got.Recommendations, len(expected.Recommendations), "recommendations count")
}

// assertLabResultValuesMatch checks the actual name->value mapping, not
// just the count — labResults is the category most directly consumed by
// Lab Search (see docs/architecture/04-search.md §7), so a value silently
// changing (not just a count) is the failure mode most worth catching
// precisely here. Order-independent: the model isn't expected to preserve
// row order.
func assertLabResultValuesMatch(t *testing.T, expected, got extraction.Result) {
	t.Helper()
	gotByName := make(map[string]extraction.LabResult, len(got.LabResults))
	for _, r := range got.LabResults {
		gotByName[r.Name] = r
	}
	for _, want := range expected.LabResults {
		got, ok := gotByName[want.Name]
		if !ok {
			t.Errorf("labResult %q missing from result", want.Name)
			continue
		}
		require.Equal(t, want.Value, got.Value, "labResult %q value", want.Name)
		require.Equal(t, want.Unit, got.Unit, "labResult %q unit", want.Name)
	}
}

// assertPlannedActionsMatch checks docs/adr/004-planned-actions.md's core
// extraction contract: plannedActions is a distinct category from
// recommendations (checked by name, not just count, unlike
// assertCategoryCountsMatch's other fields), each expected item's type must
// appear, and — the actual regression signal — whether a due date was
// extracted must match expectation exactly. An item the fixture expects
// dated must come back with the same dueAmountMax/dueUnit (proving the
// model extracted a relative offset, not a fabricated calendar date); an
// item the fixture expects undated must come back with no dueAmountMax at
// all (proving a recommendation with no stated timeframe is still captured
// as a valid plannedAction — see TestFixtures_ThyroidUltrasoundUndatedRecommendation_PlannedActions's
// own doc comment for the regression this specifically guards against).
func assertPlannedActionsMatch(t *testing.T, expected, got extraction.Result) {
	t.Helper()
	require.Len(t, got.PlannedActions, len(expected.PlannedActions), "plannedActions count")
	for _, want := range expected.PlannedActions {
		var found *planning.PlannedAction
		for i := range got.PlannedActions {
			if got.PlannedActions[i].Type == want.Type {
				found = &got.PlannedActions[i]
				break
			}
		}
		if !assert.NotNil(t, found, "plannedAction type %q missing from result", want.Type) {
			continue
		}
		if want.DueAmountMax > 0 {
			require.Equal(t, want.DueAmountMax, found.DueAmountMax, "plannedAction %q dueAmountMax", want.Type)
			require.Equal(t, want.DueUnit, found.DueUnit, "plannedAction %q dueUnit", want.Type)
		} else {
			require.Zero(t, found.DueAmountMax, "plannedAction %q must have no due date", want.Type)
		}
	}
}

func TestFixtures_InvitroCBC(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "invitro_cbc.txt")
	expected := loadExpected(t, "invitro_cbc_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
	assertLabResultValuesMatch(t, expected, got)
}

func TestFixtures_HelixBiochemLipidCBC(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "helix_biochem_lipid_cbc.txt")
	expected := loadExpected(t, "helix_biochem_lipid_cbc_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
	assertLabResultValuesMatch(t, expected, got)
}

// TestFixtures_LodeConsultation_CombinedSides is also the regression test
// for the "front+back of one physical page must be extracted as one
// document" behavior — the fixture text already concatenates both sides
// (see testdata/lode_consultation.txt), and the expected diagnoses count
// (9, matching only the front side's actual diagnoses) guards against a
// regression where a section heading on the back ("План диспансеризации по
// АГ") gets misread as introducing a new, tenth diagnosis.
func TestFixtures_LodeConsultation_CombinedSides(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "lode_consultation.txt")
	expected := loadExpected(t, "lode_consultation_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
}

func TestFixtures_GravitaUltrasound_Clinical(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "gravita_ultrasound.txt")
	expected := loadExpected(t, "gravita_ultrasound_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
}

func TestFixtures_GravitaUltrasound_Instrumental(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "gravita_ultrasound.txt")

	data, err := os.ReadFile(filepath.Join("testdata", "gravita_ultrasound_instrumental_expected.json"))
	require.NoError(t, err)
	var expected struct {
		InstrumentalFindings []extraction.InstrumentalFinding `json:"instrumentalFindings"`
	}
	require.NoError(t, json.Unmarshal(data, &expected))

	// expectFindings=true unconditionally here: this fixture is already
	// known to be an imaging report, unlike Extract's own call site which
	// has to derive that from Stage 2a's own output first.
	got, _, err := extraction.InstrumentalStructuredWithRetry(context.Background(), provider, nil, text, true, nil)
	require.NoError(t, err)

	require.Len(t, got, len(expected.InstrumentalFindings), "instrumentalFindings count")
}

// TestFixtures_ThyroidUltrasoundDatedRecommendation_PlannedActions is the
// e2e regression test for docs/adr/004-planned-actions.md's "Диапазон дат,
// а не точечная дата" decision: the fixture's one recommendation with an
// explicit relative timeframe ("УЗ-контроль в динамике (через 12 месяцев)")
// must come back as a plannedActions entry with dueAmountMax=12/dueUnit=month
// — a relative offset, never a calendar date the model computed itself —
// while the other two recommendations in the same document (no stated
// timeframe) still become plannedActions, just undated ones.
func TestFixtures_ThyroidUltrasoundDatedRecommendation_PlannedActions(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "thyroid_ultrasound_dated_recommendation.txt")
	expected := loadExpected(t, "thyroid_ultrasound_dated_recommendation_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
	assertPlannedActionsMatch(t, expected, got)
}

// TestFixtures_ThyroidUltrasoundUndatedRecommendation_PlannedActions is the
// regression test for the extraction prompt bug fixed alongside this test
// (internal/extraction.Prompt used to say "A recommendation with no
// extractable timeframe stays in recommendations only, not plannedActions"
// — directly contradicting docs/adr/004-planned-actions.md's own "сдать
// кровь на глюкозу" example and docs/domain/14-planned-action.md §5's "Пункт
// без dueDateFrom/dueDateTo... всегда присутствует в выдаче
// medical.planned_actions"). This fixture's recommendations state no
// timeframe at all ("Анализ крови на гормоны щитовидной железы.
// Консультация эндокринолога. Контроль УЗИ.") — before the fix this
// produced zero plannedActions; it must now produce one per recommendation,
// each with no due date.
func TestFixtures_ThyroidUltrasoundUndatedRecommendation_PlannedActions(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "thyroid_ultrasound_undated_recommendation.txt")
	expected := loadExpected(t, "thyroid_ultrasound_undated_recommendation_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
	assertPlannedActionsMatch(t, expected, got)
}

// TestStructuredWithRetry_TypeAwareRetryDoesNotStopOnUnrelatedCategory
// replays, against a scripted fake provider (no live API needed), the exact
// production failure this test guards against: a lab_report whose 1st
// attempt filled in "recommendations" (a near-universal field present on
// almost any document) while labResults — the one category that actually
// defines a lab_report — stayed empty. Before isSuspiciouslyEmpty became
// type-aware (see its doc comment, docs/architecture/02-processing-pipeline.md
// §5), a non-empty recommendations alone satisfied the old "every category
// empty" check and stopped the retry loop right there, silently losing the
// document's lab results. It must now keep retrying until either labResults
// is populated or attempts run out (maxStructuredRetries+1 = 2 attempts
// against the primary provider — see that constant's doc comment for why
// it's kept low, escalation being the intended next step, not a 3rd
// primary attempt).
func TestStructuredWithRetry_TypeAwareRetryDoesNotStopOnUnrelatedCategory(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20) // > minFullTextForSuspicion

	provider := llmtest.New("fake").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType:    "lab_report",
			Recommendations: []string{"Результаты не являются диагнозом"},
		})},
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType: "lab_report",
			LabResults:   []extraction.LabResult{{Name: "ALT", Value: 28.3}},
		})},
	)

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)
	require.Len(t, got.LabResults, 1, "must keep retrying past a recommendations-only attempt until labResults is populated")
}

// TestStructuredWithRetry_EscalatesWhenPrimaryStaysSuspicious covers the
// escalation path itself (see StructuredWithRetry's escalate parameter):
// once the primary provider's own attempts are exhausted and still
// suspiciously empty, one attempt against a different provider should be
// tried — and used, if it isn't suspicious either.
func TestStructuredWithRetry_EscalatesWhenPrimaryStaysSuspicious(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20)

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "lab_report"})},
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "lab_report"})},
	)
	escalation := llmtest.New("fake-escalation").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType: "lab_report",
			LabResults:   []extraction.LabResult{{Name: "ALT", Value: 28.3}},
		})},
	)

	got, _, err := extraction.StructuredWithRetry(context.Background(), primary, escalation, text, nil)
	require.NoError(t, err)
	require.Len(t, got.LabResults, 1, "escalation provider's non-suspicious result must be used once the primary is exhausted")
}

// TestStructuredWithRetry_EscalationProviderFailureFallsBackToPrimary
// covers escalate hard-erroring (e.g. its own key/quota exhausted) —
// StructuredWithRetry must not turn that into a hard failure for the whole
// call; it should fall back to the primary provider's (still suspicious)
// result instead, since an unreachable escalation provider shouldn't make
// an otherwise-successful extraction fail outright.
func TestStructuredWithRetry_EscalationProviderFailureFallsBackToPrimary(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20)

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "lab_report"})},
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "lab_report"})},
	)
	escalation := llmtest.New("fake-escalation").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("escalation provider unavailable")},
	)

	got, _, err := extraction.StructuredWithRetry(context.Background(), primary, escalation, text, nil)
	require.NoError(t, err, "an escalation provider failure must not fail the whole call")
	require.Empty(t, got.LabResults)
}

// TestStructuredWithRetry_EscalatesOnPrimaryHardError covers the gap found
// in production (2026-08-09): a hard error from the primary (e.g. every
// configured API key already 429'd) used to return immediately without
// ever trying escalate — escalation only fired for a "successful but
// suspiciously empty" result, never a genuine failure. A hard error now
// triggers one escalation attempt too.
func TestStructuredWithRetry_EscalatesOnPrimaryHardError(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20)

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("boom: 429 quota exceeded")},
	)
	escalation := llmtest.New("fake-escalation").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType: "lab_report",
			LabResults:   []extraction.LabResult{{Name: "ALT", Value: 28.3}},
		})},
	)

	got, _, err := extraction.StructuredWithRetry(context.Background(), primary, escalation, text, nil)
	require.NoError(t, err)
	require.Len(t, got.LabResults, 1)
}

// TestStructuredWithRetry_PrimaryHardErrorNoEscalationConfiguredFails
// confirms nil escalate keeps today's plain "primary fails, call fails"
// behavior — not a regression.
func TestStructuredWithRetry_PrimaryHardErrorNoEscalationConfiguredFails(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20)

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("boom: 429 quota exceeded")},
	)

	_, _, err := extraction.StructuredWithRetry(context.Background(), primary, nil, text, nil)
	require.Error(t, err)
}

// TestStructuredWithRetry_BothPrimaryAndEscalationHardErrorReturnsError
// confirms that when the primary hard-errors and escalation also
// hard-errors, there's no (suspicious) result left to fall back to — this
// must surface as a real error, unlike
// TestStructuredWithRetry_EscalationProviderFailureFallsBackToPrimary
// (where the primary at least had a successful, if suspicious, result).
func TestStructuredWithRetry_BothPrimaryAndEscalationHardErrorReturnsError(t *testing.T) {
	text := strings.Repeat("independent lab panel results section ", 20)

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("boom: primary down")},
	)
	escalation := llmtest.New("fake-escalation").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("boom: escalation also down")},
	)

	_, _, err := extraction.StructuredWithRetry(context.Background(), primary, escalation, text, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "primary down")
	require.ErrorContains(t, err, "escalation also down")
}

// TestInstrumentalStructuredWithRetry_EscalatesOnHardError mirrors
// StructuredWithRetry's own hard-error escalation for Stage 2b — found
// missing in the same production gap (2026-08-09): Stage 2b took no
// escalation provider at all, so a quota-exhausted primary would fail
// Extract even after OCR and Stage 2a had already successfully escalated.
func TestInstrumentalStructuredWithRetry_EscalatesOnHardError(t *testing.T) {
	text := "short imaging report text"

	primary := llmtest.New("fake-primary").WithStructured(
		llmtest.StructuredResponse{Err: errors.New("boom: 429 quota exceeded")},
	)
	escalation := llmtest.New("fake-escalation").WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[{"structure":"Печень","parameter":"КВР","value":120,"unit":"мм"}]}`)},
	)

	findings, _, err := extraction.InstrumentalStructuredWithRetry(context.Background(), primary, escalation, text, false, nil)
	require.NoError(t, err)
	require.Len(t, findings, 1)
}

// TestStructuredWithRetry_UnknownDocumentTypeFallsBackToAllCategoriesEmpty
// covers expectedCategoriesByDocumentType's fallback path: for a
// documentType with no known expected shape (here "other"), the retry
// decision reverts to the original "every category empty" check — a
// non-empty recommendations is enough to stop retrying, same as before this
// fix, since there's no more specific category to hold out for.
func TestStructuredWithRetry_UnknownDocumentTypeFallsBackToAllCategoriesEmpty(t *testing.T) {
	text := strings.Repeat("some document text with no clear structure to it here ", 10) // > minFullTextForSuspicion

	provider := llmtest.New("fake").WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "other"})},
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType:    "other",
			Recommendations: []string{"some note"},
		})},
	)

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, nil, text, nil)
	require.NoError(t, err)
	require.Len(t, got.Recommendations, 1, "a non-empty recommendations should stop the retry for an unmapped documentType, same as before")
}

// TestExtract_EscalatesOCROnPrimaryFailure covers the gap found in
// production (2026-08-09): a quota-exhausted Gemini failed the whole
// Extract call outright even with the provider's escalation
// configured, because none of OCR, Stage 2a, or Stage 2b's escalate
// parameters actually accepted or used an escalation provider for a hard
// error (Stage 2a only escalated a "successful but suspiciously empty"
// result, never an error; OCR and Stage 2b took no escalation provider at
// all). All three now retry once against escalate on a hard error — this
// exercises all three in one Extract call, matching the realistic case
// where one provider is down for every one of its endpoints (a quota
// applies to the whole provider, not just one call type), not just OCR.
func TestExtract_EscalatesOCROnPrimaryFailure(t *testing.T) {
	quotaErr := errors.New("boom: 429 quota exceeded")
	primary := llmtest.New("fake-primary", llmtest.Response{Err: quotaErr}).WithStructured(
		llmtest.StructuredResponse{Err: quotaErr}, // stage 2a would also hit the same exhausted quota
		llmtest.StructuredResponse{Err: quotaErr}, // stage 2b too
	)
	escalate := llmtest.New("fake-escalate", llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."}).WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{
			DocumentType: "lab_report",
			LabResults:   []extraction.LabResult{{Name: "ALT", Value: 28.3}},
		})},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)

	result, _, stillSuspicious, err := extraction.Extract(context.Background(), primary, escalate, primary, escalate, "base64img", "image/png", nil)
	require.NoError(t, err)
	require.False(t, stillSuspicious)
	require.Len(t, result.LabResults, 1)
}

// TestExtract_RetriesOCRAgainstEscalationWhenStructuredStaysSuspicious
// covers the gap found in production on 2026-08-20: a real single-page,
// fully legible consultation report ("2024-02-06.pdf") whose OCR
// transcription reliably stopped right after the boilerplate consent
// paragraph, never reaching the actual "Заключение" (diagnoses,
// recommendations) that followed on the same page — reproduced 3 times
// independently, across two different Gemini models, all stopping at the
// same boundary. Because OCR returned no error (just an incomplete
// transcription), ocrWithEscalation's own retry never triggered, and every
// Structured attempt (2 against primary + 1 escalation) kept re-analyzing
// that same truncated text, uselessly. Extract must now retry OCR itself
// against escalate once Structured has exhausted its own retries and is
// still suspicious, and use the result if that recovers a non-suspicious
// Structured result.
func TestExtract_RetriesOCRAgainstEscalationWhenStructuredStaysSuspicious(t *testing.T) {
	truncatedText := strings.Repeat("boilerplate consent paragraph, no clinical content here yet ", 10)
	completeText := truncatedText + " Заключение: гонартроз левого коленного сустава. Рекомендовано: физиотерапия."

	primary := llmtest.New("fake-primary", llmtest.Response{Text: truncatedText}).WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "consultation"})}, // attempt 1 on truncated text
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "consultation"})}, // attempt 2 on truncated text
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{ // attempt 1 on the retried, complete text
			DocumentType: "consultation",
			Diagnoses:    []extraction.Diagnosis{{Name: "Гонартроз левого коленного сустава"}},
		})},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)}, // stage 2b
	)
	escalate := llmtest.New("fake-escalate", llmtest.Response{Text: completeText}).WithStructured(
		llmtest.StructuredResponse{JSON: mustMarshalStructured(t, extraction.Result{DocumentType: "consultation"})}, // StructuredWithRetry's own escalation attempt, still on the (not yet retried) truncated text
	)

	result, _, stillSuspicious, err := extraction.Extract(context.Background(), primary, escalate, primary, escalate, "base64img", "application/pdf", nil)
	require.NoError(t, err)
	require.False(t, stillSuspicious, "the OCR retry against escalate must have recovered a non-suspicious result")
	require.Len(t, result.Diagnoses, 1)
	require.Equal(t, completeText, result.FullText, "the recovered, complete transcription must be what's actually used, not the original truncated one")
}

// TestExtract_OCRFailsOutrightWithoutEscalation confirms a nil ocrEscalate
// (no escalation configured for ocr_provider in llm.yaml) keeps today's
// plain "OCR fails, the whole call fails" behavior — not a regression.
func TestExtract_OCRFailsOutrightWithoutEscalation(t *testing.T) {
	primary := llmtest.New("fake-primary", llmtest.Response{Err: errors.New("boom: 429 quota exceeded")})

	_, _, _, err := extraction.Extract(context.Background(), primary, nil, primary, nil, "base64img", "image/png", nil)
	require.Error(t, err)
}

func mustMarshalStructured(t *testing.T, result extraction.Result) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(result)
	require.NoError(t, err)
	return data
}
