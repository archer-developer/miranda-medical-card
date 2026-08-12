package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTrace(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llm.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestParseTraceFile_GeminiShape(t *testing.T) {
	log := `=== 2026-08-12T18:14:45Z provider=gemini-agent conversation=session_1 ===
--- request ---
{"contents":[{"role":"user","parts":[{"text":"Какие анализы за июль?"}]}]}
--- response ---
{"text":"","tool_calls":[{"ID":"c1","Name":"lab_results","Arguments":"{\"from\":\"2026-07-01\"}"}]}

=== 2026-08-12T18:14:47Z provider=gemini-agent conversation=session_1 ===
--- request ---
{"contents":[{"role":"user","parts":[{"text":"Какие анализы за июль?"}]},{"role":"model","parts":[{"functionCall":{"name":"lab_results","args":{"from":"2026-07-01"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lab_results","response":{"result":"- АЛТ: 63 (2026-07-24)."}}}]}]}
--- response ---
{"text":"АЛТ повышен, стоит обсудить с врачом.","tool_calls":null}

`
	path := writeTrace(t, log)

	blocks, err := parseTraceFile(path)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	require.Equal(t, "session_1", blocks[0].Conversation)
	require.Equal(t, "gemini-agent", blocks[0].Provider)

	in, ok := describeIncoming(blocks[0], true)
	require.True(t, ok)
	require.Contains(t, in, "Какие анализы за июль?")

	out := describeOutgoing(blocks[0])
	require.Len(t, out, 1)
	require.Contains(t, out[0], "lab_results")
	require.Contains(t, out[0], "2026-07-01")

	in2, ok := describeIncoming(blocks[1], false)
	require.True(t, ok)
	require.Contains(t, in2, "lab_results ->")
	require.Contains(t, in2, "АЛТ: 63")

	out2 := describeOutgoing(blocks[1])
	require.Len(t, out2, 1)
	require.Contains(t, out2[0], "answer: АЛТ повышен")

	require.False(t, endsOnToolCallWithNoAnswer(blocks[1]), "a final text answer must not be flagged as a cut-off conversation")
	require.True(t, endsOnToolCallWithNoAnswer(blocks[0]), "a tool call with no text must be flagged as a cut-off conversation")
}

func TestParseTraceFile_AnthropicShape(t *testing.T) {
	log := `=== 2026-08-12T18:20:00Z provider=claude-escalation conversation=session_2 ===
--- request ---
{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"Что с холестерином?"}]}]}
--- response ---
{"id":"msg_1","content":[{"type":"tool_use","id":"t1","name":"lab_results","input":{"indicatorName":"холестерин"}}],"stop_reason":"tool_use"}

=== 2026-08-12T18:20:03Z provider=claude-escalation conversation=session_2 ===
--- request ---
{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"Что с холестерином?"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lab_results","input":{"indicatorName":"холестерин"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"- Холестерин: 5.2 (2026-07-24)."}]}]}
--- response ---
{"id":"msg_2","content":[{"type":"text","text":"Холестерин слегка повышен."}],"stop_reason":"end_turn"}

`
	path := writeTrace(t, log)

	blocks, err := parseTraceFile(path)
	require.NoError(t, err)
	require.Len(t, blocks, 2)

	out := describeOutgoing(blocks[0])
	require.Len(t, out, 1)
	require.Contains(t, out[0], "call lab_results")
	require.Contains(t, out[0], "холестерин")
	require.True(t, endsOnToolCallWithNoAnswer(blocks[0]))

	in2, ok := describeIncoming(blocks[1], false)
	require.True(t, ok)
	require.Contains(t, in2, "tool result ->")
	require.Contains(t, in2, "Холестерин: 5.2")

	out2 := describeOutgoing(blocks[1])
	require.Len(t, out2, 1)
	require.Contains(t, out2[0], "answer: Холестерин слегка повышен.")
	require.False(t, endsOnToolCallWithNoAnswer(blocks[1]))
}

func TestParseTraceFile_ErrorBlock(t *testing.T) {
	log := `=== 2026-08-12T18:30:00Z provider=gemini-agent conversation=session_3 ===
--- request ---
{"contents":[{"role":"user","parts":[{"text":"Вопрос"}]}]}
--- response ---
error: gemini: request failed: 429 quota exceeded

`
	path := writeTrace(t, log)

	blocks, err := parseTraceFile(path)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.Equal(t, "gemini: request failed: 429 quota exceeded", blocks[0].ErrorText)
	require.Empty(t, blocks[0].Response)
}

func TestLatestConversation_IgnoresUntaggedBlocks(t *testing.T) {
	log := `=== 2026-08-12T18:00:00Z provider=gemini-document ===
--- request ---
{}
--- response ---
{}

=== 2026-08-12T18:05:00Z provider=gemini-agent conversation=session_older ===
--- request ---
{}
--- response ---
{}

=== 2026-08-12T18:10:00Z provider=gemini-agent conversation=session_newer ===
--- request ---
{}
--- response ---
{}

`
	path := writeTrace(t, log)

	blocks, err := parseTraceFile(path)
	require.NoError(t, err)
	require.Len(t, blocks, 3)
	require.Equal(t, "session_newer", latestConversation(blocks))
}

func TestTruncate_RespectsRuneBoundaries(t *testing.T) {
	s := truncate("Показатели: холестерин, мочевина, креатинин", 15)
	require.LessOrEqual(t, len([]rune(s)), 16) // 15 + the ellipsis rune
	require.Contains(t, s, "…")
}
