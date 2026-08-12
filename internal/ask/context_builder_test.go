package ask_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func TestRankChunks_DropsExactDuplicateContent(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"}, fakeProvider{name: "documents"})
	chunks := []ask.KnowledgeChunk{
		{Source: "timeline", Content: "same fact", Confidence: 1.0},
		{Source: "documents", Content: "same fact", Confidence: 0.82},
	}
	ranked := ask.RankChunks(chunks, registry, 10)
	require.Len(t, ranked, 1)
}

func TestRankChunks_SortsByConfidenceThenProviderPriority(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"}, fakeProvider{name: "medications"}, fakeProvider{name: "documents"})
	chunks := []ask.KnowledgeChunk{
		{Source: "documents", Content: "low confidence", Confidence: 0.5},
		{Source: "medications", Content: "tie A", Confidence: 1.0},
		{Source: "timeline", Content: "tie B", Confidence: 1.0},
	}
	ranked := ask.RankChunks(chunks, registry, 10)
	require.Len(t, ranked, 3)
	require.Equal(t, "tie B", ranked[0].Content, "timeline was registered before medications, so it wins the confidence tie")
	require.Equal(t, "tie A", ranked[1].Content)
	require.Equal(t, "low confidence", ranked[2].Content)
}

func TestRankChunks_RespectsMaxChunks(t *testing.T) {
	registry := ask.NewRegistry(fakeProvider{name: "timeline"})
	chunks := []ask.KnowledgeChunk{
		{Source: "timeline", Content: "a", Confidence: 1.0},
		{Source: "timeline", Content: "b", Confidence: 0.9},
		{Source: "timeline", Content: "c", Confidence: 0.8},
	}
	ranked := ask.RankChunks(chunks, registry, 2)
	require.Len(t, ranked, 2)
}

func TestCollectSources_DedupesByDocumentAndEventID(t *testing.T) {
	chunks := []ask.KnowledgeChunk{
		{DocumentID: "doc1", Title: "T1", Content: "a"},
		{DocumentID: "doc1", Title: "T1", Content: "b"}, // same document, different chunk
		{EventID: "evt1", Title: "T2", Content: "c"},
		{Content: "no source"}, // must be excluded
	}
	sources := ask.CollectSources(chunks)
	require.Len(t, sources, 2)
}

func TestCollectSources_TruncatesLongExcerpt(t *testing.T) {
	longContent := ""
	for i := 0; i < 300; i++ {
		longContent += "x"
	}
	sources := ask.CollectSources([]ask.KnowledgeChunk{{DocumentID: "doc1", Content: longContent}})
	require.Len(t, sources, 1)
	require.LessOrEqual(t, len(sources[0].Excerpt), 203)
}

type fakeProvider struct{ name string }

func (f fakeProvider) Metadata() ask.ProviderMetadata { return ask.ProviderMetadata{Name: f.name} }
func (f fakeProvider) Collect(context.Context, ask.KnowledgeRequest) ([]ask.KnowledgeChunk, error) {
	return nil, nil
}
