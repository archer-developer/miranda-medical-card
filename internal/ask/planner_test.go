package ask_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func TestPlan_ReturnsSelections(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"}, fakeProvider{name: "lab_results"})
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"providers":[{"provider":"lab_results","indicatorName":"ALT","reason":"asks about ALT"},{"provider":"timeline","reason":"asks when"}]}`),
	})

	selections, err := ask.Plan(context.Background(), provider, "Когда впервые повысился ALT?", registry)
	require.NoError(t, err)
	require.Len(t, selections, 2)
	require.Equal(t, "lab_results", selections[0].Provider)
	require.Equal(t, "ALT", selections[0].IndicatorName)
}

func TestPlan_DropsUnknownProviderNames(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"})
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"providers":[{"provider":"timeline"},{"provider":"nonexistent_hallucinated_provider"}]}`),
	})

	selections, err := ask.Plan(context.Background(), provider, "question", registry)
	require.NoError(t, err)
	require.Len(t, selections, 1, "a provider name outside the registry must be dropped, not passed through")
	require.Equal(t, "timeline", selections[0].Provider)
}

func TestPlan_EmptyProvidersIsValid(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"})
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{JSON: json.RawMessage(`{"providers":[]}`)})

	selections, err := ask.Plan(context.Background(), provider, "hello", registry)
	require.NoError(t, err)
	require.Empty(t, selections)
}
