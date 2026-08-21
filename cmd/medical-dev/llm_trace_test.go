package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The actual parsing/analysis logic (block reassembly, provider-shape
// decoding, conversation grouping, rendering) is tested in
// miranda-llm/llmtrace/analyze — this file only wires flags to that
// package, so these tests just cover the wiring itself.

func TestRunLLMTrace_MissingFile(t *testing.T) {
	err := runLLMTrace([]string{"--file", filepath.Join(t.TempDir(), "nope.log")})
	require.Error(t, err)
}

func TestRunLLMTrace_SummaryAndConversationAndLatestAndUntagged(t *testing.T) {
	log := `=== 2026-08-12T18:14:45Z provider=gemini-agent conversation=session_1 ===
--- request ---
{"contents":[{"role":"user","parts":[{"text":"Вопрос"}]}]}
--- response ---
{"text":"Ответ.","tool_calls":null}

=== 2026-08-12T18:20:00Z provider=gemini-agent ===
--- request ---
{"contents":[{"role":"user","parts":[{"text":"Второй вопрос без sessionId"}]}]}
--- response ---
{"text":"Ответ.","tool_calls":null}

`
	path := filepath.Join(t.TempDir(), "llm.log")
	require.NoError(t, os.WriteFile(path, []byte(log), 0o644))

	require.NoError(t, runLLMTrace([]string{"--file", path}))
	require.NoError(t, runLLMTrace([]string{"--file", path, "--conversation", "session_1"}))
	require.NoError(t, runLLMTrace([]string{"--file", path, "--latest"}))
	require.NoError(t, runLLMTrace([]string{"--file", path, "--untagged"}))

	require.Error(t, runLLMTrace([]string{"--file", path, "--conversation", "does-not-exist"}))
}
