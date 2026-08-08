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
	"bytes"
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

const model = "gemini-3.6-flash"

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

	provider, err := gemini.New(ctx, "gemini-extract", model,
		[]string{"GEMINI_API_KEY_1", "GEMINI_API_KEY_2", "GEMINI_API_KEY_3"},
		gemini.ToolsConfig{},
		gemini.RotationConfig{CooldownSeconds: 10, MaxRetryCycles: 1},
		logger,
	)
	if err != nil {
		return fmt.Errorf("build gemini provider: %w", err)
	}

	for _, path := range os.Args[1:] {
		if err := extractOne(ctx, provider, path); err != nil {
			fmt.Fprintf(os.Stderr, "--- %s: FAILED: %v\n\n", path, err)
			continue
		}
	}
	return nil
}

func extractOne(ctx context.Context, provider *gemini.Provider, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	mimeType := mimeTypeFor(path, data)
	imageBase64 := base64.StdEncoding.EncodeToString(data)

	fmt.Fprintf(os.Stderr, "--- extracting %s (%s, %d bytes) ---\n", path, mimeType, len(data))

	result, raw, err := extraction.Extract(ctx, provider, imageBase64, mimeType)
	if err != nil {
		return err
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return fmt.Errorf("indent result: %w", err)
	}

	fmt.Printf("=== %s ===\n%s\n\n", path, pretty.String())
	fmt.Fprintf(os.Stderr, "summary: type=%s date=%s diagnoses=%d medications=%d labResults=%d procedures=%d allergies=%d vitalSigns=%d recommendations=%d\n\n",
		result.DocumentType, result.DocumentDate, len(result.Diagnoses), len(result.Medications),
		len(result.LabResults), len(result.Procedures), len(result.Allergies), len(result.VitalSigns), len(result.Recommendations))
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
