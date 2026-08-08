package extraction_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/gemini"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
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

func TestFixtures_InvitroCBC(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "invitro_cbc.txt")
	expected := loadExpected(t, "invitro_cbc_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, text)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
	assertLabResultValuesMatch(t, expected, got)
}

func TestFixtures_HelixBiochemLipidCBC(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "helix_biochem_lipid_cbc.txt")
	expected := loadExpected(t, "helix_biochem_lipid_cbc_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, text)
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

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, text)
	require.NoError(t, err)

	assertCategoryCountsMatch(t, expected, got)
}

func TestFixtures_GravitaUltrasound_Clinical(t *testing.T) {
	skipUnlessLive(t)
	provider := testProvider(t)
	text := loadText(t, "gravita_ultrasound.txt")
	expected := loadExpected(t, "gravita_ultrasound_expected.json")

	got, _, err := extraction.StructuredWithRetry(context.Background(), provider, text)
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
	got, _, err := extraction.InstrumentalStructuredWithRetry(context.Background(), provider, text, true)
	require.NoError(t, err)

	require.Len(t, got, len(expected.InstrumentalFindings), "instrumentalFindings count")
}
