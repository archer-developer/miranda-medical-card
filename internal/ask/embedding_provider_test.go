package ask_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestEmbeddingProvider_ResolvesDocumentSummaryContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	docs := storage.NewDocumentRepository(s)
	doc, err := docs.Add(ctx, storage.MedicalDocument{UserID: "user1", FileID: "file1"})
	require.NoError(t, err)
	require.NoError(t, docs.UpdateExtracted(ctx, doc.ID, "user1", storage.DocumentExtractedUpdate{
		Title: "Выписка", Summary: "Госпитализация по поводу пневмонии.",
	}))

	embeddings := storage.NewEmbeddingRepository(s)
	require.NoError(t, embeddings.Add(ctx, storage.Embedding{
		UserID: "user1", SourceType: "summary", SourceID: doc.ID,
		Provider: "fake", ModelVersion: "fake-model", Vector: []float32{1, 0},
	}))

	provider := ask.NewEmbeddingProvider(embeddings, docs, storage.NewSelfReportedEventRepository(s), llmtest.NewFakeEmbedder([]float32{1, 0}), "fake-model")
	chunks, err := provider.Collect(ctx, ask.KnowledgeRequest{UserID: "user1", Query: "когда мне было плохо"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, doc.ID, chunks[0].DocumentID)
	require.Contains(t, chunks[0].Content, "пневмонии")
}

func TestEmbeddingProvider_ResolvesSelfReportedEventContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	events := storage.NewSelfReportedEventRepository(s)
	event, err := events.Add(ctx, storage.SelfReportedEvent{UserID: "user1", RawText: "Было очень плохо вечером"})
	require.NoError(t, err)
	require.NoError(t, events.UpdateExtracted(ctx, event.ID, "user1", "symptom", "Плохое самочувствие вечером", ""))

	embeddings := storage.NewEmbeddingRepository(s)
	require.NoError(t, embeddings.Add(ctx, storage.Embedding{
		UserID: "user1", SourceType: "self_reported_event", SourceID: event.ID,
		Provider: "fake", ModelVersion: "fake-model", Vector: []float32{1, 0},
	}))

	provider := ask.NewEmbeddingProvider(embeddings, storage.NewDocumentRepository(s), events, llmtest.NewFakeEmbedder([]float32{1, 0}), "fake-model")
	chunks, err := provider.Collect(ctx, ask.KnowledgeRequest{UserID: "user1", Query: "когда мне было плохо"})
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, event.ID, chunks[0].EventID)
	require.Contains(t, chunks[0].Content, "Плохое самочувствие")
}

func TestEmbeddingProvider_LowScoreExcluded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	embeddings := storage.NewEmbeddingRepository(s)
	require.NoError(t, embeddings.Add(ctx, storage.Embedding{
		UserID: "user1", SourceType: "summary", SourceID: "doc1",
		Provider: "fake", ModelVersion: "fake-model", Vector: []float32{0, 1}, // orthogonal to the query vector
	}))

	provider := ask.NewEmbeddingProvider(embeddings, storage.NewDocumentRepository(s), storage.NewSelfReportedEventRepository(s), llmtest.NewFakeEmbedder([]float32{1, 0}), "fake-model")
	chunks, err := provider.Collect(ctx, ask.KnowledgeRequest{UserID: "user1", Query: "unrelated"})
	require.NoError(t, err)
	require.Empty(t, chunks, "an unrelated (orthogonal) embedding must not be surfaced as a match")
}

func TestEmbeddingProvider_EmptyQueryReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	provider := ask.NewEmbeddingProvider(storage.NewEmbeddingRepository(s), storage.NewDocumentRepository(s), storage.NewSelfReportedEventRepository(s), llmtest.NewFakeEmbedder([]float32{1, 0}), "fake-model")
	chunks, err := provider.Collect(context.Background(), ask.KnowledgeRequest{UserID: "user1"})
	require.NoError(t, err)
	require.Empty(t, chunks)
}
