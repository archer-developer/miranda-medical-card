package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTeeHandler_WarnReachesBothStdoutAndFile reproduces a production bug
// (2026-08-27): the old levelSplitHandler routed each record to exactly one
// destination by level (DEBUG => file only, INFO+ => stdout only), so a
// WARN/ERROR — e.g. an OCR provider failing over, or "upload_document
// failed" — never appeared in the app log file at all, only in the
// stdout/journal stream. Debugging a stalled request by reading the file
// alone gave no indication anything had gone wrong. teeHandler fans every
// record out to both handlers (each still gated by its own configured
// level), so the file ends up a superset of what stdout sees, not a
// level-exclusive subset of it — matching miranda's own
// cmd/miranda/main.go, which writes one io.MultiWriter'd handler instead of
// splitting by level.
func TestTeeHandler_WarnReachesBothStdoutAndFile(t *testing.T) {
	var stdoutBuf, fileBuf bytes.Buffer
	stdoutHandler := slog.NewTextHandler(&stdoutBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	fileHandler := slog.NewTextHandler(&fileBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(&teeHandler{stdout: stdoutHandler, file: fileHandler})

	logger.Debug("pipeline: process start", "documentId", "doc_1")
	logger.Warn("extraction: ocr failed, escalating to a different provider", "error", "503")

	fileOut := fileBuf.String()
	require.Contains(t, fileOut, "process start", "the file must keep receiving DEBUG-level trace lines")
	require.Contains(t, fileOut, "ocr failed", "the file must also receive WARN/ERROR lines, not just DEBUG ones")

	stdoutOut := stdoutBuf.String()
	require.NotContains(t, stdoutOut, "process start", "stdout stays at its own Info+ floor regardless of the file's level")
	require.Contains(t, stdoutOut, "ocr failed")
}

// TestTeeHandler_WithAttrsPropagatesToBothDestinations guards against a
// naive implementation that forwards WithAttrs/WithGroup to only one of the
// two wrapped handlers, which would silently drop caller-supplied
// attributes (e.g. a documentId added via logger.With(...)) from whichever
// destination got skipped.
func TestTeeHandler_WithAttrsPropagatesToBothDestinations(t *testing.T) {
	var stdoutBuf, fileBuf bytes.Buffer
	stdoutHandler := slog.NewTextHandler(&stdoutBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	fileHandler := slog.NewTextHandler(&fileBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(&teeHandler{stdout: stdoutHandler, file: fileHandler})

	logger.With("documentId", "doc_42").Warn("pipeline: process done")

	require.True(t, strings.Contains(fileBuf.String(), "documentId=doc_42"))
	require.True(t, strings.Contains(stdoutBuf.String(), "documentId=doc_42"))
}
