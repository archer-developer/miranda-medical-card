package ask

import (
	"fmt"
	"strings"
)

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
func formatToolResult(chunks []KnowledgeChunk) string {
	if len(chunks) == 0 {
		return "No matching data found."
	}
	lines := make([]string, len(chunks))
	for i, c := range chunks {
		line := "- " + c.Content
		if c.DocumentID != "" {
			line += fmt.Sprintf(" [id: %s]", c.DocumentID)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
