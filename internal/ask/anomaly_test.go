package ask

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// geminiUnknownToolTrace mirrors a real gemini.Provider trace for a turn
// where the model called a tool that doesn't exist — the same shape
// analyze/describe.go's DescribeIncoming/ExtractToolCalls decode in
// production. Kept minimal (just the fields those decoders read) rather
// than pulling in a JSON fixture file, since this is the only place these
// tests need it.
const (
	geminiQuestionRequest   = `{"contents":[{"role":"user","parts":[{"text":"how am I doing"}]}]}`
	geminiToolCallResponse  = `{"text":"","tool_calls":[{"Name":"no_such_tool","Arguments":"{}"}]}`
	geminiToolResultRequest = `{"contents":[` +
		`{"role":"user","parts":[{"text":"how am I doing"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"no_such_tool","args":{}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"no_such_tool","response":{"result":"error: unknown tool \"no_such_tool\""}}}]}` +
		`]}`
	geminiFinalAnswerResponse = `{"text":"sorry, I could not find that"}`
)

func newTestAsker(t *testing.T, logger *slog.Logger) *Asker {
	t.Helper()
	return NewAsker(nil, NewRegistry(), NewSessionStore(nil), 0, 20, 16, logger)
}

func TestReportAnomalies_NilRecorderIsNoOp(t *testing.T) {
	var logBuf bytes.Buffer
	a := newTestAsker(t, slog.New(slog.NewTextHandler(&logBuf, nil)))
	a.SetAnomalyConfig(AnomalyConfig{Dir: t.TempDir()})

	require.NotPanics(t, func() {
		a.reportAnomalies("sess-1", nil, anomaly.Outcome{})
	})
	require.Empty(t, logBuf.String())
}

func TestReportAnomalies_NoAnomaliesIsNoOp(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	a := newTestAsker(t, slog.New(slog.NewTextHandler(&logBuf, nil)))
	a.SetAnomalyConfig(AnomalyConfig{Dir: dir})

	recorder := anomaly.NewRecorder("sess-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiFinalAnswerResponse, nil)

	a.reportAnomalies("sess-1", recorder, anomaly.Outcome{IterationCount: 1, MaxIterations: 16})

	require.Empty(t, logBuf.String())
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "no anomalies dir should be created when nothing was found")
}

func TestReportAnomalies_WritesFileAndWarnsOnAnomaly(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	a := newTestAsker(t, slog.New(slog.NewTextHandler(&logBuf, nil)))
	a.SetAnomalyConfig(AnomalyConfig{Dir: dir})

	recorder := anomaly.NewRecorder("sess-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiToolCallResponse, nil)
	recorder.Trace(context.Background(), "gemini", geminiToolResultRequest, geminiFinalAnswerResponse, nil)

	a.reportAnomalies("sess-1", recorder, anomaly.Outcome{IterationCount: 2, MaxIterations: 16})

	require.Contains(t, logBuf.String(), "turn had anomalies")
	require.Contains(t, logBuf.String(), "unknown_tool")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "unknown_tool")

	content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	require.Contains(t, string(content), "no_such_tool")
}

func TestReportAnomalies_FallsBackToTurnBlocksWhenLLMLogUnreadable(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	a := newTestAsker(t, slog.New(slog.NewTextHandler(&logBuf, nil)))
	// LLMLogPath deliberately points at a file that doesn't exist — the
	// whole-conversation re-read must fail gracefully and fall back to just
	// this turn's own blocks, not lose the anomaly file entirely.
	a.SetAnomalyConfig(AnomalyConfig{Dir: dir, LLMLogPath: filepath.Join(t.TempDir(), "missing-llm.log")})

	recorder := anomaly.NewRecorder("sess-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiToolCallResponse, nil)
	recorder.Trace(context.Background(), "gemini", geminiToolResultRequest, geminiFinalAnswerResponse, nil)

	a.reportAnomalies("sess-1", recorder, anomaly.Outcome{IterationCount: 2, MaxIterations: 16})

	require.Contains(t, logBuf.String(), "re-reading llm.log for anomaly context failed")
	require.Contains(t, logBuf.String(), "turn had anomalies")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the anomaly file must still be written from the turn's own blocks")
}
