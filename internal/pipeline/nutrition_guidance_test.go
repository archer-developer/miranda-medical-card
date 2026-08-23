package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
)

// TestRebuildProfile_NutritionGuidance_SkipsLLMCallWhenInputEmpty covers
// docs/adr/006-nutrition-guidance.md §4: a user with no active diagnosis,
// no allergy, and no recent symptom must not trigger a Nutrition Guidance
// LLM call at all — only events.Extract's own Structured call happens.
// FakeProvider's "more calls than scripted" panic (llmtest.FakeProvider's
// own doc comment) would already fail this test if the short-circuit
// regressed, but this also asserts the positive: NutritionGuidance stays
// the zero value, and exactly one Structured call was made.
func TestRebuildProfile_NutritionGuidance_SkipsLLMCallWhenInputEmpty(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","description":"Купил новый тонометр"}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	_, err := p.LogEvent(ctx, "user1", "Купил новый тонометр", nil)
	require.NoError(t, err)

	got, err := p.GetProfile(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, profile.NutritionGuidance{}, got.NutritionGuidance)
	require.Len(t, provider.StructuredRequests, 1, "only events.Extract's own call — nutrition.Generate must not fire for an empty Input")
}

// TestRebuildProfile_NutritionGuidance_CarriesForwardPreviousValueOnError
// covers docs/adr/006-nutrition-guidance.md §5: MedicalProfile only
// supports a full Replace, so a transient nutrition.Generate failure on a
// later rebuild must not wipe out a previously successful
// NutritionGuidance — the stale-but-correct value should survive.
func TestRebuildProfile_NutritionGuidance_CarriesForwardPreviousValueOnError(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(
		// First log_event: events.Extract classifies a symptom, then
		// nutrition.Generate succeeds.
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"category":"symptom","description":"Изжога"}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"restrictions":[{"text":"Ограничить кофе","reason":"Жалоба на изжогу"}],"recommendations":[]}`)},
		// Second log_event: events.Extract classifies another symptom,
		// then nutrition.Generate fails outright.
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"category":"symptom","description":"Тошнота"}`)},
		llmtest.StructuredResponse{Err: errors.New("boom: provider unavailable")},
	)
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	_, err := p.LogEvent(ctx, "user1", "Изжога после еды", nil)
	require.NoError(t, err)
	first, err := p.GetProfile(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, "Ограничить кофе", first.NutritionGuidance.Restrictions[0].Text)

	// Second call's rebuildProfile hits a provider error — LogEvent itself
	// must still succeed (Nutrition Guidance is an enrichment, not a
	// required part of a successful rebuild).
	_, err = p.LogEvent(ctx, "user1", "Тошнота с утра", nil)
	require.NoError(t, err)

	second, err := p.GetProfile(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, first.NutritionGuidance, second.NutritionGuidance, "a failed regeneration must carry the previous value forward, not clear it")
}
