package ask

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Result mirrors docs/mcp/04-medical.md §5's medical.ask response shape.
type Result struct {
	Answer  string
	Sources []Source
}

// Asker ties Plan -> Providers -> RankChunks -> RenderContext ->
// GenerateAnswer together — the internal Pipeline behind medical.ask (see
// docs/mcp/04-medical.md §7).
type Asker struct {
	planner  StructuredProvider
	answerer StructuredProvider
	registry *Registry

	providerTimeout time.Duration
	maxChunks       int
	logger          *slog.Logger
}

// NewAsker builds an Asker. planner and answerer may be the same
// StructuredProvider or different ones (see docs/architecture/05-llm.md §7
// — Planner favors a fast/cheap model, Answer Generator favors generation
// quality; which concrete models these are is a main.go config concern,
// not this package's). A nil logger falls back to slog.Default().
func NewAsker(planner, answerer StructuredProvider, registry *Registry, providerTimeout time.Duration, maxChunks int, logger *slog.Logger) *Asker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Asker{planner: planner, answerer: answerer, registry: registry, providerTimeout: providerTimeout, maxChunks: maxChunks, logger: logger}
}

// Ask implements docs/mcp/04-medical.md §5-11.
func (a *Asker) Ask(ctx context.Context, userID, question string) (Result, error) {
	selections, err := Plan(ctx, a.planner, question, a.registry)
	if err != nil {
		return Result{}, fmt.Errorf("ask: plan: %w", err)
	}

	chunks := a.collect(ctx, userID, question, selections)
	ranked := RankChunks(chunks, a.registry, a.maxChunks)
	builtContext := RenderContext(question, ranked)

	answer, err := GenerateAnswer(ctx, a.answerer, question, builtContext)
	if err != nil {
		return Result{}, fmt.Errorf("ask: generate answer: %w", err)
	}

	return Result{Answer: answer, Sources: CollectSources(ranked)}, nil
}

// collect runs every selected Provider in parallel
// (docs/architecture/03-knowledge-providers.md §15), each bounded by its
// own timeout (§16) — one Provider timing out or erroring never fails the
// whole request; it's logged and simply contributes no chunks.
func (a *Asker) collect(ctx context.Context, userID, question string, selections []PlannerSelection) []KnowledgeChunk {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		chunks []KnowledgeChunk
	)

	for _, selection := range selections {
		provider, ok := a.registry.Get(selection.Provider)
		if !ok {
			continue // defensively unreachable, see Plan's own filtering
		}

		wg.Add(1)
		go func(selection PlannerSelection, provider Provider) {
			defer wg.Done()

			providerCtx, cancel := context.WithTimeout(ctx, a.providerTimeout)
			defer cancel()

			req := KnowledgeRequest{
				UserID: userID, Query: selection.SearchQuery,
				IndicatorName: selection.IndicatorName,
				Structure:     selection.Structure, Parameter: selection.Parameter,
			}
			// documents/embeddings default to the raw question if the
			// Planner didn't refine a search phrase — better than an empty
			// query, which those two Providers treat as "nothing to do".
			if req.Query == "" {
				req.Query = question
			}

			result, err := provider.Collect(providerCtx, req)
			if err != nil {
				a.logger.Warn("ask: provider failed", "provider", selection.Provider, "error", err)
				return
			}

			mu.Lock()
			chunks = append(chunks, result...)
			mu.Unlock()
		}(selection, provider)
	}

	wg.Wait()
	return chunks
}
