package diagnosisreconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/diagnosisreconcile"
)

func TestReconcile_EmptyCandidatesReturnsEmptyWithoutCallingProvider(t *testing.T) {
	// No scripted response at all — if Reconcile called the provider despite
	// an empty candidate list, llmtest.FakeProvider would error on an
	// unscripted call and this test would fail.
	provider := llmtest.New("fake")

	got, err := diagnosisreconcile.Reconcile(context.Background(), provider, "Хронический тонзиллит, вне обострения", "chronic", nil)
	require.NoError(t, err)
	require.Empty(t, got.TargetID)
}

func TestReconcile_Refines(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"targetId":"dx_1","relation":"refines"}`),
	})

	got, err := diagnosisreconcile.Reconcile(context.Background(), provider, "Хронический тонзиллит, вне обострения", "chronic", []diagnosisreconcile.Candidate{
		{ID: "dx_1", Name: "Хронический тонзиллит", Status: "chronic"},
	})
	require.NoError(t, err)
	require.Equal(t, "dx_1", got.TargetID)
	require.Equal(t, diagnosisreconcile.RelationRefines, got.Relation)
}

func TestReconcile_Cancels(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"targetId":"dx_1","relation":"cancels"}`),
	})

	got, err := diagnosisreconcile.Reconcile(context.Background(), provider, "Диагноз не подтвердился при дообследовании", "active", []diagnosisreconcile.Candidate{
		{ID: "dx_1", Name: "Подозрение на X", Status: "active"},
	})
	require.NoError(t, err)
	require.Equal(t, "dx_1", got.TargetID)
	require.Equal(t, diagnosisreconcile.RelationCancels, got.Relation)
}

func TestReconcile_NoConfidentMatchReturnsEmpty(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`),
	})

	got, err := diagnosisreconcile.Reconcile(context.Background(), provider, "ОРВИ", "active", []diagnosisreconcile.Candidate{
		{ID: "dx_1", Name: "Дислипидемия", Status: "chronic"},
	})
	require.NoError(t, err)
	require.Empty(t, got.TargetID)
	require.Empty(t, got.Relation)
}

func TestReconcile_TargetIDWithoutRelationTreatedAsNoMatchByCaller(t *testing.T) {
	// The schema constrains relation's enum, but a caller must still defend
	// against a targetId returned without any relation at all (or a value
	// outside the enum) rather than trusting a partial/malformed result.
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"targetId":"dx_1"}`),
	})

	got, err := diagnosisreconcile.Reconcile(context.Background(), provider, "ОРВИ", "active", []diagnosisreconcile.Candidate{
		{ID: "dx_1", Name: "Дислипидемия", Status: "chronic"},
	})
	require.NoError(t, err)
	require.Equal(t, "dx_1", got.TargetID)
	require.Empty(t, got.Relation)
	require.NotEqual(t, diagnosisreconcile.RelationRefines, got.Relation)
	require.NotEqual(t, diagnosisreconcile.RelationCancels, got.Relation)
}

func TestReconcile_ProviderErrorIsPropagated(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: provider unavailable"),
	})

	_, err := diagnosisreconcile.Reconcile(context.Background(), provider, "ОРВИ", "active", []diagnosisreconcile.Candidate{
		{ID: "dx_1", Name: "Дислипидемия", Status: "chronic"},
	})
	require.Error(t, err)
}
