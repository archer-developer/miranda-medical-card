// Package search implements the in-memory ranking half of Embedding Search
// (docs/architecture/04-search.md §14) — cosine similarity over a user's
// embeddings, loaded via storage.EmbeddingRepository.ListByUser. Kept
// separate from storage (which only stores/retrieves vectors, no ranking
// logic — see storage.EmbeddingRepository's doc comment) and from Knowledge
// Providers (which decide *when* to call this, per
// docs/architecture/03-knowledge-providers.md §10 "Provider самостоятельно
// принимает решение... использовать Embeddings").
package search

import (
	"math"
	"sort"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// Scored is one embedding ranked against a query vector.
type Scored struct {
	SourceType string
	SourceID   string
	Score      float64
}

// CosineSearch ranks embeddings by cosine similarity to query, highest
// first, capped to limit. Mirrors miranda-diary's Store.Search ranking
// exactly (see that package's cosineSimilarity) — the same personal-scale
// in-memory approach is more than adequate here too (see
// docs/domain/10-medical-knowledge-and-embedding.md §3's Repository doc
// comment: "поиск... всегда выполняется в памяти по эмбеддингам одного
// userId").
func CosineSearch(query []float32, embeddings []storage.Embedding, limit int) []Scored {
	result := make([]Scored, 0, len(embeddings))
	for _, e := range embeddings {
		result = append(result, Scored{
			SourceType: e.SourceType, SourceID: e.SourceID,
			Score: cosineSimilarity(query, e.Vector),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

// cosineSimilarity returns the cosine similarity between two float32
// vectors, 0 for mismatched or zero-length/zero-norm vectors (avoids NaN
// from division by zero) — identical formula to miranda-diary's
// cosineSimilarity.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
