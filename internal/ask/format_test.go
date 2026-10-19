package ask

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatToolResult_NoChunks(t *testing.T) {
	require.Equal(t, "No matching data found.", formatToolResult(nil))
}

func TestFormatToolResult_IncludesDocumentID(t *testing.T) {
	out := formatToolResult([]KnowledgeChunk{{Content: "ALT 54.7 (2025-03-14).", DocumentID: "doc1"}})
	require.Equal(t, "- ALT 54.7 (2025-03-14). [id: doc1]", out)
}

// TestFormatToolResult_TruncatesWhenExceedingByteBudget is the regression
// test for the fix alongside LabProvider's own limitOrDefault fix: even
// once every provider caps its chunk *count*, a large enough number of
// small-but-numerous chunks (or a handful of verbose ones) can still add up
// to a tool-result payload big enough, compounded across a multi-turn
// conversation, to make the agent_provider return an empty completion — see
// maxToolResultBytes's own doc comment.
func TestFormatToolResult_TruncatesWhenExceedingByteBudget(t *testing.T) {
	var chunks []KnowledgeChunk
	for i := 0; i < 600; i++ {
		chunks = append(chunks, KnowledgeChunk{Content: "ALT 54.7 U/L, норма 10-40 U/L (2025-03-14)."})
	}

	out := formatToolResult(chunks)

	require.LessOrEqual(t, len(out), maxToolResultBytes+300, "output must stay within the byte budget plus room for the omission note")
	require.Contains(t, out, "more result(s) omitted for size")
	require.True(t, strings.HasPrefix(out, "- ALT"), "must keep the earliest chunks, not drop everything")
}

// TestFormatToolResult_KeepsFirstChunkEvenIfOversizedAlone guards the "never
// silently empty a result" fallback: a single chunk larger than the whole
// budget must still be returned in full, not dropped or cut off mid-line.
func TestFormatToolResult_KeepsFirstChunkEvenIfOversizedAlone(t *testing.T) {
	huge := strings.Repeat("x", maxToolResultBytes*2)

	out := formatToolResult([]KnowledgeChunk{{Content: huge}})

	require.Equal(t, "- "+huge, out)
}

// TestFormatToolResult_OmitsLaterChunksWhenFirstAloneExceedsBudget checks
// the two-chunk edge case: once the first chunk alone already blows the
// budget, the second must be reported as omitted rather than silently
// appended (which would defeat the cap) or silently dropped with no trace
// (which would look like the tool returned less data than it actually
// found).
func TestFormatToolResult_OmitsLaterChunksWhenFirstAloneExceedsBudget(t *testing.T) {
	huge := strings.Repeat("x", maxToolResultBytes*2)

	out := formatToolResult([]KnowledgeChunk{{Content: huge}, {Content: "second"}})

	require.True(t, strings.HasPrefix(out, "- "+huge))
	require.Contains(t, out, "1 more result(s) omitted for size")
	require.NotContains(t, out, "second")
}

// TestFormatToolResultOmitting_ReportsCapDroppedChunks: chunks a caller's
// per-call cap already dropped must still be announced to the model — a
// silently truncated prefix reads as a complete answer.
func TestFormatToolResultOmitting_ReportsCapDroppedChunks(t *testing.T) {
	out := formatToolResultOmitting([]KnowledgeChunk{{Content: "a"}, {Content: "b"}}, 7)
	require.Contains(t, out, "7 more result(s) omitted")

	require.NotContains(t, formatToolResultOmitting([]KnowledgeChunk{{Content: "a"}}, 0), "omitted")
}

// A 44-row lab report with per-line document ids (Cyrillic = 2 bytes/char)
// must fit the byte budget whole — the regression behind raising
// maxToolResultBytes.
func TestFormatToolResult_FitsFullSmearReport(t *testing.T) {
	var chunks []KnowledgeChunk
	for i := 0; i < 44; i++ {
		chunks = append(chunks, KnowledgeChunk{
			Content:    "Задний свод влагалища. Внутриклеточные и внеклеточные Гр(-) диплококки: не обнаружено (2026-10-17).",
			DocumentID: "doc_f93575ab-ffb6-4984-9f6e-1d1c5d847b1d",
		})
	}
	require.NotContains(t, formatToolResult(chunks), "omitted")
}
