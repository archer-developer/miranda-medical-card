package ask

import (
	"context"
	"time"
)

// KnowledgeChunk mirrors docs/architecture/03-knowledge-providers.md §12 —
// the one shared shape every Provider returns, deliberately small and
// human-readable (that doc's "Хороший пример" vs "Плохой пример": a few
// readable lines, never raw internal structures).
//
// DocumentID/EventID are typed fields rather than folded into Metadata
// (which docs describe as map[string]any) because every consumer of a
// chunk in this package needs exactly these two identifiers to build
// docs/mcp/04-medical.md §5's `sources` — making them first-class avoids a
// map[string]any type-assertion at every call site for the one thing that
// actually matters here. Metadata is kept too, for whatever a specific
// Provider wants to attach that this package doesn't need to understand.
type KnowledgeChunk struct {
	Source     string
	Title      string
	Content    string
	Confidence float64
	DocumentID string
	EventID    string
	Metadata   map[string]any
}

// KnowledgeRequest is the structured description of what's needed that the
// Planner hands to a Provider (docs/architecture/03-knowledge-providers.md
// §11) — one shared shape across every Provider (rather than a distinct
// Go type per Provider) since every field here is optional and each
// Provider reads only the ones relevant to it; see each provider's own
// doc comment for which fields it uses.
type KnowledgeRequest struct {
	UserID string
	// Query is the free-text question, or a Planner-refined search phrase
	// — used by DocumentProvider (FTS) and EmbeddingProvider (semantic).
	Query string
	// IndicatorName narrows LabProvider to one indicator's history; empty
	// means "every indicator" (bounded by Limit).
	IndicatorName string
	// DocumentID narrows LabProvider to results from one specific
	// document/event — the id an earlier timeline/documents tool call
	// returned for it (see KnowledgeChunk.DocumentID and formatToolResult).
	// Takes priority over IndicatorName when both are set. Unlike every
	// other field here, the value is model-supplied and not otherwise tied
	// to UserID, so LabProvider.Collect re-checks ownership itself rather
	// than trusting it (see filterByOwner).
	DocumentID string
	// Structure/Parameter narrow InstrumentalFindingProvider — both must be
	// set (see that provider's doc comment for why partial values return
	// nothing).
	Structure string
	Parameter string
	// From/To bound TimelineProvider and, where relevant, others.
	From, To *time.Time
	Limit    int
}

// ProviderMetadata mirrors docs/architecture/03-knowledge-providers.md §9 —
// what the Planner sees when deciding which Providers to use.
type ProviderMetadata struct {
	Name        string
	Description string
}

// Provider mirrors docs/architecture/03-knowledge-providers.md §10.
type Provider interface {
	Metadata() ProviderMetadata
	Collect(ctx context.Context, request KnowledgeRequest) ([]KnowledgeChunk, error)
}

// Registry mirrors docs/architecture/03-knowledge-providers.md §8 — the
// closed set of Providers the Planner can choose from. A plain map wrapped
// in a small type (rather than a bare map[string]Provider) so
// Names()/Descriptions() have one obvious home instead of being
// recalculated at every call site (the Planner prompt needs both).
type Registry struct {
	providers map[string]Provider
	order     []string // registration order, used for a stable Planner prompt and Context Builder's provider-priority tiebreak
}

// NewRegistry builds a Registry from providers, in priority order (see
// docs/architecture/03-knowledge-providers.md §17 for the recommended
// order this package's wiring follows).
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		name := p.Metadata().Name
		r.providers[name] = p
		r.order = append(r.order, name)
	}
	return r
}

// Get returns the provider registered under name, or nil, false.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names returns every registered provider's name, in registration
// (priority) order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Metadata returns every registered provider's ProviderMetadata, in
// registration order — what Plan feeds to the Planner prompt.
func (r *Registry) Metadata() []ProviderMetadata {
	result := make([]ProviderMetadata, len(r.order))
	for i, name := range r.order {
		result[i] = r.providers[name].Metadata()
	}
	return result
}

// PriorityIndex returns name's position in registration order (lower is
// higher priority), or len(order) if unregistered — used by the Context
// Builder to break Confidence ties per
// docs/architecture/03-knowledge-providers.md §17.
func (r *Registry) PriorityIndex(name string) int {
	for i, n := range r.order {
		if n == name {
			return i
		}
	}
	return len(r.order)
}
