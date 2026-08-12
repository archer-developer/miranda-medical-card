package ask

import "strings"

// formatToolResult renders one tool call's returned chunks into the text
// that becomes that call's RoleTool message Content — scoped to exactly
// what this one call returned, unlike the old RenderContext's single
// combined block (removed along with the one-shot pipeline it served):
// each tool call is now its own turn in an open conversation, so there's no
// upfront "everything gathered so far" block to build. Cross-call
// dedup/ranking happens once, at the end of the loop, via RankChunks over
// every chunk the whole turn accumulated.
func formatToolResult(chunks []KnowledgeChunk) string {
	if len(chunks) == 0 {
		return "No matching data found."
	}
	lines := make([]string, len(chunks))
	for i, c := range chunks {
		lines[i] = "- " + c.Content
	}
	return strings.Join(lines, "\n")
}
