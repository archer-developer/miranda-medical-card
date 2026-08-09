package ask_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestAsker_Ask_EndToEnd(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	labs := storage.NewLabResultRepository(s)
	require.NoError(t, labs.Add(ctx, normalization.LabResult{
		ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT",
		Value: 54.7, Unit: "U/L", ReferenceHigh: 40, TakenAt: mustDate("2025-03-12"),
	}))

	registry := ask.NewRegistry(
		ask.NewTimelineProvider(storage.NewTimelineRepository(s)),
		ask.NewLabProvider(labs),
	)

	planner := llmtest.New("planner").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"providers":[{"provider":"lab_results","indicatorName":"ALT","reason":"asks about ALT"}]}`),
	})
	answerer := llmtest.New("answerer").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"answer":"Согласно результатам анализа от 12 марта 2025, ALT составил 54.7 U/L, что выше нормы (до 40 U/L)."}`),
	})

	asker := ask.NewAsker(planner, nil, answerer, nil, registry, 5*time.Second, 20, nil)
	result, err := asker.Ask(ctx, "user1", "Когда впервые повысился ALT?")
	require.NoError(t, err)
	require.Contains(t, result.Answer, "54.7")
	require.Len(t, result.Sources, 1)
	require.Equal(t, "doc1", result.Sources[0].DocumentID)
}

func TestAsker_Ask_NoProvidersSelectedStillAnswers(t *testing.T) {
	s := newTestStore(t)
	registry := ask.NewRegistry(ask.NewTimelineProvider(storage.NewTimelineRepository(s)))

	planner := llmtest.New("planner").WithStructured(llmtest.StructuredResponse{JSON: json.RawMessage(`{"providers":[]}`)})
	answerer := llmtest.New("answerer").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"answer":"В медицинской истории нет информации об этом."}`),
	})

	asker := ask.NewAsker(planner, nil, answerer, nil, registry, 5*time.Second, 20, nil)
	result, err := asker.Ask(context.Background(), "user1", "Какая столица Франции?")
	require.NoError(t, err)
	require.Contains(t, result.Answer, "нет информации")
	require.Empty(t, result.Sources)
}

func TestAsker_Ask_ProviderErrorDoesNotFailWholeRequest(t *testing.T) {
	s := newTestStore(t)
	registry := ask.NewRegistry(ask.NewTimelineProvider(storage.NewTimelineRepository(s)), failingProvider{})

	planner := llmtest.New("planner").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"providers":[{"provider":"timeline"},{"provider":"failing"}]}`),
	})
	answerer := llmtest.New("answerer").WithStructured(llmtest.StructuredResponse{JSON: json.RawMessage(`{"answer":"ok"}`)})

	asker := ask.NewAsker(planner, nil, answerer, nil, registry, 5*time.Second, 20, nil)
	result, err := asker.Ask(context.Background(), "user1", "question")
	require.NoError(t, err, "one provider failing must not fail the whole ask")
	require.Equal(t, "ok", result.Answer)
}

type failingProvider struct{}

func (failingProvider) Metadata() ask.ProviderMetadata { return ask.ProviderMetadata{Name: "failing"} }
func (failingProvider) Collect(context.Context, ask.KnowledgeRequest) ([]ask.KnowledgeChunk, error) {
	return nil, context.DeadlineExceeded
}
