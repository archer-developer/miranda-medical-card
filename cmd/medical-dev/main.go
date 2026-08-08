// Command medical-dev implements a scoped subset of `medical dev`
// (docs/cli/medical_dev.md) — developer diagnostic tools that use the same
// Application Services as the MCP server (internal/pipeline.Pipeline,
// internal/ask.Asker), never internal/storage directly, per that doc's §2
// ("использовать те же Application Services, что и MCP API... не
// обращаться напрямую к Repository").
//
// Implemented: profile, timeline, document, ask (the four read-oriented
// commands directly useful for inspecting a running deployment's data).
// Not implemented: planner, provider, search, prompt, pipeline, llm (see
// docs/cli/medical_dev.md §5-8, §12-13) — each would need its own
// intermediate-result plumbing (e.g. exposing the Planner's raw selections,
// or a single Provider's raw output, independent of a full Ask) that
// internal/ask doesn't expose today. Also not implemented: the separate,
// undocumented-in-detail `medical` administrative CLI (docs/cli/medical.md
// §4's documents/embeddings/timeline/database/maintenance/doctor/users/
// statistics categories) — those category docs (02-documents.md etc.)
// don't exist yet in docs/cli/, so there's no concrete spec to implement
// against.
//
// Usage:
//
//	medical-dev profile --user alex
//	medical-dev timeline --user alex [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--type TYPE]
//	medical-dev document <documentId> --user alex
//	medical-dev ask --user alex "question"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/archer-developer/miranda-llm/embedding"
	"github.com/archer-developer/miranda-llm/gemini"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/envfile"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: medical-dev <profile|timeline|document|ask> [flags]")
	}
	command, args := args[0], args[1:]

	_ = envfile.Load(".env")
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.Database.Path, err)
	}
	defer func() { _ = store.Close() }()

	switch command {
	case "profile":
		return runProfile(args, cfg, store)
	case "timeline":
		return runTimeline(args, cfg, store)
	case "document":
		return runDocument(args, cfg, store)
	case "ask":
		return runAsk(args, cfg, store, logger)
	default:
		return fmt.Errorf("unknown command %q — expected profile, timeline, document, or ask", command)
	}
}

// loadConfig mirrors cmd/miranda-medical-card/main.go's configFilePaths +
// config.Load, minus the logging around it — a CLI diagnostic tool should
// stay quiet unless something goes wrong.
func loadConfig() (config.Config, error) {
	dir := "config"
	if v := os.Getenv("MEDICAL_CARD_CONFIG_DIR"); v != "" {
		dir = v
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return config.Config{}, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return config.Load(paths...)
}

func newPipeline(cfg config.Config, store *storage.Store) (*pipeline.Pipeline, error) {
	ctx := context.Background()
	provider, err := gemini.New(ctx, "gemini-document", cfg.LLM.DocumentModel, cfg.LLM.APIKeyEnvs, gemini.ToolsConfig{}, gemini.RotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1}, slog.Default())
	if err != nil {
		return nil, err
	}
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return nil, err
	}
	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return nil, err
	}
	return pipeline.New(provider, embedder, "gemini", cfg.Embedding.Model, files, store, slog.Default()), nil
}

func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// --- profile ---

func runProfile(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	pl, err := newPipeline(cfg, store)
	if err != nil {
		return err
	}
	built, err := pl.GetProfile(context.Background(), *user)
	if err != nil {
		return err
	}
	return printJSON(built)
}

// --- timeline ---

func runTimeline(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("timeline", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	from := fs.String("from", "", "YYYY-MM-DD")
	to := fs.String("to", "", "YYYY-MM-DD")
	eventType := fs.String("type", "", "event type filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	filter := storage.TimelineFilter{}
	if *from != "" {
		t, err := time.Parse("2006-01-02", *from)
		if err != nil {
			return fmt.Errorf("--from: %w", err)
		}
		filter.From = &t
	}
	if *to != "" {
		t, err := time.Parse("2006-01-02", *to)
		if err != nil {
			return fmt.Errorf("--to: %w", err)
		}
		filter.To = &t
	}
	if *eventType != "" {
		filter.Types = []string{*eventType}
	}

	pl, err := newPipeline(cfg, store)
	if err != nil {
		return err
	}
	events, err := pl.GetTimeline(context.Background(), *user, filter)
	if err != nil {
		return err
	}
	return printJSON(events)
}

// --- document ---

func runDocument(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("document", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: medical-dev document <documentId> --user <user>")
	}
	documentID := fs.Arg(0)
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	pl, err := newPipeline(cfg, store)
	if err != nil {
		return err
	}
	doc, err := pl.GetDocument(context.Background(), *user, documentID)
	if err != nil {
		return err
	}
	return printJSON(doc)
}

// --- ask ---

func runAsk(args []string, cfg config.Config, store *storage.Store, logger *slog.Logger) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: medical-dev ask --user <user> \"question\"")
	}
	question := strings.Join(fs.Args(), " ")
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	ctx := context.Background()
	plannerProvider, err := gemini.New(ctx, "gemini-planner", cfg.LLM.PlannerModel, cfg.LLM.APIKeyEnvs, gemini.ToolsConfig{}, gemini.RotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1}, logger)
	if err != nil {
		return err
	}
	answerProvider, err := gemini.New(ctx, "gemini-answer", cfg.LLM.AnswerModel, cfg.LLM.APIKeyEnvs, gemini.ToolsConfig{}, gemini.RotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1}, logger)
	if err != nil {
		return err
	}
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return err
	}

	registry := ask.NewRegistry(
		ask.NewTimelineProvider(storage.NewTimelineRepository(store)),
		ask.NewMedicationProvider(storage.NewMedicationRepository(store)),
		ask.NewDiagnosisProvider(storage.NewDiagnosisRepository(store)),
		ask.NewLabProvider(storage.NewLabResultRepository(store)),
		ask.NewInstrumentalFindingProvider(storage.NewInstrumentalFindingRepository(store)),
		ask.NewProcedureProvider(storage.NewProcedureRepository(store)),
		ask.NewDocumentProvider(storage.NewFTSRepository(store)),
		ask.NewEmbeddingProvider(storage.NewEmbeddingRepository(store), storage.NewDocumentRepository(store), storage.NewSelfReportedEventRepository(store), embedder, cfg.Embedding.Model),
	)
	asker := ask.NewAsker(plannerProvider, answerProvider, registry, 20*time.Second, 20, logger)

	fmt.Println("Question")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println()
	fmt.Println(question)
	fmt.Println()

	result, err := asker.Ask(ctx, *user, question)
	if err != nil {
		return err
	}

	fmt.Println("Answer")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println()
	fmt.Println(result.Answer)
	fmt.Println()

	if len(result.Sources) > 0 {
		fmt.Println("Sources")
		fmt.Println(strings.Repeat("-", 40))
		fmt.Println()
		for _, s := range result.Sources {
			fmt.Printf("- %s (documentId=%s eventId=%s)\n", s.Title, s.DocumentID, s.EventID)
		}
	}
	return nil
}
