package ask

import (
	"sort"
	"strings"
)

// providerDisplayNames labels each provider's section in the built prompt
// — a human-readable header, not the internal provider name (matching
// docs/architecture/03-knowledge-providers.md §13's example: "Timeline",
// "Lab Results", not "timeline"/"lab_results").
var providerDisplayNames = map[string]string{
	"timeline":              "Timeline",
	"medications":           "Medications",
	"diagnoses":             "Diagnoses",
	"procedures":            "Procedures",
	"lab_results":           "Lab Results",
	"instrumental_findings": "Instrumental Findings",
	"profile":               "Medical Profile",
	"documents":             "Documents",
	"embeddings":            "Related (semantic search)",
}

// RankChunks implements the Context Builder's merge/dedupe/rank step
// (docs/architecture/03-knowledge-providers.md §13): drops exact-duplicate
// content, ranks by Confidence (ties broken by Provider priority —
// registry's registration order, see docs/architecture/03-knowledge-providers.md
// §17), and caps to maxChunks. Exposed separately from RenderContext so a
// caller (Asker) can build docs/mcp/04-medical.md §5's `sources` from
// exactly the chunk set that ended up in the prompt, not the raw
// pre-dedup/pre-cap set every Provider originally returned.
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

// RenderContext renders ranked (as returned by RankChunks) into the
// documented QUESTION/section-separator prompt shape
// (docs/architecture/03-knowledge-providers.md §13's example). Pure
// string formatting — no ranking decisions of its own.
func RenderContext(question string, ranked []KnowledgeChunk) string {
	var b strings.Builder
	b.WriteString("QUESTION\n\n")
	b.WriteString(question)

	for _, group := range groupBySource(ranked) {
		b.WriteString("\n\n========================\n\n")
		name := providerDisplayNames[group.source]
		if name == "" {
			name = group.source
		}
		b.WriteString(name)
		b.WriteString("\n\n")
		for i, c := range group.chunks {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(c.Content)
		}
	}

	return b.String()
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

type sourceGroup struct {
	source string
	chunks []KnowledgeChunk
}

// groupBySource groups chunks by Source, preserving each group's first
// appearance position in chunks (i.e. the overall Confidence/priority
// ranking RankChunks already applied) rather than re-sorting groups
// alphabetically.
func groupBySource(chunks []KnowledgeChunk) []sourceGroup {
	var groups []sourceGroup
	index := make(map[string]int)
	for _, c := range chunks {
		i, ok := index[c.Source]
		if !ok {
			i = len(groups)
			index[c.Source] = i
			groups = append(groups, sourceGroup{source: c.Source})
		}
		groups[i].chunks = append(groups[i].chunks, c)
	}
	return groups
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
// chunks (as returned by RankChunks — pass the same slice used to build the
// context, so sources exactly match what the model actually saw).
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
