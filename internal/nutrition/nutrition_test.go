package nutrition_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/nutrition"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
)

func TestGenerate_ParsesRestrictionsAndRecommendations(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{
			"restrictions": [{"text": "Строго ограничить жирную пищу", "reason": "После холецистэктомии снижена выработка желчи"}],
			"recommendations": [{"text": "Увеличить клетчатку в рационе", "reason": "Жалобы на запоры в последний месяц"}]
		}`),
	})
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	got, err := nutrition.Generate(context.Background(), provider, nutrition.Input{
		Diagnoses:      []string{"Холецистэктомия"},
		RecentSymptoms: []string{"Запоры"},
	}, now)
	require.NoError(t, err)
	require.Equal(t, profile.NutritionGuidance{
		Restrictions: []profile.NutritionNote{
			{Text: "Строго ограничить жирную пищу", Reason: "После холецистэктомии снижена выработка желчи"},
		},
		Recommendations: []profile.NutritionNote{
			{Text: "Увеличить клетчатку в рационе", Reason: "Жалобы на запоры в последний месяц"},
		},
		GeneratedAt: now,
	}, got)
}

func TestGenerate_EmptyListsRoundTrip(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"restrictions": [], "recommendations": []}`),
	})

	got, err := nutrition.Generate(context.Background(), provider, nutrition.Input{Diagnoses: []string{"x"}}, time.Now())
	require.NoError(t, err)
	require.Empty(t, got.Restrictions)
	require.Empty(t, got.Recommendations)
}

func TestGenerate_ProviderErrorIsPropagated(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: provider unavailable"),
	})

	_, err := nutrition.Generate(context.Background(), provider, nutrition.Input{Diagnoses: []string{"x"}}, time.Now())
	require.Error(t, err)
}

func TestInput_Empty(t *testing.T) {
	require.True(t, nutrition.Input{}.Empty())
	require.False(t, nutrition.Input{Diagnoses: []string{"x"}}.Empty())
	require.False(t, nutrition.Input{Allergies: []string{"x"}}.Empty())
	require.False(t, nutrition.Input{RecentSymptoms: []string{"x"}}.Empty())
	// Age/sex alone don't count — see Empty's own doc comment.
	age := 42
	require.True(t, nutrition.Input{AgeYears: &age, Sex: "male"}.Empty())
}
