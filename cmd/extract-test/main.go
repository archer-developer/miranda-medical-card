// Command extract-test is a throwaway harness for validating the Structured
// Extraction prompt (internal/extraction) against real documents before
// that package gets wired into the real Pipeline. Not part of the eventual
// service — delete once the prompt/schema are validated and the real
// Pipeline/CLI exist to replace it.
//
// Usage:
//
//	go run ./cmd/extract-test path/to/document.jpg [path/to/another.png ...]
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/archer-developer/miranda-llm/gemini"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
)

// ocrModel is deliberately a cheaper/higher-quota model than
// structuredModel — OCR (Stage 1) is "just" transcription, and splitting
// models this way spends the scarcer, stronger model's daily quota only on
// the stage that actually needs its reasoning (Stage 2).
const (
	ocrModel        = "gemini-3.5-flash-lite"
	structuredModel = "gemini-3.6-flash"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: extract-test path/to/document.jpg [more files...]")
	}

	if err := loadDotEnv(".env"); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	apiKeyEnvs := []string{"GEMINI_API_KEY_1", "GEMINI_API_KEY_2", "GEMINI_API_KEY_3"}
	rotation := gemini.RotationConfig{CooldownSeconds: 10, MaxRetryCycles: 1}

	ocrProvider, err := gemini.New(ctx, "gemini-ocr", ocrModel, apiKeyEnvs, gemini.ToolsConfig{}, rotation, logger)
	if err != nil {
		return fmt.Errorf("build gemini OCR provider: %w", err)
	}
	structuredProvider, err := gemini.New(ctx, "gemini-structured", structuredModel, apiKeyEnvs, gemini.ToolsConfig{}, rotation, logger)
	if err != nil {
		return fmt.Errorf("build gemini structured provider: %w", err)
	}

	for _, path := range os.Args[1:] {
		if err := extractOne(ctx, ocrProvider, structuredProvider, path, logger); err != nil {
			fmt.Fprintf(os.Stderr, "--- %s: FAILED: %v\n\n", path, err)
			continue
		}
	}
	return nil
}

func extractOne(ctx context.Context, ocrProvider *gemini.Provider, structuredProvider *gemini.Provider, path string, logger *slog.Logger) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	// A .txt input is treated as already-transcribed text (e.g. a saved
	// fixture, see internal/extraction/testdata) — skips Stage 1 (OCR)
	// entirely and goes straight to Structured, so re-validating the
	// Stage 2 prompt against known-good text doesn't cost an OCR call or
	// require the original image.
	var text string
	if strings.ToLower(filepath.Ext(path)) == ".txt" {
		text = string(data)
		fmt.Fprintf(os.Stderr, "--- using %s as already-transcribed text (%d chars), skipping OCR ---\n\n", path, len(text))
	} else {
		mimeType := mimeTypeFor(path, data)
		imageBase64 := base64.StdEncoding.EncodeToString(data)

		fmt.Fprintf(os.Stderr, "--- extracting %s (%s, %d bytes) ---\n", path, mimeType, len(data))

		text, err = extraction.OCR(ctx, ocrProvider, imageBase64, mimeType)
		if err != nil {
			return fmt.Errorf("ocr: %w", err)
		}
		fmt.Fprintf(os.Stderr, "--- stage 1 (OCR, %s) text (%d chars) ---\n%s\n\n", ocrModel, len(text), text)
	}

	result, _, err := extraction.StructuredWithRetry(ctx, structuredProvider, text, logger)
	if err != nil {
		return fmt.Errorf("structured: %w", err)
	}

	findings, findingsRaw, err := extraction.InstrumentalStructuredWithRetry(ctx, structuredProvider, text, result.DocumentType == "imaging_report", logger)
	if err != nil {
		return fmt.Errorf("instrumental structured: %w", err)
	}
	result.InstrumentalFindings = findings
	fmt.Fprintf(os.Stderr, "--- stage 2b (instrumental findings) raw ---\n%s\n\n", string(findingsRaw))

	merged, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged result: %w", err)
	}

	fmt.Printf("=== %s ===\n%s\n\n", path, string(merged))
	fmt.Fprintf(os.Stderr, "summary: type=%s date=%s diagnoses=%d medications=%d labResults=%d instrumentalFindings=%d procedures=%d allergies=%d vitalSigns=%d recommendations=%d\n\n",
		result.DocumentType, result.DocumentDate, len(result.Diagnoses), len(result.Medications),
		len(result.LabResults), len(result.InstrumentalFindings), len(result.Procedures), len(result.Allergies), len(result.VitalSigns), len(result.Recommendations))
	return nil
}

// mimeTypeFor prefers the file extension (more reliable for
// JPEG-vs-generic-binary edge cases in http.DetectContentType) and falls
// back to content sniffing.
func mimeTypeFor(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".heic":
		return "image/heic"
	case ".pdf":
		return "application/pdf"
	}
	return http.DetectContentType(data)
}

// loadDotEnv is a minimal, throwaway .env loader for this harness only —
// real services in this family use internal/envfile (miranda-service-skeleton),
// not duplicated here since this command is deleted once the prompt is
// validated. Never overwrites a variable already set in the real
// environment, same convention as envfile.Load.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}
