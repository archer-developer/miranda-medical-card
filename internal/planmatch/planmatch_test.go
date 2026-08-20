package planmatch_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/planmatch"
)

func TestMatch_LabTestMatchesByIndicatorName(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "lab_test", MatchIndicatorName: "Глюкоза"},
	}
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{
			{ID: "lab_1", IndicatorName: "Глюкоза"},
		},
	}

	got := planmatch.Match(normalized, pending)
	require.Equal(t, []planmatch.Completion{{PlannedActionID: "plan_1", MatchedEntityID: "lab_1"}}, got)
}

func TestMatch_ProcedureTypesMatchByTypeAndName(t *testing.T) {
	for _, procType := range []string{"surgery", "examination", "hospitalization", "vaccination", "consultation", "other"} {
		t.Run(procType, func(t *testing.T) {
			pending := []normalization.PlannedAction{
				{ID: "plan_1", Type: procType, MatchProcedureName: "Прививка от бешенства"},
			}
			normalized := normalization.Result{
				Procedures: []normalization.Procedure{
					{ID: "proc_1", Type: procType, Name: "Прививка от бешенства"},
				},
			}
			got := planmatch.Match(normalized, pending)
			require.Equal(t, []planmatch.Completion{{PlannedActionID: "plan_1", MatchedEntityID: "proc_1"}}, got)
		})
	}
}

func TestMatch_NoMatch_TypeMismatch(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "vaccination", MatchProcedureName: "Прививка от бешенства"},
	}
	normalized := normalization.Result{
		Procedures: []normalization.Procedure{
			{ID: "proc_1", Type: "consultation", Name: "Прививка от бешенства"},
		},
	}
	require.Empty(t, planmatch.Match(normalized, pending))
}

func TestMatch_NoMatch_NameMismatch(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "lab_test", MatchIndicatorName: "Глюкоза"},
	}
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{
			{ID: "lab_1", IndicatorName: "Холестерин"},
		},
	}
	require.Empty(t, planmatch.Match(normalized, pending))
}

func TestMatch_EmptyMatchKeyNeverMatches(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "lab_test", MatchIndicatorName: ""},
		{ID: "plan_2", Type: "other", MatchProcedureName: ""},
	}
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{{ID: "lab_1", IndicatorName: ""}},
		Procedures: []normalization.Procedure{{ID: "proc_1", Type: "other", Name: ""}},
	}
	require.Empty(t, planmatch.Match(normalized, pending),
		"a pending action with no stated test/procedure name must never match — nothing to compare against")
}

func TestMatch_MultiplePendingActionsCanShareOneMatchingEntity(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "lab_test", MatchIndicatorName: "Глюкоза"},
		{ID: "plan_2", Type: "lab_test", MatchIndicatorName: "Глюкоза"},
	}
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{{ID: "lab_1", IndicatorName: "Глюкоза"}},
	}

	got := planmatch.Match(normalized, pending)
	require.ElementsMatch(t, []planmatch.Completion{
		{PlannedActionID: "plan_1", MatchedEntityID: "lab_1"},
		{PlannedActionID: "plan_2", MatchedEntityID: "lab_1"},
	}, got)
}

func TestMatch_EmptyNormalizedResultNeverErrorsOrPanics(t *testing.T) {
	pending := []normalization.PlannedAction{
		{ID: "plan_1", Type: "lab_test", MatchIndicatorName: "Глюкоза"},
	}
	require.Empty(t, planmatch.Match(normalization.Result{}, pending))
}

func TestMatch_NoPendingActions(t *testing.T) {
	normalized := normalization.Result{
		LabResults: []normalization.LabResult{{ID: "lab_1", IndicatorName: "Глюкоза"}},
	}
	require.Empty(t, planmatch.Match(normalized, nil))
}
