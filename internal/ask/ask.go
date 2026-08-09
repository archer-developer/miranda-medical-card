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
	planner          StructuredProvider
	plannerEscalate  StructuredProvider
	answerer         StructuredProvider
	answererEscalate StructuredProvider
	registry         *Registry

	providerTimeout time.Duration
	maxChunks       int
	logger          *slog.Logger
}

// NewAsker builds an Asker. planner and answerer may be the same
// StructuredProvider or different ones (see docs/architecture/05-llm.md §7
// — Planner favors a fast/cheap model, Answer Generator favors generation
// quality; which concrete models these are is a main.go config concern,
// not this package's). plannerEscalate/answererEscalate are each tried
// once if their corresponding primary provider hard-errors (see
// structuredWithEscalation) — either or both may be nil to disable
// escalation for that step, matching llm.yaml's per-provider escalation
// config (planner_provider and answer_provider are independent providers
// with independent escalation targets, unlike document_provider's OCR+
// Structured Extraction, which share one). A nil logger falls back to
// slog.Default().
func NewAsker(planner, plannerEscalate, answerer, answererEscalate StructuredProvider, registry *Registry, providerTimeout time.Duration, maxChunks int, logger *slog.Logger) *Asker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Asker{
		planner: planner, plannerEscalate: plannerEscalate,
		answerer: answerer, answererEscalate: answererEscalate,
		registry: registry, providerTimeout: providerTimeout, maxChunks: maxChunks, logger: logger,
	}
}

// Ask implements docs/mcp/04-medical.md §5-11.
func (a *Asker) Ask(ctx context.Context, userID, question string) (Result, error) {
	selections, err := Plan(ctx, a.planner, a.plannerEscalate, question, a.registry, a.logger)
	if err != nil {
		return Result{}, fmt.Errorf("ask: plan: %w", err)
	}
	if len(selections) == 0 {
		a.logger.Debug("ask: plan selected no providers", "question", question)
	}
	for _, s := range selections {
		a.logger.Debug("ask: plan selected provider",
			"provider", s.Provider, "reason", s.Reason, "indicatorName", s.IndicatorName,
			"structure", s.Structure, "parameter", s.Parameter, "searchQuery", s.SearchQuery)
	}

	chunks := a.collect(ctx, userID, question, selections)
	ranked := RankChunks(chunks, a.registry, a.maxChunks)
	builtContext := RenderContext(question, ranked)

	// Full context, not just its length — this is exactly what
	// GenerateAnswer assembles into its user message alongside question
	// (see that function's fmt.Sprintf), so logging it here is the
	// complete picture of what the Answer Generator sees, without needing
	// to thread a logger into that package function too.
	a.logger.Debug("ask: generating answer", "question", question, "chunks", len(ranked), "context", builtContext)

	answer, err := GenerateAnswer(ctx, a.answerer, a.answererEscalate, question, builtContext, a.logger)
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
			// Scoped to just those two: LabProvider also reads Query (as a
			// fallback search term when IndicatorName is empty — see its
			// own doc comment), and defaulting it to the entire raw
			// question there would break the documented "no term at all
			// means every indicator" contract.
			if req.Query == "" && (selection.Provider == "documents" || selection.Provider == "embeddings") {
				req.Query = question
			}

			a.logger.Debug("ask: calling provider",
				"provider", selection.Provider, "userId", userID, "query", req.Query,
				"indicatorName", req.IndicatorName, "structure", req.Structure, "parameter", req.Parameter)

			result, err := provider.Collect(providerCtx, req)
			if err != nil {
				a.logger.Warn("ask: provider failed", "provider", selection.Provider, "error", err)
				return
			}
			a.logger.Debug("ask: provider returned", "provider", selection.Provider, "chunks", len(result))

			mu.Lock()
			chunks = append(chunks, result...)
			mu.Unlock()
		}(selection, provider)
	}

	wg.Wait()
	return chunks
}
