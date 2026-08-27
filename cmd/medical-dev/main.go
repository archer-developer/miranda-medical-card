// Command medical-dev implements a scoped subset of `medical dev`
// (docs/cli/medical_dev.md) — developer diagnostic tools that use the same
// Application Services as the MCP server (internal/pipeline.Pipeline,
// internal/ask.Asker), never internal/storage directly, per that doc's §2
// ("использовать те же Application Services, что и MCP API... не
// обращаться напрямую к Repository").
//
// Implemented: profile (its --rebuild flag re-runs Profile aggregation
// against already-persisted data before printing it, see
// docs/cli/medical_dev.md §9 and pipeline.Pipeline.RebuildProfile's own doc
// comment), timeline, planned-actions, document, ask, pipeline (the
// read-oriented commands directly useful for inspecting a running
// deployment's data, plus pipeline for re-running the Processing Pipeline
// against an already-imported document with full Debug-level tracing to
// stderr — docs/cli/medical_dev.md §13. --stage picks which document-scoped
// stage boundary to resume from (ocr/extraction/normalization — see
// pipelineStages' own doc comment); --all loops over every document a user
// has instead of a single documentId), backfill-titles and reindex-fts —
// not part of that doc (both are one-off data migrations, not standing
// diagnostic commands), see pipeline.Pipeline.BackfillStudyTitle's and
// .ReindexDocumentFTS's own doc comments — and llm-trace (see
// llm_trace.go), also not part of that doc: a
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
//	medical-dev profile --user alex --rebuild
//	medical-dev timeline --user alex [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--type TYPE]
//	medical-dev planned-actions --user alex [--include-resolved]
//	medical-dev document <documentId> --user alex
//	medical-dev ask --user alex "question"
//	medical-dev pipeline <documentId> --user alex
//	medical-dev pipeline <documentId> --user alex --stage extraction [--provider gemini-agent]
//	medical-dev pipeline --all --user alex --stage normalization
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
	return newPipelineWithLoggerAndProvider(cfg, store, logger, "")
}

// newPipelineWithLoggerAndProvider is newPipelineWithLogger plus an optional
// providerOverride in place of both cfg.LLM.OCRProvider and
// cfg.LLM.ExtractionProvider — see runPipeline's --provider flag (mirroring
// runBackfillTitles's own): a configured model's free-tier daily quota is
// per-model, not shared across a project's other models, so a bulk
// operation can keep going against a differently-named model once the
// default one starts returning 429 RESOURCE_EXHAUSTED. Applied to both
// roles uniformly rather than requiring two separate flags — the realistic
// use case is "send everything to a fresh model," not retargeting OCR and
// Structured Extraction independently (config/llm.yaml itself is the tool
// for that, see LLMConfig.OCRProvider/ExtractionProvider's own doc
// comments). Unlike runBackfillTitles's own hand-rolled Pipeline
// construction, this still wires escalation (via resolveEscalationProvider)
// for whichever provider ends up named — a real Structured Extraction run
// should get the same escalation behavior production wiring gives it,
// unlike backfill-titles's one-off metadata patch which deliberately opts
// out (see that function's own doc comment).
func newPipelineWithLoggerAndProvider(cfg config.Config, store *storage.Store, logger *slog.Logger, providerOverride string) (*pipeline.Pipeline, error) {
	ctx := context.Background()
	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return nil, err
	}
	ocrName := cfg.LLM.OCRProvider
	extractionName := cfg.LLM.ExtractionProvider
	if providerOverride != "" {
		ocrName = providerOverride
		extractionName = providerOverride
	}
	ocrProvider, err := resolveProvider(providers, ocrName, "ocr_provider")
	if err != nil {
		return nil, err
	}
	ocrEscalation := resolveEscalationProvider(providers, cfg.LLM.Providers, ocrName)
	extractionProvider, err := resolveProvider(providers, extractionName, "extraction_provider")
	if err != nil {
		return nil, err
	}
	extractionEscalation := resolveEscalationProvider(providers, cfg.LLM.Providers, extractionName)
	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return nil, err
	}
	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return nil, err
	}
	return pipeline.New(ocrProvider, ocrEscalation, extractionProvider, extractionEscalation, embedder, "gemini", cfg.Embedding.Model, files, store, logger, pipeline.NewConfigUserRepository(cfg.Users)), nil
}

// buildProviders, resolveProvider, and resolveEscalationProvider mirror
// cmd/miranda-medical-card/main.go's helpers of the same name — see that
// copy's doc comments. Duplicated rather than shared since these are two
// separate main packages, same as this file's other construction helpers.
func buildProviders(ctx context.Context, configs []config.ProviderConfig, logger *slog.Logger) (map[string]extraction.Provider, error) {
	providers := make(map[string]extraction.Provider, len(configs))
	for _, c := range configs {
		switch c.Type {
		case "gemini":
			p, err := gemini.New(ctx, c.Name, c.Model, c.APIKeyEnvs,
				gemini.ToolsConfig{},
				gemini.RotationConfig{CooldownSeconds: c.Rotation.CooldownSeconds, MaxRetryCycles: c.Rotation.MaxRetryCycles},
				logger,
			)
			if err != nil {
				return nil, fmt.Errorf("build provider %q: %w", c.Name, err)
			}
			providers[c.Name] = p
		case "anthropic":
			p, err := anthropic.New(c.Name, c.Model, c.APIKeyEnvs,
				anthropic.ToolsConfig{},
				anthropic.RotationConfig{CooldownSeconds: c.Rotation.CooldownSeconds, MaxRetryCycles: c.Rotation.MaxRetryCycles},
				logger,
			)
			if err != nil {
				return nil, fmt.Errorf("build provider %q: %w", c.Name, err)
			}
			providers[c.Name] = p
		case "openai_compat":
			providers[c.Name] = openaicompat.New(c.Name, c.BaseURL, c.Model, c.APIKeyEnvs,
				openaicompat.RotationConfig{CooldownSeconds: c.Rotation.CooldownSeconds, MaxRetryCycles: c.Rotation.MaxRetryCycles},
				logger,
			)
		default:
			return nil, fmt.Errorf("build provider %q: unknown type %q", c.Name, c.Type)
		}
	}
	return providers, nil
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
// any configured role (ocr_provider, extraction_provider, agent_provider),
// not just one of them — each has its own independent escalation target.
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
	// rebuild re-runs Profile aggregation against already-persisted data
	// before printing it (pipeline.Pipeline.RebuildProfile) instead of just
	// reading back the last stored snapshot — useful after a Profile-
	// shaping change ships (new aggregation logic, a new field) to refresh
	// an existing user's snapshot without re-running OCR/Structured
	// Extraction on any of their documents. Unlike every other medical-dev
	// command, this one flag makes profile a write, not a read — see
	// docs/cli/medical_dev.md §2's "любые операции, изменяющие данные,
	// должны быть явно указаны пользователем", which --rebuild is.
	rebuild := fs.Bool("rebuild", false, "rebuild the stored profile from current data before printing it")
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
	if *rebuild {
		built, err := pl.RebuildProfile(context.Background(), *user)
		if err != nil {
			return err
		}
		return printJSON(built)
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

// pipelineStages are the values --stage accepts (default "ocr") — the
// document-scoped resumable boundaries from
// docs/architecture/02-processing-pipeline.md §2's "Независимость этапов":
//
//   - ocr: full run — OCR + Structured Extraction + everything downstream
//     (Pipeline.ReprocessDocument).
//   - extraction: skip OCR, reuse stored RecognizedText, fresh Structured
//     Extraction + everything downstream (Pipeline.ReextractDocument) —
//     for a Structured Extraction schema/prompt fix (e.g.
//     Diagnosis.status/expectedResolution) that doesn't need OCR redone.
//   - normalization: skip OCR and Structured Extraction, replay the
//     document's current active Extraction (its stored Raw JSON) through
//     Normalization and everything downstream — zero LLM calls
//     (Pipeline.RenormalizeDocument) — for a Normalization-only fix (unit
//     conversion, date parsing) that doesn't need a fresh model call at
//     all.
//
// Profile rebuild and an embeddings-model refresh are deliberately not
// stage values here: Profile is user-scoped (rebuilt from every document's
// already-persisted entities at once, no single document to resume from),
// and a bulk re-embed only needs a document's already-stored Summary — the
// same "loop every document, redo one cheap step" shape as backfill-titles,
// not a resume point partway through this per-document chain.
var pipelineStages = map[string]bool{
	"ocr":           true,
	"extraction":    true,
	"normalization": true,
}

// runPipeline re-runs the Processing Pipeline against an already-imported
// document, starting from --stage (default "ocr", today's full pipeline —
// see pipelineStages). A single document gets a Debug-level logger writing
// straight to stderr, so every Structured Extraction attempt and Pipeline
// stage (see internal/extraction.StructuredWithRetry,
// internal/pipeline.Pipeline's process/normalizeAndPersist) is visible
// immediately — docs/cli/medical_dev.md §13's "подробный лог выполнения",
// without needing to enable server-wide debug logging or tail
// logs/miranda-medical-card.log separately.
//
// --all loops over every document --user has instead of a single
// documentId, with a quieter Warn-level logger and one summary line per
// document, continuing past an individual failure rather than aborting the
// whole batch — a per-model daily quota running out partway through
// --stage ocr/extraction is the realistic failure mode (see --provider
// below) — and a non-zero final exit status still reports how many failed,
// so a wrapping script can tell "some documents didn't finish" from "all
// done."
//
// --provider overrides both llm.ocr_provider and llm.extraction_provider
// with the same named provider, same escape hatch as backfill-titles's own
// --provider flag; harmlessly unused for --stage=normalization, which never
// calls an LLM at all (and, for --stage=extraction, only extraction_provider
// is actually resolved — see newPipelineWithLoggerAndProvider's own doc
// comment for why the override still applies uniformly to both roles).
func runPipeline(args []string, cfg config.Config, store *storage.Store) error {
	fs := flag.NewFlagSet("pipeline", flag.ExitOnError)
	user := fs.String("user", "", "user id")
	stage := fs.String("stage", "ocr", "resume from this stage: ocr (default, full run) | extraction (skip OCR) | normalization (skip OCR and Structured Extraction, zero LLM calls)")
	all := fs.Bool("all", false, "run every document --user has instead of a single documentId")
	providerName := fs.String("provider", "", "override llm.ocr_provider and llm.extraction_provider with this configured provider name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("--user is required")
	}
	if !pipelineStages[*stage] {
		return fmt.Errorf("--stage %q is not one of ocr, extraction, normalization", *stage)
	}
	if !*all && fs.NArg() < 1 {
		return fmt.Errorf("usage: medical-dev pipeline <documentId> --user <user> [--stage ocr|extraction|normalization]  (or --all --user <user>)")
	}

	runStage := func(pl *pipeline.Pipeline, ctx context.Context, userID, documentID string) (pipeline.Result, error) {
		switch *stage {
		case "extraction":
			return pl.ReextractDocument(ctx, userID, documentID)
		case "normalization":
			return pl.RenormalizeDocument(ctx, userID, documentID)
		default:
			return pl.ReprocessDocument(ctx, userID, documentID)
		}
	}

	if *all {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		pl, err := newPipelineWithLoggerAndProvider(cfg, store, logger, *providerName)
		if err != nil {
			return err
		}
		ctx := context.Background()
		docs, err := pl.ListDocuments(ctx, *user)
		if err != nil {
			return err
		}
		var failed int
		for _, doc := range docs {
			result, err := runStage(pl, ctx, *user, doc.ID)
			if err != nil {
				failed++
				fmt.Printf("%s: error: %v\n", doc.ID, err)
				continue
			}
			fmt.Printf("%s: ok (diagnoses=%d, medications=%d, labResults=%d, instrumentalFindings=%d)\n",
				doc.ID, result.ExtractedCounts.Diagnoses, result.ExtractedCounts.Medications,
				result.ExtractedCounts.LabResults, result.ExtractedCounts.InstrumentalFindings)
		}
		if failed > 0 {
			return fmt.Errorf("%d/%d document(s) failed — rerun 'medical-dev pipeline --all --user %s --stage %s' to retry (already-succeeded documents are unaffected)", failed, len(docs), *user, *stage)
		}
		return nil
	}

	documentID := fs.Arg(0)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pl, err := newPipelineWithLoggerAndProvider(cfg, store, logger, *providerName)
	if err != nil {
		return err
	}
	result, err := runStage(pl, context.Background(), *user, documentID)
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
// of llm.extraction_provider (BackfillStudyTitle never calls OCR, only
// Stage 2a — see its own doc comment — so llm.ocr_provider is irrelevant
// here and left unresolved) — useful when the default model's free-tier
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
	providerName := fs.String("provider", "", "override llm.extraction_provider with this configured provider name")
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
	name := cfg.LLM.ExtractionProvider
	if *providerName != "" {
		name = *providerName
	}
	provider, err := resolveProvider(providers, name, "extraction_provider")
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
	// users is nil — BackfillStudyTitle never triggers rebuildProfile (see
	// this function's own doc comment: "no ... Profile ... rebuild"), so
	// Nutrition Advisor's age/sex input is never consulted here.
	pl := pipeline.New(nil, nil, provider, nil, embedder, "gemini", cfg.Embedding.Model, files, store, logger, nil)

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
// newPipeline's default llm.ocr_provider/extraction_provider are never even
// called.
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
		// Wrapped in ContextTracer, not SetTracer(llmtrace.New(...)) alone —
		// mirrors cmd/miranda-medical-card/main.go's own run() exactly, so
		// SetAnomalyConfig below actually has a live tracer to tee onto (see
		// llmtrace.WithTracer's doc comment).
		askRouter.SetTracer(&llmtrace.ContextTracer{Default: llmtrace.New(llmLogWriter)})
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
	if llmLogWriter != nil {
		asker.SetAnomalyConfig(ask.AnomalyConfig{LLMLogPath: llmLogPath, Dir: filepath.Join(filepath.Dir(llmLogPath), "anomalies")})
	}

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
