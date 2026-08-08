package search_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/search"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestCosineSearch_RanksByHighestSimilarityFirst(t *testing.T) {
	embeddings := []storage.Embedding{
		{SourceType: "summary", SourceID: "doc_far", Vector: []float32{0, 1}},
		{SourceType: "summary", SourceID: "doc_close", Vector: []float32{1, 0.01}},
		{SourceType: "summary", SourceID: "doc_exact", Vector: []float32{1, 0}},
	}
	result := search.CosineSearch([]float32{1, 0}, embeddings, 10)
	require.Len(t, result, 3)
	require.Equal(t, "doc_exact", result[0].SourceID)
	require.Equal(t, "doc_close", result[1].SourceID)
	require.Equal(t, "doc_far", result[2].SourceID)
	require.InDelta(t, 1.0, result[0].Score, 0.0001)
}

func TestCosineSearch_RespectsLimit(t *testing.T) {
	embeddings := []storage.Embedding{
		{SourceID: "a", Vector: []float32{1, 0}},
		{SourceID: "b", Vector: []float32{1, 0}},
		{SourceID: "c", Vector: []float32{1, 0}},
	}
	result := search.CosineSearch([]float32{1, 0}, embeddings, 2)
	require.Len(t, result, 2)
}

func TestCosineSearch_ZeroNormVectorScoresZeroNotNaN(t *testing.T) {
	embeddings := []storage.Embedding{{SourceID: "zero", Vector: []float32{0, 0}}}
	result := search.CosineSearch([]float32{1, 0}, embeddings, 10)
	require.Len(t, result, 1)
	require.Equal(t, 0.0, result[0].Score)
}

func TestCosineSearch_MismatchedDimensionsScoresZero(t *testing.T) {
	embeddings := []storage.Embedding{{SourceID: "mismatched", Vector: []float32{1, 0, 0}}}
	result := search.CosineSearch([]float32{1, 0}, embeddings, 10)
	require.Len(t, result, 1)
	require.Equal(t, 0.0, result[0].Score)
}
