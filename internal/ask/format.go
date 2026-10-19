package ask

import (
	"fmt"
	"strings"
)

// maxToolResultBytes hard-caps how large a single formatToolResult text can
// grow. Each provider's own Collect call is already capped to maxChunks
// entries before it ever reaches here (see agent_loop.go's
// executeToolCall's RankChunks call), but that's a chunk-count cap, not a
// byte cap — a handful of verbose chunks (long document excerpts, a wide
// lab panel with reference ranges) can still add up. Every RoleTool message
// this produces stays in the conversation for the rest of the turn, so a
// few oversized results compound across iterations into a request payload
// large enough to make the agent_provider (Gemini, in production —
// see llm.agent_provider) return an empty completion instead of an error —
// indistinguishable, from here, from "no answer". This is a mechanical
// safety net independent of any provider-level limit, not a substitute for
// one (see LabProvider.Collect's own limitOrDefault fix for the actual
// per-provider bug that first surfaced this).
//
// 12000, not the original 4000: Cyrillic text is two bytes per character
// and every line carries its own " [id: doc_...]" suffix (~50 bytes), so a
// single 44-row lab report (three sampling sites x ~15 parameters) needs
// ~7.5KB — at 4000 only ~22 rows survived and the rest were reported as
// "omitted", which the model read as "no such results".
const maxToolResultBytes = 12000

// formatToolResult renders one tool call's returned chunks into the text
// that becomes that call's RoleTool message Content — scoped to exactly
// what this one call returned, unlike the old RenderContext's single
// combined block (removed along with the one-shot pipeline it served):
// each tool call is now its own turn in an open conversation, so there's no
// upfront "everything gathered so far" block to build. Cross-call
// dedup/ranking happens once, at the end of the loop, via RankChunks over
// every chunk the whole turn accumulated.
//
// A chunk's DocumentID, when set, is appended as "[id: ...]" — the only way
// the model ever learns a document/event id well-formed enough to pass back
// into a later documentId-scoped tool call (e.g. lab_results), since
// nothing else in this text surfaces it.
//
// Truncation, when maxToolResultBytes is exceeded, always happens on a
// whole-chunk boundary — the model never sees a fact cut off mid-line — and
// always keeps at least the first chunk, even if that one chunk alone
// exceeds the budget, so a result is never silently emptied outright.
func formatToolResult(chunks []KnowledgeChunk) string {
	return formatToolResultOmitting(chunks, 0)
}

// formatToolResultOmitting is formatToolResult for chunks that a caller has
// already dropped droppedByCap more of (executeToolCall's per-call
// maxChunks cap) — folded into the same trailing "omitted" note as any
// byte-budget truncation, so the model sees one honest total instead of a
// complete-looking prefix.
func formatToolResultOmitting(chunks []KnowledgeChunk, droppedByCap int) string {
	if len(chunks) == 0 {
		return "No matching data found."
	}
	lines := make([]string, 0, len(chunks))
	size := 0
	omitted := droppedByCap
	for i, c := range chunks {
		line := "- " + c.Content
		if c.DocumentID != "" {
			line += fmt.Sprintf(" [id: %s]", c.DocumentID)
		}
		if len(lines) > 0 && size+len(line)+1 > maxToolResultBytes {
			omitted += len(chunks) - i
			break
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf(
			"... %d more result(s) omitted for size — narrow your query (date range, specific indicator, or documentId) to see them.",
			omitted))
	}
	return strings.Join(lines, "\n")
}
