// Command miranda-medical-card is the Medical Service MCP server
// (docs/architecture/01-overview.md). It stores, structures, indexes, and
// answers questions about a household's medical history.
//
// Architecture mirrors miranda-service-skeleton and miranda-diary: static
// CGO-free binary, config/*.yaml + .env for local development, systemd
// --user service on the server.
//
// Bootstrap: envfile.Load(.env) -> list config/*.yaml (configFilePaths) ->
// config.Load(paths...) -> build the real logger -> check required secrets
// are set -> open SQLite + filestore -> construct Gemini providers/embedder
// -> build Pipeline + Asker -> build the MCP server -> wrap it in a
// Streamable HTTP handler -> mount it behind bearer auth and /healthz ->
// serve until SIGINT/SIGTERM, then shut down gracefully.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	"github.com/archer-developer/miranda-medical-card/internal/httpserver"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/tlscert"
)

const (
	dotEnvPath       = ".env"
	defaultConfigDir = "config"
	configDirEnv     = "MEDICAL_CARD_CONFIG_DIR"
	shutdownTimeout  = 10 * time.Second
	debugLogDir      = "logs"
	debugLogFile     = "debug.log"
	// llmLogFile carries every LLM request/response the router.Router
	// backing the medical.ask agent loop traces (see buildLLMTraceWriter) —
	// only written when logging.level is "debug", same as debugLogFile.
	llmLogFile = "llm.log"

	// askProviderTimeout bounds each Knowledge Provider's Collect call
	// (docs/architecture/03-knowledge-providers.md §16) — a slow/hung
	// Provider must not hang the whole medical.ask request.
	askProviderTimeout = 20 * time.Second
	// askMaxChunks caps the Context Builder's output
	// (docs/architecture/04-search.md §3, "Минимизировать объём контекста").
	askMaxChunks = 20
	// askMaxToolIterations bounds the agent loop's tool-call round trips
	// per medical.ask call (see internal/ask.Asker.Ask) — lower than
	// miranda's own agent loop's 15, since this loop only ever calls
	// read-only Knowledge Providers (no chained side-effecting steps), so a
	// realistic worst case is a handful of distinct calls plus parameter
	// refinement.
	askMaxToolIterations = 8
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with process environment", "error", err)
	}

	cfgDir := defaultConfigDir
	if v := os.Getenv(configDirEnv); v != "" {
		cfgDir = v
	}

	configPaths, err := configFilePaths(cfgDir, logger)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(configPaths...)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	realLogger, closeLogger, err := buildLogger(cfg.Logging)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
	defer closeLogger()
	logger = realLogger

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// configFilePaths lists every *.yaml file directly under cfgDir, sorted by
// name — see miranda-diary/cmd/miranda-diary/main.go's function of the same
// name for the full rationale (os.ReadDir vs filepath.Glob, missing dir
// handling) this mirrors verbatim.
func configFilePaths(cfgDir string, logger *slog.Logger) ([]string, error) {
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Warn("config directory not found, using built-in defaults", "dir", cfgDir)
			return nil, nil
		}
		return nil, fmt.Errorf("main: read config dir %s: %w", cfgDir, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(cfgDir, entry.Name()))
	}

	logger.Info("config files discovered", "dir", cfgDir, "paths", paths)
	return paths, nil
}

// seedIndicatorAliases populates the indicator_aliases table (see
// internal/storage/indicator_alias.go) from
// normalization.IndicatorAliasSeedGroups on every startup — cheap and
// idempotent (SetIfAbsent never overwrites a row already there), so a
// fresh database gets the curated dictionary immediately and an existing
// one keeps any operator/LLM edit made directly against the table since
// the last restart.
func seedIndicatorAliases(ctx context.Context, store *storage.Store) error {
	repo := storage.NewIndicatorAliasRepository(store)
	for _, group := range normalization.IndicatorAliasSeedGroups() {
		canonical := group[0]
		for _, alias := range group {
			if err := repo.SetIfAbsent(ctx, alias, canonical); err != nil {
				return err
			}
		}
	}
	return nil
}

func run(cfg config.Config, logger *slog.Logger) error {
	token := os.Getenv(cfg.AuthTokenEnv)
	if token == "" {
		return fmt.Errorf("main: environment variable %s (named by auth_token_env) is not set — refusing to start with no auth token", cfg.AuthTokenEnv)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("main: create database directory: %w", err)
	}
	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("main: open database: %w", err)
	}
	defer func() {
		logger.Info("closing database")
		_ = store.Close()
	}()

	if err := seedIndicatorAliases(context.Background(), store); err != nil {
		return fmt.Errorf("main: seed indicator aliases: %w", err)
	}

	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return fmt.Errorf("main: open filestore: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, err := buildProviders(ctx, cfg.LLM.Providers, logger)
	if err != nil {
		return fmt.Errorf("main: init llm providers: %w", err)
	}
	documentProvider, err := resolveProvider(providers, cfg.LLM.DocumentProvider, "document_provider")
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}
	escalationProvider := resolveEscalationProvider(providers, cfg.LLM.Providers, cfg.LLM.DocumentProvider)

	if _, err := resolveProvider(providers, cfg.LLM.AgentProvider, "agent_provider"); err != nil {
		return fmt.Errorf("main: %w", err)
	}
	askRouter, err := buildAskRouter(providers, cfg.LLM.Providers, cfg.LLM.AgentProvider)
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	embeddingAPIKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	if embeddingAPIKey == "" {
		return fmt.Errorf("main: environment variable %s (named by embedding.api_key_env) is not set", cfg.Embedding.APIKeyEnv)
	}
	embedder, err := embedding.NewGemini(ctx, embeddingAPIKey, cfg.Embedding.Model)
	if err != nil {
		return fmt.Errorf("main: init gemini embedder: %w", err)
	}

	pl := pipeline.New(documentProvider, escalationProvider, embedder, "gemini", cfg.Embedding.Model, files, store, logger)

	// timelineRepo is shared between TimelineProvider and
	// SelfReportedEventProvider — both read the same timeline_events table,
	// just with a different Types filter (see providers.go).
	timelineRepo := storage.NewTimelineRepository(store)
	registry := ask.NewRegistry(
		ask.NewTimelineProvider(timelineRepo),
		ask.NewSelfReportedEventProvider(timelineRepo),
		ask.NewMedicationProvider(storage.NewMedicationRepository(store)),
		ask.NewDiagnosisProvider(storage.NewDiagnosisRepository(store)),
		ask.NewLabProvider(storage.NewLabResultRepository(store)),
		ask.NewInstrumentalFindingProvider(storage.NewInstrumentalFindingRepository(store)),
		ask.NewProcedureProvider(storage.NewProcedureRepository(store)),
		ask.NewProfileProvider(profile.NewStore(storage.NewProfileRepository(store))),
		ask.NewDocumentProvider(storage.NewFTSRepository(store), storage.NewDocumentRepository(store)),
		ask.NewEmbeddingProvider(storage.NewEmbeddingRepository(store), storage.NewDocumentRepository(store), storage.NewSelfReportedEventRepository(store), embedder, cfg.Embedding.Model),
	)
	sessionStore := ask.NewSessionStore(storage.NewAskSessionRepository(store))
	asker := ask.NewAsker(askRouter, registry, sessionStore, askProviderTimeout, askMaxChunks, askMaxToolIterations, logger)

	if cfg.TLS.Enabled {
		if err := tlscert.EnsureSelfSigned(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.Hosts); err != nil {
			return fmt.Errorf("main: prepare TLS certificate: %w", err)
		}
	}

	llmLogWriter, err := buildLLMTraceWriter(cfg.Logging)
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}
	if llmLogWriter != nil {
		defer func() { _ = llmLogWriter.Close() }()
		// askRouter wraps every configured provider instance (see
		// buildAskRouter), so this one call traces both the agent loop's
		// Chat calls and the document pipeline's Structured/Chat calls —
		// pipeline.New above never reconstructs the providers, it reuses
		// these same instances.
		askRouter.SetTracer(llmtrace.New(llmLogWriter))
	}

	server := mcpserver.New(pl, asker, cfg.Users, cfg.Files.MaxSizeBytes, cfg.PublicBaseURL, logger)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	fileHandler := mcpserver.NewFileDownloadHandler(pl, logger)
	handler := httpserver.New(mcpHandler, fileHandler, token)
	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
		// BaseContext ties every request's r.Context() to the same ctx
		// signal.NotifyContext cancels on SIGINT/SIGTERM (net/http derives
		// each request's context as a child of whatever this returns) — so
		// the moment a shutdown signal arrives, an in-flight medical.ask
		// call's provider.Collect (internal/ask/agent_loop.go's
		// providerCtx) and the LLM SDK call underneath it see their context
		// canceled immediately and return, instead of running to
		// completion (which, under Gemini rate-limit retries, can easily
		// outlast shutdownTimeout). Without this, httpServer.Shutdown
		// below just blocks on that slow handler until shutdownCtx expires
		// and returns context.DeadlineExceeded, which main() logs as a
		// fatal crash and systemd restarts — a normal `systemctl restart`
		// masquerading as a failure.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	logger.Info("medical-card ready",
		"database", cfg.Database.Path,
		"files", cfg.Files.Dir,
		"documentProvider", cfg.LLM.DocumentProvider,
		"agentProvider", cfg.LLM.AgentProvider,
		"escalationConfigured", escalationProvider != nil,
		"embeddingModel", cfg.Embedding.Model,
		"publicBaseURL", cfg.PublicBaseURL,
		"addr", cfg.HTTPAddr,
		"tls", cfg.TLS.Enabled,
		"users", len(cfg.Users),
		"llmLogEnabled", llmLogWriter != nil,
	)

	return serveUntilInterrupted(ctx, httpServer, cfg.TLS, logger)
}

// buildProviders constructs every configured LLM backend once, keyed by
// its ProviderConfig.Name, so document_provider/agent_provider (and any
// provider's escalation.target_provider) can all resolve by name against
// the same set of instances — mirrors miranda's own cmd/miranda/main.go
// buildProviders. extraction.Provider (Chat + Structured) is the return
// type since every provider type here implements both, and that's a
// superset of what any single call site actually needs
// (resolveProvider/resolveEscalationProvider narrow it further where only
// Structured is required; buildAskRouter narrows it to llm.Provider, the
// Chat + Name() superset router.New needs).
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

// firstAPIKey mirrors miranda's own helper of the same name: only the
// first configured env var is used for provider types with no built-in
// key rotation (anthropic, openai_compat) — see ProviderConfig.APIKeyEnvs.
func firstAPIKey(envs []string) string {
	if len(envs) == 0 {
		return ""
	}
	return os.Getenv(envs[0])
}

// resolveProvider looks up name in providers, erroring with the
// config.yaml field name a caller would need to fix — config.go's
// validate() already guarantees the name exists by the time main.go runs,
// so this is a defensive check, not the primary place that error surfaces.
func resolveProvider(providers map[string]extraction.Provider, name, field string) (extraction.Provider, error) {
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("llm.%s %q does not name a configured provider", field, name)
	}
	return p, nil
}

// resolveEscalationProvider looks up providerName's own
// ProviderConfig.Escalation and, if enabled, resolves its TargetProvider —
// see extraction.StructuredWithRetry's escalate parameter (and, since
// escalation now also covers OCR, extraction.Extract's). Returns nil
// (disabling escalation) when providerName has no escalation configured,
// which is the default. Only called for document_provider now — the agent
// loop's escalation (agent_provider and any other router-wired provider)
// goes through router.EscalationConfig instead (see buildAskRouter), a
// different mechanism entirely (tool-based, mid-conversation) from this
// content-based "the result looked suspiciously empty" retry.
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

// buildAskRouter wraps every configured LLM provider (not just
// agentProviderName) in a router.Router for the medical.ask agent loop
// (internal/ask) — wrapping all of them, rather than just the one named
// agent_provider, means any provider's own escalation.target_provider can
// resolve regardless of which role it's nominally configured for, and it's
// what lets one router.SetTracer call (see buildLLMTraceWriter) cover the
// document pipeline's providers too, since providers here are the exact
// same instances buildProviders constructed — pipeline.New is never handed
// a separate copy.
func buildAskRouter(providers map[string]extraction.Provider, configs []config.ProviderConfig, agentProviderName string) (*router.Router, error) {
	routerProviders := make([]llm.Provider, 0, len(providers))
	for name, p := range providers {
		lp, ok := p.(llm.Provider)
		if !ok {
			// Unreachable in practice: every buildProviders branch
			// (gemini.Provider/anthropic.Provider/openaicompat.Provider)
			// implements llm.Provider's Name()+Chat() — extraction.Provider
			// just doesn't declare Name() itself, so the assertion is
			// needed to recover it. Guarded rather than assumed in case
			// that ever stops being true for some future provider type.
			return nil, fmt.Errorf("provider %q (%T) does not implement llm.Provider", name, p)
		}
		routerProviders = append(routerProviders, lp)
	}

	// A provider's escalation.tool_name being empty (document_provider's
	// current config never sets one) keeps it out of router.Router's
	// tool-based escalation entirely — its content-based escalation (see
	// resolveEscalationProvider) is a separate mechanism this map doesn't
	// touch.
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

// buildLLMTraceWriter opens logs/llm.log for router.Router.SetTracer (see
// run()) when debug logging is enabled — mirrors buildLogger's own
// open/lifecycle pattern (same directory, same append-mode open). Returns
// (nil, nil), not an error, when debug logging is off, so the caller
// simply skips wiring a tracer.
func buildLLMTraceWriter(cfg config.LoggingConfig) (io.WriteCloser, error) {
	if cfg.Level != "debug" {
		return nil, nil
	}
	if err := os.MkdirAll(debugLogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create llm log dir: %w", err)
	}
	path := filepath.Join(debugLogDir, llmLogFile)
	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open llm log %s: %w", path, err)
	}
	return w, nil
}

func serveUntilInterrupted(ctx context.Context, httpServer *http.Server, tlsCfg config.TLSConfig, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr, "tls", tlsCfg.Enabled)
		if tlsCfg.Enabled {
			errCh <- httpServer.ListenAndServeTLS(tlsCfg.CertFile, tlsCfg.KeyFile)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func buildLogger(cfg config.LoggingConfig) (*slog.Logger, func(), error) {
	noop := func() {}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, noop, fmt.Errorf("main: invalid logging.level %q: %w", cfg.Level, err)
	}

	stdoutLevel := level
	if stdoutLevel < slog.LevelInfo {
		stdoutLevel = slog.LevelInfo
	}
	stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: stdoutLevel})

	if level > slog.LevelDebug {
		return slog.New(stdoutHandler), noop, nil
	}

	if err := os.MkdirAll(debugLogDir, 0o755); err != nil {
		return nil, noop, fmt.Errorf("main: create debug log dir: %w", err)
	}
	debugPath := filepath.Join(debugLogDir, debugLogFile)
	debugWriter, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, noop, fmt.Errorf("main: open debug log %s: %w", debugPath, err)
	}
	debugHandler := slog.NewTextHandler(debugWriter, &slog.HandlerOptions{Level: slog.LevelDebug})

	handler := &levelSplitHandler{stdout: stdoutHandler, debugFile: debugHandler}
	return slog.New(handler), func() { _ = debugWriter.Close() }, nil
}

type levelSplitHandler struct {
	stdout    slog.Handler
	debugFile slog.Handler
}

func (h *levelSplitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.debugFile.Enabled(ctx, level)
}

func (h *levelSplitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		return h.debugFile.Handle(ctx, r)
	}
	return h.stdout.Handle(ctx, r)
}

func (h *levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithAttrs(attrs),
		debugFile: h.debugFile.WithAttrs(attrs),
	}
}

func (h *levelSplitHandler) WithGroup(name string) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithGroup(name),
		debugFile: h.debugFile.WithGroup(name),
	}
}
