// Command medical-dev implements a scoped subset of `medical dev`
// (docs/cli/medical_dev.md) — developer diagnostic tools that use the same
// Application Services as the MCP server (internal/pipeline.Pipeline,
// internal/ask.Asker), never internal/storage directly, per that doc's §2
// ("использовать те же Application Services, что и MCP API... не
// обращаться напрямую к Repository").
//
// Implemented: profile, timeline, document, ask, pipeline (the read-oriented
// commands directly useful for inspecting a running deployment's data, plus
// pipeline for re-running Processing Pipeline against an already-imported
// document with full Debug-level tracing to stderr — docs/cli/medical_dev.md
// §12).
// Not implemented: planner, provider, search, prompt, llm (see
// docs/cli/medical_dev.md §5-8, §13) — each would need its own
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
//	medical-dev pipeline <documentId> --user alex
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

	"github.com/archer-developer/miranda-llm/anthropic"
	"github.com/archer-developer/miranda-llm/embedding"
	"github.com/archer-developer/miranda-llm/gemini"
	"github.com/archer-developer/miranda-llm/openaicompat"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/envfile"
	"github.com/archer-developer/miranda-medical-card/internal/extraction"
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
	case "pipeline":
		return runPipeline(args, cfg, store)
	default:
		return fmt.Errorf("unknown command %q — expected profile, timeline, document, ask, or pipeline", command)
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
	return newPipelineWithLogger(cfg, store, slog.Default())
}

func newPipelineWithLogger(cfg config.Config, store *storage.Store, logger *slog.Logger) (*pipeline.Pipeline, error) {
	ctx := context.Background()
	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return nil, err
	}
	provider, err := resolveProvider(providers, cfg.LLM.DocumentProvider, "document_provider")
	if err != nil {
		return nil, err
	}
	escalationProvider := resolveEscalationProvider(providers, cfg.LLM.Providers, cfg.LLM.DocumentProvider)
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return nil, err
	}
	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return nil, err
	}
	return pipeline.New(provider, escalationProvider, embedder, "gemini", cfg.Embedding.Model, files, store, logger), nil
}

// buildProviders, firstAPIKey, resolveProvider, and resolveEscalationProvider
// mirror cmd/miranda-medical-card/main.go's helpers of the same name — see
// that copy's doc comments. Duplicated rather than shared since these are
// two separate main packages, same as this file's other construction
// helpers.
func buildProviders(ctx context.Context, configs []config.ProviderConfig, logger *slog.Logger) (map[string]extraction.Provider, error) {
	providers := make(map[string]extraction.Provider, len(configs))
	for _, c := range configs {
		switch c.Type {
		case "gemini":
			p, err := gemini.New(ctx, c.Name, c.Model, c.APIKeyEnvs,
				gemini.ToolsConfig{},
				gemini.RotationConfig{CooldownSeconds: c.GeminiRotation.CooldownSeconds, MaxRetryCycles: c.GeminiRotation.MaxRetryCycles},
				logger,
			)
			if err != nil {
				return nil, fmt.Errorf("build provider %q: %w", c.Name, err)
			}
			providers[c.Name] = p
		case "anthropic":
			apiKey := firstAPIKey(c.APIKeyEnvs)
			if apiKey == "" {
				return nil, fmt.Errorf("build provider %q: environment variable %s (named by api_key_envs[0]) is not set", c.Name, c.APIKeyEnvs[0])
			}
			providers[c.Name] = anthropic.New(c.Name, c.Model, apiKey, anthropic.ToolsConfig{})
		case "openai_compat":
			apiKey := firstAPIKey(c.APIKeyEnvs)
			if apiKey == "" {
				return nil, fmt.Errorf("build provider %q: environment variable %s (named by api_key_envs[0]) is not set", c.Name, c.APIKeyEnvs[0])
			}
			providers[c.Name] = openaicompat.New(c.Name, c.BaseURL, c.Model, apiKey)
		default:
			return nil, fmt.Errorf("build provider %q: unknown type %q", c.Name, c.Type)
		}
	}
	return providers, nil
}

func firstAPIKey(envs []string) string {
	if len(envs) == 0 {
		return ""
	}
	return os.Getenv(envs[0])
}

func resolveProvider(providers map[string]extraction.Provider, name, field string) (extraction.Provider, error) {
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("llm.%s %q does not name a configured provider", field, name)
	}
	return p, nil
}

func resolveEscalationProvider(providers map[string]extraction.Provider, configs []config.ProviderConfig, documentProviderName string) extraction.StructuredProvider {
	for _, c := range configs {
		if c.Name != documentProviderName {
			continue
		}
		if !c.Escalation.Enabled {
			return nil
		}
		return providers[c.Escalation.TargetProvider]
	}
	return nil
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

// --- pipeline ---

// runPipeline re-runs the Processing Pipeline against an already-imported
// document (Pipeline.ReprocessDocument — the same Application Service
// method medical.reprocess_document uses) with a Debug-level logger writing
// straight to stderr, so every Structured Extraction attempt and Pipeline
// stage (see internal/extraction.StructuredWithRetry, internal/pipeline's
// run) is visible immediately — docs/cli/medical_dev.md §12's "подробный
// лог выполнения", without needing to enable server-wide debug logging or
// tail logs/debug.log separately.
func runPipeline(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("pipeline", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: medical-dev pipeline <documentId> --user <user>")
	}
	documentID := fs.Arg(0)
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	pl, err := newPipelineWithLogger(cfg, store, logger)
	if err != nil {
		return err
	}

	result, err := pl.ReprocessDocument(context.Background(), *user, documentID)
	if err != nil {
		return err
	}
	return printJSON(result)
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
	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return err
	}
	plannerProvider, err := resolveProvider(providers, cfg.LLM.PlannerProvider, "planner_provider")
	if err != nil {
		return err
	}
	answerProvider, err := resolveProvider(providers, cfg.LLM.AnswerProvider, "answer_provider")
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
