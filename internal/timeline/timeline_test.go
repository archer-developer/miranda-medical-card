package timeline_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

func date(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestBuild_DiagnosisAndMedicationAndProcedure(t *testing.T) {
	normalized := normalization.Result{
		Diagnoses: []normalization.Diagnosis{
			{ID: "dx_1", Name: "Артериальная гипертензия", DiagnosedAt: date("2026-03-01")},
		},
		Medications: []normalization.Medication{
			{ID: "med_1", DrugName: "периндоприл", DoseAmount: 5, DoseUnit: "mg", Status: "active", StartedAt: date("2026-03-01")},
			{ID: "med_2", DrugName: "старый препарат", Status: "discontinued", EndedAt: date("2026-02-15")},
		},
		Procedures: []normalization.Procedure{
			{ID: "proc_1", Type: "vaccination", Name: "Грипп", PerformedAt: date("2026-01-10")},
		},
	}

	events := timeline.Build("user1", "doc1", "Консультация", date("2026-03-01"), normalized)
	require.Len(t, events, 4)

	byType := map[string][]timeline.Event{}
	for _, e := range events {
		byType[e.Type] = append(byType[e.Type], e)
		require.Equal(t, "user1", e.UserID)
		require.Equal(t, "doc1", e.DocumentID)
		require.NotEmpty(t, e.ID)
	}
	require.Len(t, byType["diagnosis"], 1)
	require.Len(t, byType["medication_started"], 1)
	require.Len(t, byType["medication_stopped"], 1)
	require.Len(t, byType["vaccination"], 1)
}

func TestBuild_EntityWithNoDateAndNoDocumentDateFallbackIsSkipped(t *testing.T) {
	normalized := normalization.Result{
		Diagnoses: []normalization.Diagnosis{{ID: "dx_1", Name: "Undated"}},
	}
	events := timeline.Build("user1", "doc1", "", nil, normalized)
	require.Empty(t, events, "no date on the entity and no document date fallback: must not fabricate a date")
}

func TestBuild_EntityWithNoDateFallsBackToDocumentDate(t *testing.T) {
	normalized := normalization.Result{
		Diagnoses: []normalization.Diagnosis{{ID: "dx_1", Name: "Fallback"}},
	}
	events := timeline.Build("user1", "doc1", "", date("2026-05-01"), normalized)
	require.Len(t, events, 1)
	require.True(t, date("2026-05-01").Equal(events[0].Date))
}

func TestBuild_LabResultsGroupedIntoOneEvent(t *testing.T) {
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{
			{ID: "lab_1", IndicatorName: "АЛТ", Value: 54.7, Unit: "U/L", ReferenceHigh: 40, TakenAt: date("2026-03-12")},
			{ID: "lab_2", IndicatorName: "АСТ", Value: 21.5, Unit: "U/L", ReferenceHigh: 40, TakenAt: date("2026-03-12")},
			{ID: "lab_3", IndicatorName: "Глюкоза", Value: 5.1, Unit: "mmol/L", TakenAt: date("2026-03-12")},
		},
	}
	events := timeline.Build("user1", "doc1", "Общий анализ крови", nil, normalized)
	require.Len(t, events, 1, "20-40 lab results in one document must become one Timeline event, not one each")
	require.Equal(t, "lab_result", events[0].Type)
	require.Equal(t, "Общий анализ крови", events[0].Title)
	require.Contains(t, events[0].Summary, "3 показателей")
	require.Contains(t, events[0].Summary, "АЛТ", "the out-of-range result should be called out by name")
	require.Contains(t, events[0].Summary, "повышен")
	require.NotContains(t, events[0].Summary, "АСТ", "in-range results should not be listed as notable")
}

func TestBuild_AllergiesGroupedIntoOneDocumentEvent(t *testing.T) {
	normalized := normalization.Result{
		Allergies: []normalization.Allergy{
			{ID: "allergy_1", Substance: "Пенициллин"},
			{ID: "allergy_2", Substance: "Аспирин"},
		},
	}
	events := timeline.Build("user1", "doc1", "", date("2026-01-01"), normalized)
	require.Len(t, events, 1)
	require.Equal(t, "document", events[0].Type)
	require.Contains(t, events[0].Summary, "Пенициллин")
	require.Contains(t, events[0].Summary, "Аспирин")
}

func TestBuild_VitalSignBloodPressure(t *testing.T) {
	normalized := normalization.Result{
		VitalSigns: []normalization.VitalSign{
			{ID: "vital_1", Type: "blood_pressure", Systolic: 130, Diastolic: 82, MeasuredAt: date("2026-05-30")},
		},
	}
	events := timeline.Build("user1", "doc1", "", nil, normalized)
	require.Len(t, events, 1)
	require.Equal(t, "vital_sign", events[0].Type)
	require.Contains(t, events[0].Summary, "130/82")
}

func TestBuild_EventsSortedChronologically(t *testing.T) {
	normalized := normalization.Result{
		Diagnoses: []normalization.Diagnosis{
			{ID: "dx_1", Name: "Later", DiagnosedAt: date("2026-06-01")},
			{ID: "dx_2", Name: "Earlier", DiagnosedAt: date("2026-01-01")},
		},
	}
	events := timeline.Build("user1", "doc1", "", nil, normalized)
	require.Len(t, events, 2)
	require.Equal(t, "Earlier", events[0].Title)
	require.Equal(t, "Later", events[1].Title)
}
