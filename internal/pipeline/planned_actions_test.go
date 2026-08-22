package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestDeclinePlannedAction_HappyPath(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	planRepo := storage.NewPlannedActionRepository(s)
	_, err := planRepo.Add(ctx, normalization.PlannedAction{
		UserID: "user1", SourceType: "document", SourceID: "doc1",
		Type: "lab_test", Description: "Повторный анализ глюкозы",
	})
	require.NoError(t, err)
	rabies, err := planRepo.Add(ctx, normalization.PlannedAction{
		UserID: "user1", SourceType: "self_reported", SourceID: "selfevt_1",
		Type: "vaccination", Description: "Прививка от бешенства",
	})
	require.NoError(t, err)

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"matchId":"` + rabies.ID + `"}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	declined, err := p.DeclinePlannedAction(ctx, "user1", "отмени прививку от бешенства")
	require.NoError(t, err)
	require.Equal(t, rabies.ID, declined.ID)
	require.Equal(t, normalization.PlannedActionStatusDeclined, declined.Status)

	all, err := planRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	for _, a := range all {
		if a.ID == rabies.ID {
			require.Equal(t, normalization.PlannedActionStatusDeclined, a.Status)
		} else {
			require.Equal(t, normalization.PlannedActionStatusPending, a.Status, "the other pending action must be untouched")
		}
	}
}

func TestDeclinePlannedAction_NoPendingActionsAtAll(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.DeclinePlannedAction(ctx, "user1", "отмени что угодно")
	require.Error(t, err)
	var notFound *pipeline.PlannedActionNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.PendingDescriptions)
}

func TestDeclinePlannedAction_NoConfidentMatchListsPendingDescriptions(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	planRepo := storage.NewPlannedActionRepository(s)
	_, err := planRepo.Add(ctx, normalization.PlannedAction{
		UserID: "user1", SourceType: "document", SourceID: "doc1",
		Type: "lab_test", Description: "Повторный анализ глюкозы",
	})
	require.NoError(t, err)

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`), // model found no confident match
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err = p.DeclinePlannedAction(ctx, "user1", "отмени что-то непонятное")
	require.Error(t, err)
	var notFound *pipeline.PlannedActionNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, []string{"Повторный анализ глюкозы"}, notFound.PendingDescriptions)

	pending, err := planRepo.ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, pending, 1, "no match must leave the pending action untouched")
}

func TestDeclinePlannedAction_EmptyTextRejected(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.DeclinePlannedAction(ctx, "user1", "   ")
	require.Error(t, err)
	var notFound *pipeline.PlannedActionNotFoundError
	require.NotErrorAs(t, err, &notFound, "empty text must be its own plain error, not PlannedActionNotFoundError")
}
