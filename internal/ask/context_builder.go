package ask

import (
	"sort"
)

// RankChunks implements the Context Builder's merge/dedupe/rank step
// (docs/architecture/03-knowledge-providers.md §13): drops exact-duplicate
// content, ranks by Confidence (ties broken by Provider priority —
// registry's registration order, see docs/architecture/03-knowledge-providers.md
// §17), and caps to maxChunks (a maxChunks of 0 disables the cap, dedupe/
// sort only). Used twice per agent loop turn (see agent_loop.go): once per
// tool call, capping what that single call shows the model
// (executeToolCall), and once more at the very end with an unbounded
// maxChunks, purely to dedupe/sort the whole turn's accumulated chunks
// before CollectSources builds docs/mcp/04-medical.md §5's `sources` from
// them — the end-of-turn pass must never re-truncate, or `sources` could
// silently omit a chunk the model was actually shown.
func RankChunks(chunks []KnowledgeChunk, registry *Registry, maxChunks int) []KnowledgeChunk {
	deduped := dedupeChunks(chunks)
	sort.SliceStable(deduped, func(i, j int) bool {
		if deduped[i].Confidence != deduped[j].Confidence {
			return deduped[i].Confidence > deduped[j].Confidence
		}
		return registry.PriorityIndex(deduped[i].Source) < registry.PriorityIndex(deduped[j].Source)
	})
	if maxChunks > 0 && len(deduped) > maxChunks {
		deduped = deduped[:maxChunks]
	}
	return deduped
}

func dedupeChunks(chunks []KnowledgeChunk) []KnowledgeChunk {
	seen := make(map[string]bool, len(chunks))
	result := make([]KnowledgeChunk, 0, len(chunks))
	for _, c := range chunks {
		if c.Content == "" || seen[c.Content] {
			continue
		}
		seen[c.Content] = true
		result = append(result, c)
	}
	return result
}

// CollectedSources returns the docs/mcp/04-medical.md §5 `sources` list for
// chunks — one entry per chunk carrying a DocumentID or EventID, since a
// chunk with neither (e.g. a Profile aggregate with no single source
// document) isn't attributable to one specific record.
type Source struct {
	DocumentID string
	EventID    string
	Title      string
	Excerpt    string
}

// CollectSources builds docs/mcp/04-medical.md §5's `sources` from ranked
// chunks (as returned by RankChunks's end-of-turn, unbounded-maxChunks
// pass — see that function's own doc comment). Because each tool call's
// own contribution was already capped before ever reaching the model (see
// agent_loop.go's executeToolCall), and this final pass never re-caps,
// sources built here are guaranteed to be a subset of exactly what the
// model actually saw, never missing something it may have cited.
func CollectSources(chunks []KnowledgeChunk) []Source {
	seen := make(map[string]bool, len(chunks))
	var result []Source
	for _, c := range chunks {
		if c.DocumentID == "" && c.EventID == "" {
			continue
		}
		key := c.DocumentID + "|" + c.EventID
		if seen[key] {
			continue
		}
		seen[key] = true
		excerpt := c.Content
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "..."
		}
		result = append(result, Source{DocumentID: c.DocumentID, EventID: c.EventID, Title: c.Title, Excerpt: excerpt})
	}
	return result
}
