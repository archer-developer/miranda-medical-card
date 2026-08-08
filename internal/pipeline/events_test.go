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
	})
	p := pipeline.New(provider, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

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
	p := pipeline.New(provider, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	result, err := p.LogEvent(ctx, "user1", "Выпил ибупрофен 400", nil)
	require.NoError(t, err)
	require.Len(t, result.TimelineEventIDs, 1, "pure medication_intake note: only the medication_taken event, no redundant symptom event")
}

func TestLogEvent_ExtractionFailureStillReachesReadyAndPreservesText(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{Err: errors.New("boom")})
	p := pipeline.New(provider, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

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
	p := pipeline.New(llmtest.New("fake"), llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.LogEvent(ctx, "user1", "   ", nil)
	require.Error(t, err)
}

func TestDeleteEvent_RemovesEventTimelineAndIntake(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"d","medicationIntake":{"drugName":"ибупрофен"}}`),
	})
	p := pipeline.New(provider, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

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
	p := pipeline.New(provider, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

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
