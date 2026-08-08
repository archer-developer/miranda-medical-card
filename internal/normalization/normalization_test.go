package normalization_test

import (
	"encoding/json"
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

func TestNormalize_InvitroCBC(t *testing.T) {
	extracted := loadFixture(t, "invitro_cbc_expected.json")

	result, errs := normalization.Normalize("user_test", "doc_test", extracted)
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

	result, errs := normalization.Normalize("user_test", "doc_test", extracted)
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

	result, errs := normalization.Normalize("user_test", "doc_test", extracted)
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

	result, errs := normalization.Normalize("user_test", "doc_test", extracted)
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

	result, errs := normalization.Normalize("user_test", "doc_test", extracted)
	require.Len(t, errs, 1, "exactly the one bad date should be reported")
	require.Len(t, result.LabResults, 2, "both entities must still be present — a bad date on one must not discard the other")
	require.Nil(t, result.LabResults[0].TakenAt)
	require.NotNil(t, result.LabResults[1].TakenAt)
}
