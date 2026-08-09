package normalization_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// These tests use the same fixtures as internal/extraction's regression
// tests (internal/extraction/testdata/*_expected.json — already
// hand-verified, anonymized real Extraction output) but make zero LLM
// calls: Normalize is pure Go, so this is free to run as part of every
// `go test ./...`, unlike extraction's own live tests.
func loadFixture(t *testing.T, name string) extraction.Result {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "extraction", "testdata", name))
	require.NoError(t, err)
	var result extraction.Result
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

// fakeResolver is a minimal in-memory normalization.CanonicalUnitResolver
// — mirrors how a real Repository-backed implementation behaves ("the
// first Normalize call for an indicator establishes its canonical unit")
// without a database. Matches this family's "narrow interface + small
// hand-written fake" convention (see miranda-service-skeleton/CLAUDE.md)
// over a mocking framework.
type fakeResolver struct {
	canonical map[string]string // "userID/indicatorName" -> unit
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{canonical: map[string]string{}}
}

func (r *fakeResolver) CanonicalUnit(ctx context.Context, userID, indicatorName string) (string, bool, error) {
	unit, ok := r.canonical[userID+"/"+indicatorName]
	return unit, ok, nil
}

// recordUnits stands in for what a real integration does after Normalize
// returns: persist each result's NormalizedUnit so it becomes canonical for
// any later document. Only records the first unit seen per indicator,
// matching CanonicalUnitResolver's "first write wins" contract.
func (r *fakeResolver) recordUnits(userID string, results []normalization.LabResult) {
	for _, res := range results {
		key := userID + "/" + res.IndicatorName
		if _, exists := r.canonical[key]; !exists && res.NormalizedUnit != "" {
			r.canonical[key] = res.NormalizedUnit
		}
	}
}

func TestNormalize_InvitroCBC(t *testing.T) {
	extracted := loadFixture(t, "invitro_cbc_expected.json")

	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_test", extracted, nil)
	require.Empty(t, errs, "no date-parsing issues expected on this fixture")

	require.Len(t, result.LabResults, 20)
	require.Len(t, result.Diagnoses, 0)
	require.Len(t, result.Medications, 0)

	// Spot-check one entry end to end: field values carried through
	// correctly, TakenAt actually parsed (not left nil).
	var hgb *normalization.LabResult
	for i := range result.LabResults {
		if result.LabResults[i].IndicatorName == "Гемоглобин" {
			hgb = &result.LabResults[i]
		}
	}
	require.NotNil(t, hgb, "Гемоглобин must be present")
	require.Equal(t, 14.4, hgb.Value)
	require.Equal(t, "г/дл", hgb.Unit)
	require.Equal(t, 13.2, hgb.ReferenceLow)
	require.Equal(t, 17.3, hgb.ReferenceHigh)
	require.NotNil(t, hgb.TakenAt, "documentDate should have propagated to takenAt on the fixture — see note below if this ever fails")
}

func TestNormalize_HelixBiochemLipidCBC(t *testing.T) {
	extracted := loadFixture(t, "helix_biochem_lipid_cbc_expected.json")

	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_test", extracted, nil)
	require.Empty(t, errs)

	require.Len(t, result.LabResults, 39)
	require.Len(t, result.Medications, 2)

	// The two medications in this fixture have no dose unit split issue,
	// but do exercise the "extracted from a free-text aside, no dose
	// amount/frequency at all" shape — Normalize must not choke on a
	// mostly-empty Medication.
	names := make(map[string]normalization.Medication, len(result.Medications))
	for _, m := range result.Medications {
		names[m.DrugName] = m
	}
	require.Contains(t, names, "периндоприл")
	require.Contains(t, names, "бисопролол")
	require.Zero(t, names["периндоприл"].DoseAmount, "no dose was stated in the source text for this one")
}

func TestNormalize_LodeConsultation(t *testing.T) {
	extracted := loadFixture(t, "lode_consultation_expected.json")

	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_test", extracted, nil)
	require.Empty(t, errs)

	require.Len(t, result.Diagnoses, 9)
	require.Len(t, result.Medications, 2)
	require.Len(t, result.Procedures, 1)

	// The two diagnoses with an ICD-10 code must carry Code/CodeSystem
	// through distinctly from the seven that don't.
	var coded, uncoded int
	for _, d := range result.Diagnoses {
		if d.Code != "" {
			coded++
			require.Equal(t, "icd10", d.CodeSystem)
		} else {
			uncoded++
		}
	}
	require.Equal(t, 2, coded)
	require.Equal(t, 7, uncoded)

	// Medication dosing (amount split from unit) must survive the round trip.
	for _, m := range result.Medications {
		require.NotZero(t, m.DoseAmount, "medication %q should have a parsed dose amount", m.DrugName)
		require.NotEmpty(t, m.DoseUnit, "medication %q should have a dose unit", m.DrugName)
	}
}

func TestNormalize_GravitaUltrasound(t *testing.T) {
	extracted := loadFixture(t, "gravita_ultrasound_expected.json")

	// InstrumentalFindings come from a separate Stage 2b fixture — merge
	// them the same way Extract does (see extraction.Extract) before
	// normalizing.
	data, err := os.ReadFile(filepath.Join("..", "extraction", "testdata", "gravita_ultrasound_instrumental_expected.json"))
	require.NoError(t, err)
	var findings struct {
		InstrumentalFindings []extraction.InstrumentalFinding `json:"instrumentalFindings"`
	}
	require.NoError(t, json.Unmarshal(data, &findings))
	extracted.InstrumentalFindings = findings.InstrumentalFindings

	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_test", extracted, nil)
	require.Empty(t, errs, "every measuredAt in this fixture is the same valid date — any parse error here is a real bug")

	require.Len(t, result.InstrumentalFindings, 76)
	require.Len(t, result.Diagnoses, 3)
	require.Len(t, result.Procedures, 1)

	// Spot-check a numeric finding and a qualitative-only finding — both
	// shapes must round-trip without one clobbering the other.
	var liverKVR, liverContours *normalization.InstrumentalFinding
	for i := range result.InstrumentalFindings {
		f := &result.InstrumentalFindings[i]
		if f.Structure == "Печень" && f.Parameter == "правая доля КВР" {
			liverKVR = f
		}
		if f.Structure == "Печень" && f.Parameter == "контуры" {
			liverContours = f
		}
	}
	require.NotNil(t, liverKVR)
	require.Equal(t, 157.0, liverKVR.Value)
	require.Equal(t, "мм", liverKVR.Unit)
	require.Empty(t, liverKVR.QualitativeValue)

	require.NotNil(t, liverContours)
	require.Equal(t, "ровные", liverContours.QualitativeValue)
	require.Zero(t, liverContours.Value)
}

func TestNormalize_InvalidDateDoesNotDiscardOtherEntities(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "lab_report",
		LabResults: []extraction.LabResult{
			{Name: "ALT", Value: 28.3, TakenAt: "not-a-date"},
			{Name: "AST", Value: 21.5, TakenAt: "2025-03-14"},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_test", extracted, nil)
	require.Len(t, errs, 1, "exactly the one bad date should be reported")
	require.Len(t, result.LabResults, 2, "both entities must still be present — a bad date on one must not discard the other")
	require.Nil(t, result.LabResults[0].TakenAt)
	require.NotNil(t, result.LabResults[1].TakenAt)
}

// TestNormalize_UnitNormalization_FirstSeenBecomesCanonical uses the exact
// real values from the two fixtures that motivated unit normalization in
// the first place (invitro_cbc's Гемоглобин, 14.4 г/дл; helix's Гемоглобин
// (HGB), 150 г/л — see units.go's package doc comment) to verify the
// "first seen wins" mechanism end to end.
//
// Deliberately uses the same literal indicator name for both measurements,
// unlike the two real fixtures' actual literal names ("Гемоглобин" vs
// "Гемоглобин (HGB)") — CanonicalUnitResolver looks up by indicatorName,
// and those two strings aren't recognized as the same indicator without
// canonicalize() first solving name matching (still an open gap, see
// canonicalize's doc comment). Unit normalization and name canonicalization
// are two different open problems that happen to both need solving before
// these two real documents' Гемоглобин values actually get compared to
// each other in practice — this test isolates and verifies only the first
// one.
func TestNormalize_UnitNormalization_FirstSeenBecomesCanonical(t *testing.T) {
	resolver := newFakeResolver()

	first := extraction.Result{
		DocumentType: "lab_report",
		LabResults:   []extraction.LabResult{{Name: "Гемоглобин", Value: 14.4, Unit: "г/дл"}},
	}
	firstResult, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", first, resolver)
	require.Empty(t, errs)
	require.Equal(t, 14.4, firstResult.LabResults[0].NormalizedValue)
	require.Equal(t, "г/дл", firstResult.LabResults[0].NormalizedUnit, "first-ever measurement: its own unit becomes canonical")
	resolver.recordUnits("user_test", firstResult.LabResults)

	second := extraction.Result{
		DocumentType: "lab_report",
		LabResults:   []extraction.LabResult{{Name: "Гемоглобин", Value: 150, Unit: "г/л"}},
	}
	secondResult, errs := normalization.Normalize(context.Background(), "user_test", "doc_2", second, resolver)
	require.Empty(t, errs)
	require.Equal(t, "г/дл", secondResult.LabResults[0].NormalizedUnit, "converted to match the already-established canonical unit")
	require.InDelta(t, 15.0, secondResult.LabResults[0].NormalizedValue, 0.001, "150 г/л = 15.0 г/дл")
}

// TestNormalize_UnitNormalization_CellCountsAreExactlyEqualNotJustProportional
// covers the second real pair from the same two fixtures — Эритроциты
// "млн/мкл" vs "10^12 клеток/л" — where the conversion factor is exactly 1
// (see units.go's package doc comment for the arithmetic), not a case where
// getting the factor slightly wrong would be easy to miss in a spot check.
func TestNormalize_UnitNormalization_CellCountsAreExactlyEqualNotJustProportional(t *testing.T) {
	resolver := newFakeResolver()

	first := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "Эритроциты", Value: 4.80, Unit: "млн/мкл"}},
	}
	firstResult, _ := normalization.Normalize(context.Background(), "user_test", "doc_1", first, resolver)
	resolver.recordUnits("user_test", firstResult.LabResults)

	second := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "Эритроциты", Value: 4.9, Unit: "10^12 клеток/л"}},
	}
	secondResult, _ := normalization.Normalize(context.Background(), "user_test", "doc_2", second, resolver)
	require.Equal(t, "млн/мкл", secondResult.LabResults[0].NormalizedUnit)
	require.Equal(t, 4.9, secondResult.LabResults[0].NormalizedValue, "1 10^12/л = 1 млн/мкл exactly — no scaling")
}

func TestNormalize_UnitNormalization_UnknownUnitLeftUnset(t *testing.T) {
	resolver := newFakeResolver()
	resolver.canonical["user_test/Some Indicator"] = "widgets"

	extracted := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "Some Indicator", Value: 5, Unit: "gizmos"}},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, resolver)
	require.Empty(t, errs)
	require.Zero(t, result.LabResults[0].NormalizedValue)
	require.Empty(t, result.LabResults[0].NormalizedUnit, "gizmos -> widgets isn't a known conversion — must not guess")
}

// TestNormalize_LOINC_FallbackDictionaryAppliesWhenNoCodePrinted covers the
// far more common real-world path than transcription: the source document
// never printed a LOINC code at all (true of every fixture under
// internal/extraction/testdata — none of our real lab reports print one),
// so the small curated alias table in loinc.go is what actually assigns a
// code in practice.
func TestNormalize_LOINC_FallbackDictionaryAppliesWhenNoCodePrinted(t *testing.T) {
	extracted := extraction.Result{
		LabResults: []extraction.LabResult{
			{Name: "Гемоглобин (HGB)", Value: 150, Unit: "г/л"},
			{Name: "Совершенно неизвестный показатель", Value: 1, Unit: "ед"},
		},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, nil)
	require.Empty(t, errs)

	require.Equal(t, "718-7", result.LabResults[0].Code, "known alias should resolve via the fallback dictionary")
	require.Equal(t, "loinc", result.LabResults[0].CodeSystem)

	require.Empty(t, result.LabResults[1].Code, "an indicator not in the dictionary must be left uncoded, never guessed")
	require.Empty(t, result.LabResults[1].CodeSystem)
}

// TestNormalize_LOINC_PrintedCodeTakesPriorityOverDictionary covers the
// rarer path — extraction.LabResult.Code/CodeSystem is only ever populated
// when Extraction transcribed a code that was literally printed in the
// document — and a printed code, when present, must win over the fallback
// dictionary rather than being silently overwritten.
func TestNormalize_LOINC_PrintedCodeTakesPriorityOverDictionary(t *testing.T) {
	extracted := extraction.Result{
		LabResults: []extraction.LabResult{
			{Name: "Гемоглобин (HGB)", Code: "custom-lab-code-123", CodeSystem: "local", Value: 150, Unit: "г/л"},
		},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, nil)
	require.Empty(t, errs)

	require.Equal(t, "custom-lab-code-123", result.LabResults[0].Code)
	require.Equal(t, "local", result.LabResults[0].CodeSystem)
}

// erroringResolver always fails — used to verify Normalize treats a genuine
// resolver failure as a reportable error (not a silent "not found").
type erroringResolver struct{}

func (erroringResolver) CanonicalUnit(ctx context.Context, userID, indicatorName string) (string, bool, error) {
	return "", false, errors.New("boom: canonical unit lookup unavailable")
}

func TestNormalize_UnitNormalization_ResolverErrorIsReportedNotSwallowed(t *testing.T) {
	extracted := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "Гемоглобин", Value: 150, Unit: "г/л"}},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, erroringResolver{})

	require.Len(t, errs, 1, "a resolver failure must surface as an error, not be swallowed as 'not found'")
	require.Zero(t, result.LabResults[0].NormalizedValue, "must not guess a normalized value when the resolver itself failed")
	require.Empty(t, result.LabResults[0].NormalizedUnit)
	require.Equal(t, 150.0, result.LabResults[0].Value, "the raw value must still be present — a resolver failure must not discard the entity")
}

func TestNormalize_UnitNormalization_NilResolverLeavesNormalizedFieldsZero(t *testing.T) {
	extracted := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "ALT", Value: 28.3, Unit: "Ед/л"}},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, nil)
	require.Empty(t, errs)
	require.Zero(t, result.LabResults[0].NormalizedValue)
	require.Empty(t, result.LabResults[0].NormalizedUnit)
	require.Equal(t, 28.3, result.LabResults[0].Value, "the raw value must be unaffected by resolver being nil")
}

// TestNormalize_QualitativeLabResult covers a lab result with no numeric
// value at all — e.g. blood type — which extraction.LabResult.Value/Unit
// leaves at their zero values and populates QualitativeValue instead (see
// docs/domain/09-lab-result-and-vital-sign.md §4). Normalize must carry
// QualitativeValue through and must not attempt unit normalization (no
// Unit to convert), even with a resolver present that would otherwise be
// called.
func TestNormalize_QualitativeLabResult(t *testing.T) {
	resolver := newFakeResolver()
	extracted := extraction.Result{
		LabResults: []extraction.LabResult{{Name: "Группа крови", QualitativeValue: "A(II) Rh+"}},
	}
	result, errs := normalization.Normalize(context.Background(), "user_test", "doc_1", extracted, resolver)
	require.Empty(t, errs)
	require.Len(t, result.LabResults, 1)
	got := result.LabResults[0]
	require.Equal(t, "A(II) Rh+", got.QualitativeValue)
	require.Zero(t, got.Value)
	require.Empty(t, got.Unit)
	require.Zero(t, got.NormalizedValue)
	require.Empty(t, got.NormalizedUnit)
}
