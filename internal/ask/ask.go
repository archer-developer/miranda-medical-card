// Package ask implements medical.ask (docs/mcp/04-medical.md) as an
// internal agent loop (docs/adr/001-internal-agent-loop-implementation.md):
// a single LLM, given the registered Knowledge Providers as tools
// (docs/architecture/03-knowledge-providers.md), iteratively decides which
// to call — possibly several times, based on what earlier calls
// returned — until it has enough to answer directly, with no separate
// Planner or Answer Generator call. See agent_loop.go for the loop itself.
//
// Replaces the old one-shot Planner (LLM call #1, picked Providers up
// front) -> Providers (plain Go code) -> Answer Generator (LLM call #2)
// pipeline, which could never revise its provider selection based on what
// an earlier call actually returned, and had no memory between separate
// medical.ask calls.
package ask

import (
	"context"
	"log/slog"
	"time"

	llm "github.com/archer-developer/miranda-llm"
)

// Result mirrors docs/mcp/04-medical.md §5's medical.ask response shape.
type Result struct {
	Answer  string
	Sources []Source
}

// ChatProvider is the subset of *router.Router the agent loop needs — kept
// narrow (mirrors the old StructuredProvider interface this package used
// before it was removed along with planner.go/answer.go) so this package
// never imports the router package directly. *router.Router satisfies this
// structurally; see cmd/miranda-medical-card/main.go for how it's built.
type ChatProvider interface {
	ChatPinned(ctx context.Context, req llm.ChatRequest, pinnedProvider string, onProviderUsed func(string)) (<-chan llm.StreamChunk, error)
}

// Asker runs medical.ask's internal agent loop (see agent_loop.go).
type Asker struct {
	chat     ChatProvider
	registry *Registry
	sessions *SessionStore

	providerTimeout time.Duration
	// maxChunks caps how many KnowledgeChunks from a single tool call are
	// shown to the model (see agent_loop.go's executeToolCall) — applied
	// per call, not once across the whole conversation, so it can never
	// cause the final `sources` list to omit something the model actually
	// saw and might have cited.
	maxChunks     int
	maxIterations int
	logger        *slog.Logger

	// anomaly configures per-turn anomaly detection (see agent_loop.go's
	// Ask and anomaly.go's reportAnomalies) — disabled (zero value) unless
	// SetAnomalyConfig is called.
	anomaly AnomalyConfig
}

// NewAsker builds an Asker. chat is typically a *router.Router wrapping
// every configured LLM provider (see cmd/miranda-medical-card/main.go) — its
// own reliability fallback and per-provider tool-based escalation
// (docs/architecture/05-llm.md §9.1) apply transparently to every turn this
// loop makes, now that there's a genuine open conversation for escalation
// to make sense in (previously it didn't — see that doc section's history).
// sessions is where per-sessionId conversation history is loaded from and
// appended to (see session.go); a sessionId of "" makes every session
// operation a no-op. At the MCP level (internal/mcpserver/ask.go's
// AskInput.SessionID) an empty sessionId is now rejected — Miranda's own
// backend always injects its resolved conversation id (see
// miranda/docs/medical-card-session-injection.md) — but this Go-level
// parameter still legitimately accepts "" for callers that bypass MCP
// entirely and have no session to begin with, e.g. cmd/medical-dev's `ask`
// CLI, and for tests.
// maxIterations bounds how many tool-call round trips one Ask call may make
// before giving up (see agent_loop.go's Ask). A nil logger falls back to
// slog.Default().
func NewAsker(chat ChatProvider, registry *Registry, sessions *SessionStore, providerTimeout time.Duration, maxChunks, maxIterations int, logger *slog.Logger) *Asker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Asker{
		chat: chat, registry: registry, sessions: sessions,
		providerTimeout: providerTimeout, maxChunks: maxChunks, maxIterations: maxIterations, logger: logger,
	}
}
