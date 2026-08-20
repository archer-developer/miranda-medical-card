// Command medical-dev implements a scoped subset of `medical dev`
// (docs/cli/medical_dev.md) — developer diagnostic tools that use the same
// Application Services as the MCP server (internal/pipeline.Pipeline,
// internal/ask.Asker), never internal/storage directly, per that doc's §2
// ("использовать те же Application Services, что и MCP API... не
// обращаться напрямую к Repository").
//
// Implemented: profile, timeline, planned-actions, document, ask, pipeline
// (the read-oriented commands directly useful for inspecting a running
// deployment's data, plus pipeline for re-running Processing Pipeline
// against an already-imported document with full Debug-level tracing to
// stderr — docs/cli/medical_dev.md
// §12), backfill-titles and reindex-fts — not part of that doc (both are
// one-off data migrations, not standing diagnostic commands), see
// pipeline.Pipeline.BackfillStudyTitle's and .ReindexDocumentFTS's own doc
// comments — and llm-trace (see llm_trace.go), also not part of that doc: a
// reader for logs/llm.log (only ever produced when logging.level: debug —
// written by cmd/miranda-medical-card/main.go's buildLLMTraceWriter for a
// real medical.ask MCP call, and identically by this file's own
// openLLMTraceWriter for `medical-dev ask`, so the same log and the same
// llm-trace command cover a question asked either way) rather than
// anything Application-Service-shaped.
// Not implemented: planner, provider, search, prompt, llm (see
// docs/cli/medical_dev.md §5-8, §14) — each would need its own
// intermediate-result plumbing (e.g. exposing the Planner's raw selections,
// or a single Provider's raw output, independent of a full Ask) that
// internal/ask doesn't expose today.
//
// docs/cli/medical.md (a separate, broader "medical" administrative CLI —
// documents/embeddings/timeline/database/maintenance/doctor/users/
// statistics categories, none of which this binary is) was deleted
// 2026-08-13: it never had an implementation, its own category docs
// (02-documents.md etc.) never existed either, and everything it aspired to
// cover for actual day-to-day diagnostic use is already this binary's job —
// a second, broader admin CLI would just duplicate it. If a real need for
// destructive/administrative operations (not diagnostic reads or the
// narrow one-off migrations above) shows up later, it belongs as a new
// medical-dev command, not a revived separate binary.
//
// Usage:
//
//	medical-dev              # or `medical-dev help` — full command list with examples (help.go)
//	medical-dev profile --user alex
//	medical-dev timeline --user alex [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--type TYPE]
//	medical-dev planned-actions --user alex [--include-resolved]
//	medical-dev document <documentId> --user alex
//	medical-dev ask --user alex "question"
//	medical-dev pipeline <documentId> --user alex
//	medical-dev backfill-titles --user alex [--provider gemini-agent]
//	medical-dev reindex-fts --user alex
//	medical-dev llm-trace [--file logs/llm.log] [--conversation ID | --latest]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/anthropic"
	"github.com/archer-developer/miranda-llm/embedding"
	"github.com/archer-developer/miranda-llm/gemini"
	"github.com/archer-developer/miranda-llm/llmtrace"
	"github.com/archer-developer/miranda-llm/openaicompat"
	"github.com/archer-developer/miranda-llm/router"

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
		printHelp()
		return nil
	}
	command, args := args[0], args[1:]

	// help/-h/--help and llm-trace both dispatch before config/db setup
	// below since neither needs it — help reads nothing at all, and
	// llm-trace only reads logs/llm.log (see llm_trace.go).
	if command == "help" || command == "-h" || command == "--help" {
		printHelp()
		return nil
	}
	if command == "llm-trace" {
		return runLLMTrace(args)
	}
	if !isKnownCommand(command) {
		return fmt.Errorf("unknown command %q — run 'medical-dev help' for the full list", command)
	}

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
	case "planned-actions":
		return runPlannedActions(args, cfg, store)
	case "document":
		return runDocument(args, cfg, store)
	case "ask":
		return runAsk(args, cfg, store, logger)
	case "pipeline":
		return runPipeline(args, cfg, store)
	case "backfill-titles":
		return runBackfillTitles(args, cfg, store)
	case "reindex-fts":
		return runReindexFTS(args, cfg, store)
	default:
		return fmt.Errorf("unknown command %q — run 'medical-dev help' for the full list", command)
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

// resolveEscalationProvider mirrors cmd/miranda-medical-card/main.go's
// helper of the same name — see that copy's doc comment. providerName is
// any configured role (document_provider, agent_provider), not just the
// document one — each has its own independent escalation target.
func resolveEscalationProvider(providers map[string]extraction.Provider, configs []config.ProviderConfig, providerName string) extraction.Provider {
	for _, c := range configs {
		if c.Name != providerName {
			continue
		}
		if !c.Escalation.Enabled {
			return nil
		}
		return providers[c.Escalation.TargetProvider]
	}
	return nil
}

// buildAskRouter mirrors cmd/miranda-medical-card/main.go's helper of the
// same name — see that copy's doc comment.
func buildAskRouter(providers map[string]extraction.Provider, configs []config.ProviderConfig, agentProviderName string) (*router.Router, error) {
	routerProviders := make([]llm.Provider, 0, len(providers))
	for name, p := range providers {
		lp, ok := p.(llm.Provider)
		if !ok {
			return nil, fmt.Errorf("provider %q (%T) does not implement llm.Provider", name, p)
		}
		routerProviders = append(routerProviders, lp)
	}

	escalations := make(map[string]router.EscalationConfig, len(configs))
	for _, c := range configs {
		if c.Escalation.Enabled && c.Escalation.ToolName != "" {
			escalations[c.Name] = router.EscalationConfig{
				Enabled:        true,
				ToolName:       c.Escalation.ToolName,
				Description:    c.Escalation.Description,
				TargetProvider: c.Escalation.TargetProvider,
			}
		}
	}

	r, err := router.New(routerProviders, escalations, agentProviderName)
	if err != nil {
		return nil, fmt.Errorf("build ask router: %w", err)
	}
	return r, nil
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

// --- planned-actions ---

func runPlannedActions(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("planned-actions", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	includeResolved := fs.Bool("include-resolved", false, "include completed/declined actions")
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
	actions, err := pl.GetUpcomingPlan(context.Background(), *user, *includeResolved, 0)
	if err != nil {
		return err
	}
	return printJSON(actions)
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
// run) is visible immediately — docs/cli/medical_dev.md §13's "подробный
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

// --- backfill-titles ---

// runBackfillTitles re-derives MedicalDocument.Title for every existing
// document of --user via pipeline.Pipeline.BackfillStudyTitle — a one-off
// migration for documents processed before extraction.Schema had a
// studyTitle field (see that method's doc comment). Cheap relative to
// `pipeline` (ReprocessDocument): no OCR, no new Extraction version, no
// Timeline/Profile/FTS/Embeddings rebuild — replays only Stage 2a
// (Structured) against each document's already-stored RecognizedText.
//
// --provider overrides which configured llm.providers entry to use instead
// of llm.document_provider — useful when the default model's free-tier
// daily quota (per-model, not shared across a project's models) is
// exhausted but a differently-named model still has budget, e.g.
// --provider gemini-agent when gemini-document returns 429
// RESOURCE_EXHAUSTED. No escalation
// provider is wired for this command regardless of llm.yaml's escalation
// config — a one-off metadata backfill doesn't need it, and StudyTitle
// simply won't be set for a document this pass can't get a title for (see
// BackfillStudyTitle's "changed=false is not an error" contract).
func runBackfillTitles(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("backfill-titles", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	providerName := fs.String("provider", "", "override llm.document_provider with this configured provider name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return err
	}
	name := cfg.LLM.DocumentProvider
	if *providerName != "" {
		name = *providerName
	}
	provider, err := resolveProvider(providers, name, "provider")
	if err != nil {
		return err
	}
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return err
	}
	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return err
	}
	pl := pipeline.New(provider, nil, embedder, "gemini", cfg.Embedding.Model, files, store, logger)

	docs, err := pl.ListDocuments(ctx, *user)
	if err != nil {
		return err
	}

	for _, doc := range docs {
		if doc.Status != storage.DocumentStatusReady {
			fmt.Printf("%s: skipped (status=%s)\n", doc.ID, doc.Status)
			continue
		}
		changed, newTitle, err := pl.BackfillStudyTitle(ctx, *user, doc.ID)
		if err != nil {
			fmt.Printf("%s: error: %v\n", doc.ID, err)
			continue
		}
		if !changed {
			fmt.Printf("%s: unchanged (%q)\n", doc.ID, doc.Title)
			continue
		}
		fmt.Printf("%s: %q -> %q\n", doc.ID, doc.Title, newTitle)
	}
	return nil
}

// --- reindex-fts ---

// runReindexFTS drives pipeline.Pipeline.ReindexDocumentFTS (see its own
// doc comment) over every READY document a user has, to bring documents
// imported before documentTypesWithoutFreeTextContent existed (pipeline.go)
// in line with it — no LLM calls, no re-OCR, just a plain rebuild from
// what's already stored. Unlike backfill-titles, needs no --provider
// override: ReindexDocumentFTS never touches an LLM provider at all, so
// newPipeline's default llm.document_provider is never even called.
func runReindexFTS(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("reindex-fts", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("--user is required")
	}

	ctx := context.Background()
	pl, err := newPipeline(cfg, store)
	if err != nil {
		return err
	}

	docs, err := pl.ListDocuments(ctx, *user)
	if err != nil {
		return err
	}

	for _, doc := range docs {
		if doc.Status != storage.DocumentStatusReady {
			fmt.Printf("%s: skipped (status=%s)\n", doc.ID, doc.Status)
			continue
		}
		if err := pl.ReindexDocumentFTS(ctx, *user, doc.ID); err != nil {
			fmt.Printf("%s: error: %v\n", doc.ID, err)
			continue
		}
		fmt.Printf("%s: reindexed (documentType=%s)\n", doc.ID, doc.DocumentType)
	}
	return nil
}

// --- ask ---

// llmLogPath is where openLLMTraceWriter (below) and llm-trace's own
// default --file both look — the same relative path
// cmd/miranda-medical-card/main.go's buildLLMTraceWriter writes to, so a
// medical-dev ask call traces into the exact same file a real medical.ask
// MCP call would, on whichever host's config/logging.yaml has
// logging.level: debug.
const llmLogPath = "logs/llm.log"

// openLLMTraceWriter mirrors cmd/miranda-medical-card/main.go's
// buildLLMTraceWriter — same gate (only when logging.level: debug), same
// path, same append-mode open — so medical-dev ask traces uniformly with
// the real service instead of being a second, untraceable code path for
// the exact same medical.ask call (see internal/ask.Asker.Ask; runAsk below
// builds the identical Registry/Asker, just against a CLI-supplied
// question instead of an MCP request). Returns (nil, nil), not an error,
// when debug logging is off, so the caller simply skips wiring a tracer.
func openLLMTraceWriter(cfg config.LoggingConfig) (io.WriteCloser, error) {
	if cfg.Level != "debug" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(llmLogPath), 0o755); err != nil {
		return nil, fmt.Errorf("create llm log dir: %w", err)
	}
	w, err := os.OpenFile(llmLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open llm log %s: %w", llmLogPath, err)
	}
	return w, nil
}

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
	if _, err := resolveProvider(providers, cfg.LLM.AgentProvider, "agent_provider"); err != nil {
		return err
	}
	askRouter, err := buildAskRouter(providers, cfg.LLM.Providers, cfg.LLM.AgentProvider)
	if err != nil {
		return err
	}
	llmLogWriter, err := openLLMTraceWriter(cfg.Logging)
	if err != nil {
		return err
	}
	if llmLogWriter != nil {
		defer func() { _ = llmLogWriter.Close() }()
		askRouter.SetTracer(llmtrace.New(llmLogWriter))
	}
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return err
	}

	timelineRepo := storage.NewTimelineRepository(store)
	registry := ask.NewRegistry(
		ask.NewTimelineProvider(timelineRepo),
		ask.NewSelfReportedEventProvider(timelineRepo),
		ask.NewMedicationProvider(storage.NewMedicationRepository(store)),
		ask.NewDiagnosisProvider(storage.NewDiagnosisRepository(store)),
		ask.NewLabProvider(storage.NewLabResultRepository(store)),
		ask.NewInstrumentalFindingProvider(storage.NewInstrumentalFindingRepository(store)),
		ask.NewProcedureProvider(storage.NewProcedureRepository(store)),
		ask.NewPlannedActionProvider(storage.NewPlannedActionRepository(store)),
		ask.NewDocumentProvider(storage.NewFTSRepository(store), storage.NewDocumentRepository(store)),
		ask.NewEmbeddingProvider(storage.NewEmbeddingRepository(store), storage.NewDocumentRepository(store), storage.NewSelfReportedEventRepository(store), embedder, cfg.Embedding.Model),
	)
	sessionStore := ask.NewSessionStore(storage.NewAskSessionRepository(store))
	// 20s/20/16 mirror cmd/miranda-medical-card/main.go's
	// askProviderTimeout/askMaxChunks/askMaxToolIterations — kept in sync by
	// hand (no shared package for three literals) so a question behaves
	// identically asked either way, which is exactly what makes comparing
	// a medical-dev ask run against a real medical.ask trace meaningful.
	asker := ask.NewAsker(askRouter, registry, sessionStore, 20*time.Second, 20, 16, logger)

	fmt.Println("Question")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println()
	fmt.Println(question)
	fmt.Println()

	// medical-dev is a one-off diagnostic CLI, not a Miranda-facing session
	// — every invocation is stateless (empty sessionId), and *user doubles
	// as both the caller and the subject (no subjectId flag exposed here).
	result, err := asker.Ask(ctx, *user, *user, "", question)
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
