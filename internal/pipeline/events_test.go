package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestLogEvent_SymptomWithMedicationIntake_CreatesBothTimelineEvents(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"Приступ головной боли","medicationIntake":{"drugName":"ибупрофен","doseAmount":400,"doseUnit":"mg","reason":"головная боль"}}`),
	}, emptyNutritionResponse)
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	result, err := p.LogEvent(ctx, "user1", "Приступ головной боли, принял 400 мг ибупрофена", nil)
	require.NoError(t, err)
	require.Equal(t, storage.DocumentStatusReady, result.Status)
	require.Equal(t, "symptom", result.Category)
	require.Len(t, result.TimelineEventIDs, 2, "both a symptom note and a medication_taken event")
	require.NotNil(t, result.MedicationIntake)
	require.Equal(t, "ибупрофен", result.MedicationIntake.DrugName)

	events, err := storage.NewTimelineRepository(s).List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Len(t, events, 2)

	intakes, err := storage.NewMedicationIntakeRepository(s).ListByUser(ctx, "user1", storage.DateRange{})
	require.NoError(t, err)
	require.Len(t, intakes, 1)
	require.Equal(t, "ибупрофен", intakes[0].DrugName)

	ftsResults, err := storage.NewFTSRepository(s).SearchEvents(ctx, "user1", "головной боли", 10)
	require.NoError(t, err)
	require.Len(t, ftsResults, 1)

	embeddings, err := storage.NewEmbeddingRepository(s).ListByUser(ctx, "user1", "fake-model")
	require.NoError(t, err)
	require.Len(t, embeddings, 1)
	require.Equal(t, "self_reported_event", embeddings[0].SourceType)
}

func TestLogEvent_MedicationOnlyCategory_SkipsRedundantSymptomEvent(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"medication_intake","description":"Принял ибупрофен","medicationIntake":{"drugName":"ибупрофен","doseAmount":400,"doseUnit":"mg"}}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	result, err := p.LogEvent(ctx, "user1", "Выпил ибупрофен 400", nil)
	require.NoError(t, err)
	require.Len(t, result.TimelineEventIDs, 1, "pure medication_intake note: only the medication_taken event, no redundant symptom event")
}

func TestLogEvent_ExtractionFailureStillReachesReadyAndPreservesText(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{Err: errors.New("boom")})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	result, err := p.LogEvent(ctx, "user1", "нечто нераспознаваемое", nil)
	require.NoError(t, err, "extraction failure must never fail log_event itself")
	require.Equal(t, storage.DocumentStatusReady, result.Status)
	require.Empty(t, result.Category)
	require.Len(t, result.TimelineEventIDs, 1, "the raw note still gets a fallback Timeline event")

	events, err := storage.NewSelfReportedEventRepository(s).Get(ctx, result.EventID, "user1")
	require.NoError(t, err)
	require.Equal(t, "нечто нераспознаваемое", events.RawText)
}

func TestLogEvent_EmptyTextRejected(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	_, err := p.LogEvent(ctx, "user1", "   ", nil)
	require.Error(t, err)
}

func TestDeleteEvent_RemovesEventTimelineAndIntake(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"d","medicationIntake":{"drugName":"ибупрофен"}}`),
	}, emptyNutritionResponse)
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	logged, err := p.LogEvent(ctx, "user1", "text", nil)
	require.NoError(t, err)

	deleted, err := p.DeleteEvent(ctx, "user1", logged.EventID)
	require.NoError(t, err)
	require.True(t, deleted)

	_, err = storage.NewSelfReportedEventRepository(s).Get(ctx, logged.EventID, "user1")
	require.ErrorIs(t, err, storage.ErrNotFound)

	events, err := storage.NewTimelineRepository(s).List(ctx, "user1", storage.TimelineFilter{})
	require.NoError(t, err)
	require.Empty(t, events)

	intakes, err := storage.NewMedicationIntakeRepository(s).ListByUser(ctx, "user1", storage.DateRange{})
	require.NoError(t, err)
	require.Empty(t, intakes)

	embeddings, err := storage.NewEmbeddingRepository(s).ListByUser(ctx, "user1", "fake-model")
	require.NoError(t, err)
	require.Empty(t, embeddings)
}

func TestDeleteEvent_IdempotentAndOwnershipMismatchLooksLikeNotFound(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{JSON: json.RawMessage(`{}`)})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	logged, err := p.LogEvent(ctx, "user1", "text", nil)
	require.NoError(t, err)

	deleted, err := p.DeleteEvent(ctx, "user2", logged.EventID)
	require.NoError(t, err)
	require.False(t, deleted, "wrong owner must look identical to not-found")

	deleted, err = p.DeleteEvent(ctx, "user1", "selfevt_never_existed")
	require.NoError(t, err)
	require.False(t, deleted)

	deleted, err = p.DeleteEvent(ctx, "user1", logged.EventID)
	require.NoError(t, err)
	require.True(t, deleted)

	deleted, err = p.DeleteEvent(ctx, "user1", logged.EventID)
	require.NoError(t, err, "deleting again must not error")
	require.False(t, deleted)
}

func TestLogEvent_PlannedAction_CreatesSelfReportedPendingAction(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","description":"Нужно сделать прививку от бешенства","plannedAction":{"type":"vaccination","description":"Прививка от бешенства","relatedProcedureName":"Прививка от бешенства","dueAmountMin":5,"dueAmountMax":7,"dueUnit":"month"}}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	result, err := p.LogEvent(ctx, "user1", "нужно сделать прививку от бешенства в течение полугода", nil)
	require.NoError(t, err)
	require.NotNil(t, result.PlannedAction)
	require.Equal(t, "vaccination", result.PlannedAction.Type)
	require.Equal(t, "Прививка от бешенства", result.PlannedAction.Description)
	require.NotNil(t, result.PlannedAction.DueDateTo)

	pending, err := storage.NewPlannedActionRepository(s).ListPending(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "self_reported", pending[0].SourceType)
	require.Equal(t, result.EventID, pending[0].SourceID)
	require.Equal(t, "Прививка от бешенства", pending[0].MatchProcedureName)
}

func TestLogEvent_NoPlannedActionInText_CreatesNoRecord(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"головная боль"}`),
	}, emptyNutritionResponse)
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	result, err := p.LogEvent(ctx, "user1", "болит голова", nil)
	require.NoError(t, err)
	require.Nil(t, result.PlannedAction)

	all, err := storage.NewPlannedActionRepository(s).ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestDeleteEvent_RemovesPlannedAction(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"other","plannedAction":{"type":"vaccination","description":"Прививка от бешенства","relatedProcedureName":"Прививка от бешенства","dueAmountMax":6,"dueUnit":"month"}}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	logged, err := p.LogEvent(ctx, "user1", "text", nil)
	require.NoError(t, err)
	require.NotNil(t, logged.PlannedAction)

	deleted, err := p.DeleteEvent(ctx, "user1", logged.EventID)
	require.NoError(t, err)
	require.True(t, deleted)

	all, err := storage.NewPlannedActionRepository(s).ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, all, "delete_event must clean up the PlannedAction it created, same as MedicationIntake")
}
