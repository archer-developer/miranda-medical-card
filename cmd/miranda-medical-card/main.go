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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-llm/embedding"
	"github.com/archer-developer/miranda-llm/gemini"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/envfile"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/httpserver"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
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

	// askProviderTimeout bounds each Knowledge Provider's Collect call
	// (docs/architecture/03-knowledge-providers.md §16) — a slow/hung
	// Provider must not hang the whole medical.ask request.
	askProviderTimeout = 20 * time.Second
	// askMaxChunks caps the Context Builder's output
	// (docs/architecture/04-search.md §3, "Минимизировать объём контекста").
	askMaxChunks = 20
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

	files, err := filestore.New(cfg.Files.Dir)
	if err != nil {
		return fmt.Errorf("main: open filestore: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	documentProvider, err := newGeminiProvider(ctx, "document", cfg.LLM, logger)
	if err != nil {
		return fmt.Errorf("main: init document model: %w", err)
	}
	plannerProvider, err := newGeminiProvider(ctx, "planner", cfg.LLM, logger)
	if err != nil {
		return fmt.Errorf("main: init planner model: %w", err)
	}
	answerProvider, err := newGeminiProvider(ctx, "answer", cfg.LLM, logger)
	if err != nil {
		return fmt.Errorf("main: init answer model: %w", err)
	}

	embeddingAPIKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	if embeddingAPIKey == "" {
		return fmt.Errorf("main: environment variable %s (named by embedding.api_key_env) is not set", cfg.Embedding.APIKeyEnv)
	}
	embedder, err := embedding.NewGemini(ctx, embeddingAPIKey, cfg.Embedding.Model)
	if err != nil {
		return fmt.Errorf("main: init gemini embedder: %w", err)
	}

	pl := pipeline.New(documentProvider, embedder, "gemini", cfg.Embedding.Model, files, store, logger)

	registry := ask.NewRegistry(
		ask.NewTimelineProvider(storage.NewTimelineRepository(store)),
		ask.NewMedicationProvider(storage.NewMedicationRepository(store)),
		ask.NewDiagnosisProvider(storage.NewDiagnosisRepository(store)),
		ask.NewLabProvider(storage.NewLabResultRepository(store)),
		ask.NewInstrumentalFindingProvider(storage.NewInstrumentalFindingRepository(store)),
		ask.NewProcedureProvider(storage.NewProcedureRepository(store)),
		ask.NewProfileProvider(profile.NewStore(storage.NewProfileRepository(store))),
		ask.NewDocumentProvider(storage.NewFTSRepository(store)),
		ask.NewEmbeddingProvider(storage.NewEmbeddingRepository(store), storage.NewDocumentRepository(store), storage.NewSelfReportedEventRepository(store), embedder, cfg.Embedding.Model),
	)
	asker := ask.NewAsker(plannerProvider, answerProvider, registry, askProviderTimeout, askMaxChunks, logger)

	if cfg.TLS.Enabled {
		if err := tlscert.EnsureSelfSigned(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.Hosts); err != nil {
			return fmt.Errorf("main: prepare TLS certificate: %w", err)
		}
	}

	server := mcpserver.New(pl, asker, cfg.Users, cfg.Files.MaxSizeBytes, logger)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler := httpserver.New(mcpHandler, token)
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: handler}

	logger.Info("medical-card ready",
		"database", cfg.Database.Path,
		"files", cfg.Files.Dir,
		"documentModel", cfg.LLM.DocumentModel,
		"plannerModel", cfg.LLM.PlannerModel,
		"answerModel", cfg.LLM.AnswerModel,
		"embeddingModel", cfg.Embedding.Model,
		"addr", cfg.HTTPAddr,
		"tls", cfg.TLS.Enabled,
		"users", len(cfg.Users),
	)

	return serveUntilInterrupted(ctx, httpServer, cfg.TLS, logger)
}

// newGeminiProvider builds one gemini.Provider for stage (a log-friendly
// name only, e.g. "planner") using model. All Gemini calls share the same
// rotation pool of API keys (cfg.APIKeyEnvs) — see gemini.RotationConfig.
func newGeminiProvider(ctx context.Context, stage string, cfg config.LLMConfig, logger *slog.Logger) (*gemini.Provider, error) {
	var model string
	switch stage {
	case "document":
		model = cfg.DocumentModel
	case "planner":
		model = cfg.PlannerModel
	case "answer":
		model = cfg.AnswerModel
	}
	return gemini.New(ctx, "gemini-"+stage, model, cfg.APIKeyEnvs,
		gemini.ToolsConfig{},
		gemini.RotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1},
		logger,
	)
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
