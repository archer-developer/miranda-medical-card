package normalization_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/planning"
)

func TestComputeDueDateRange(t *testing.T) {
	base := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		min, max         float64
		unit             string
		wantFrom, wantTo *time.Time
		wantErr          bool
	}{
		{
			name: "no timeframe stated returns nil range, no error",
			min:  0, max: 0, unit: "",
		},
		{
			name: "min defaults to max when omitted",
			min:  0, max: 6, unit: "month",
			wantFrom: timePtr(base.AddDate(0, 6, 0)), wantTo: timePtr(base.AddDate(0, 6, 0)),
		},
		{
			name: "range through polgoda: min 5 max 7 months",
			min:  5, max: 7, unit: "month",
			wantFrom: timePtr(base.AddDate(0, 5, 0)), wantTo: timePtr(base.AddDate(0, 7, 0)),
		},
		{
			name: "days",
			min:  0, max: 10, unit: "day",
			wantFrom: timePtr(base.AddDate(0, 0, 10)), wantTo: timePtr(base.AddDate(0, 0, 10)),
		},
		{
			name: "weeks",
			min:  0, max: 2, unit: "week",
			wantFrom: timePtr(base.AddDate(0, 0, 14)), wantTo: timePtr(base.AddDate(0, 0, 14)),
		},
		{
			name: "years",
			min:  0, max: 1, unit: "year",
			wantFrom: timePtr(base.AddDate(1, 0, 0)), wantTo: timePtr(base.AddDate(1, 0, 0)),
		},
		{
			name: "min greater than max is an error",
			min:  8, max: 5, unit: "month",
			wantErr: true,
		},
		{
			name: "unknown unit is an error",
			min:  0, max: 1, unit: "fortnight",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := normalization.ComputeDueDateRange(base, tc.min, tc.max, tc.unit)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantFrom, from)
			require.Equal(t, tc.wantTo, to)
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestBuildPlannedAction_LabTest_UsesIndicatorAliasResolver(t *testing.T) {
	resolver := newFakeAliasResolver() // seeded with "Глюкоза (сахар)" -> "Глюкоза", see normalization_test.go
	base := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)

	built, err := normalization.BuildPlannedAction(context.Background(), "plan_1", "user_1",
		normalization.PlannedActionSourceDocument, "doc_1", base,
		planning.PlannedAction{
			Type: "lab_test", Description: "Повторный анализ глюкозы",
			RelatedIndicatorName: "Глюкоза (сахар)",
			DueAmountMax:         6, DueUnit: "month",
		}, resolver)

	require.NoError(t, err)
	require.Equal(t, "Глюкоза", built.MatchIndicatorName, "must land on the same canonical spelling LabResult.IndicatorName would")
	require.Empty(t, built.MatchProcedureName)
	require.Equal(t, normalization.PlannedActionStatusPending, built.Status)
	require.NotNil(t, built.DueDateTo)
	require.Equal(t, base.AddDate(0, 6, 0), *built.DueDateTo)
}

func TestBuildPlannedAction_NonLabTest_CanonicalizesProcedureName(t *testing.T) {
	base := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)

	built, err := normalization.BuildPlannedAction(context.Background(), "plan_1", "user_1",
		normalization.PlannedActionSourceSelfReported, "selfevt_1", base,
		planning.PlannedAction{
			Type: "vaccination", Description: "Прививка от бешенства",
			RelatedProcedureName: "  Прививка от бешенства  ",
			DueAmountMin:         5, DueAmountMax: 7, DueUnit: "month",
		}, nil)

	require.NoError(t, err)
	require.Equal(t, "Прививка от бешенства", built.MatchProcedureName, "must be trimmed like Procedure.Name")
	require.Empty(t, built.MatchIndicatorName)
	require.Equal(t, normalization.PlannedActionSourceSelfReported, built.SourceType)
	require.Equal(t, "selfevt_1", built.SourceID)
}

func TestBuildPlannedAction_DueDateRangeErrorIsReportedNotSwallowed(t *testing.T) {
	base := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)

	built, err := normalization.BuildPlannedAction(context.Background(), "plan_1", "user_1",
		normalization.PlannedActionSourceDocument, "doc_1", base,
		planning.PlannedAction{
			Type: "other", Description: "bad range",
			DueAmountMin: 8, DueAmountMax: 5, DueUnit: "month",
		}, nil)

	require.Error(t, err)
	require.Nil(t, built.DueDateFrom)
	require.Nil(t, built.DueDateTo)
	// The rest of the entity is still built even though the date range
	// failed — same "don't discard the whole entity over one bad field"
	// posture as Normalize's other per-entity errors.
	require.Equal(t, "bad range", built.Description)
}

func TestNormalize_PlannedActions_AnchoredOnDocumentDate(t *testing.T) {
	extracted := extraction.Result{
		DocumentType: "consultation",
		DocumentDate: "2026-01-19",
		FullText:     "some text",
		PlannedActions: []planning.PlannedAction{
			{
				Type: "lab_test", Description: "Повторный анализ глюкозы крови",
				RelatedIndicatorName: "Глюкоза",
				DueAmountMin:         5, DueAmountMax: 7, DueUnit: "month",
			},
			{
				// No timeframe stated at all — must still produce a
				// PlannedAction, just with nil due dates.
				Type: "lab_test", Description: "Сдать кровь на глюкозу",
				RelatedIndicatorName: "Глюкоза",
			},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.PlannedActions, 2)

	first := result.PlannedActions[0]
	require.Equal(t, normalization.PlannedActionSourceDocument, first.SourceType)
	require.Equal(t, "doc_1", first.SourceID)
	require.NotNil(t, first.DueDateFrom)
	require.NotNil(t, first.DueDateTo)
	documentDate := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)
	require.Equal(t, documentDate.AddDate(0, 5, 0), *first.DueDateFrom)
	require.Equal(t, documentDate.AddDate(0, 7, 0), *first.DueDateTo)

	second := result.PlannedActions[1]
	require.Nil(t, second.DueDateFrom)
	require.Nil(t, second.DueDateTo)
	require.Equal(t, normalization.PlannedActionStatusPending, second.Status)
}

func TestNormalize_PlannedActions_FallsBackToNowWithoutDocumentDate(t *testing.T) {
	before := time.Now().UTC()
	extracted := extraction.Result{
		DocumentType: "other",
		FullText:     "some text",
		PlannedActions: []planning.PlannedAction{
			{Type: "other", Description: "no document date", DueAmountMax: 1, DueUnit: "month"},
		},
	}

	result, errs := normalization.Normalize(context.Background(), "user_1", "doc_1", extracted, nil, nil)
	require.Empty(t, errs)
	require.Len(t, result.PlannedActions, 1)
	require.NotNil(t, result.PlannedActions[0].DueDateTo)
	require.True(t, result.PlannedActions[0].DueDateTo.After(before.AddDate(0, 0, 25)),
		"due date should anchor roughly on 'now' (within a month + a few days of slack), got %v vs now %v",
		result.PlannedActions[0].DueDateTo, before)
}

func TestPlannedAction_Overdue(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.AddDate(0, 0, -1)
	future := now.AddDate(0, 0, 1)

	require.True(t, normalization.PlannedAction{Status: normalization.PlannedActionStatusPending, DueDateTo: &past}.Overdue(now))
	require.False(t, normalization.PlannedAction{Status: normalization.PlannedActionStatusPending, DueDateTo: &future}.Overdue(now))
	require.False(t, normalization.PlannedAction{Status: normalization.PlannedActionStatusPending, DueDateTo: nil}.Overdue(now),
		"no due date at all must never be overdue")
	require.False(t, normalization.PlannedAction{Status: normalization.PlannedActionStatusCompleted, DueDateTo: &past}.Overdue(now),
		"completed is never overdue even with a past due date")
	require.False(t, normalization.PlannedAction{Status: normalization.PlannedActionStatusDeclined, DueDateTo: &past}.Overdue(now),
		"declined is never overdue even with a past due date")
}
