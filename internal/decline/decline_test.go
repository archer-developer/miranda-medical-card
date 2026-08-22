package decline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
)

const testKind = "The user wants to cancel a planned medical action."

func TestMatch_EmptyCandidatesReturnsEmptyWithoutCallingProvider(t *testing.T) {
	// No scripted response at all — if Match called the provider despite
	// an empty candidate list, llmtest.FakeProvider would error on an
	// unscripted call and this test would fail.
	provider := llmtest.New("fake")

	got, err := decline.Match(context.Background(), provider, testKind, "отмени прививку от бешенства", nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestMatch_ExactCandidateReturned(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"matchId":"plan_2"}`),
	})

	got, err := decline.Match(context.Background(), provider, testKind, "отмени прививку от бешенства", []decline.Candidate{
		{ID: "plan_1", Description: "Повторный анализ глюкозы", Type: "lab_test"},
		{ID: "plan_2", Description: "Прививка от бешенства", Type: "vaccination"},
	})
	require.NoError(t, err)
	require.Equal(t, "plan_2", got)
}

func TestMatch_NoConfidentMatchReturnsEmpty(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`),
	})

	got, err := decline.Match(context.Background(), provider, testKind, "отмени что-то непонятное", []decline.Candidate{
		{ID: "plan_1", Description: "Повторный анализ глюкозы", Type: "lab_test"},
	})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestMatch_ProviderErrorIsPropagated(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: provider unavailable"),
	})

	_, err := decline.Match(context.Background(), provider, testKind, "text", []decline.Candidate{{ID: "plan_1", Description: "x"}})
	require.Error(t, err)
}
