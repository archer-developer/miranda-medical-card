package normalization_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

func TestNormalize_Diagnosis_ExpectedResolutionAnchoredOnDiagnosedAt(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "consultation",
		DocumentDate: "2026-01-19",
		FullText:     "some text",
		Diagnoses: []extraction.Diagnosis{
			{
				Name: "ОРВИ", Status: "active", DiagnosedAt: "2026-01-10",
				ExpectedResolutionAmountMin: 7, ExpectedResolutionAmountMax: 14, ExpectedResolutionUnit: "day",
			},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.Diagnoses, 1)

	dx := result.Diagnoses[0]
	diagnosedAt := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	require.NotNil(t, dx.ExpectedResolutionFrom)
	require.NotNil(t, dx.ExpectedResolutionTo)
	require.Equal(t, diagnosedAt.AddDate(0, 0, 7), *dx.ExpectedResolutionFrom)
	require.Equal(t, diagnosedAt.AddDate(0, 0, 14), *dx.ExpectedResolutionTo)
}

func TestNormalize_Diagnosis_ExpectedResolutionFallsBackToDocumentDateWithoutDiagnosedAt(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "consultation",
		DocumentDate: "2026-01-19",
		FullText:     "some text",
		Diagnoses: []extraction.Diagnosis{
			{
				Name: "ОРВИ", Status: "active",
				ExpectedResolutionAmountMax: 10, ExpectedResolutionUnit: "day",
			},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.Diagnoses, 1)

	documentDate := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)
	require.NotNil(t, result.Diagnoses[0].ExpectedResolutionTo)
	require.Equal(t, documentDate.AddDate(0, 0, 10), *result.Diagnoses[0].ExpectedResolutionTo)
}

func TestNormalize_Diagnosis_NoExpectedResolutionStatedLeavesFieldsNil(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "consultation",
		FullText:     "some text",
		Diagnoses: []extraction.Diagnosis{
			{Name: "Хронический тонзиллит", Status: "chronic"},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.Diagnoses, 1)
	require.Nil(t, result.Diagnoses[0].ExpectedResolutionFrom)
	require.Nil(t, result.Diagnoses[0].ExpectedResolutionTo)
}

func TestNormalize_Diagnosis_StatusReasoningCarriesThroughButNeverIntoNotes(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "consultation",
		FullText:     "some text",
		Diagnoses: []extraction.Diagnosis{
			{
				Name: "Серная пробка", Status: "resolved", Notes: "left ear",
				StatusReasoning: "Серная пробка была удалена во время данного приема.",
			},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.Diagnoses, 1)
	require.Equal(t, "Серная пробка была удалена во время данного приема.", result.Diagnoses[0].StatusReasoning)
	require.Equal(t, "left ear", result.Diagnoses[0].Notes, "StatusReasoning must never be folded into Notes")
}
