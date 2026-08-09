package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	llm "github.com/archer-developer/miranda-llm"
)

// StructuredProvider is the subset of llm.StructuredProvider both Plan and
// GenerateAnswer need.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// PlannerSchemaName is passed as llm.StructuredRequest.SchemaName.
const PlannerSchemaName = "medical_ask_planner"

// PlannerSelection is one chosen Provider and the request parameters to
// call it with — mirrors docs/architecture/03-knowledge-providers.md §6-7's
// Planner/Execution Plan. Every field beyond Provider/Reason is optional
// and provider-specific (see KnowledgeRequest's doc comment for which
// provider reads which).
type PlannerSelection struct {
	Provider      string `json:"provider"`
	Reason        string `json:"reason,omitempty"`
	IndicatorName string `json:"indicatorName,omitempty"`
	Structure     string `json:"structure,omitempty"`
	Parameter     string `json:"parameter,omitempty"`
	SearchQuery   string `json:"searchQuery,omitempty"`
}

type plannerResult struct {
	Providers []PlannerSelection `json:"providers"`
}

const plannerPromptTemplate = `You are the Planner for a medical knowledge assistant. Your only job is to decide which knowledge sources (Providers) are needed to answer the user's question — you never answer the question yourself, never look up any data, and never draw medical conclusions.

Available Providers:
%s

Rules:
- Select every Provider whose description matches what the question needs — err toward including a relevant one rather than omitting it, but do not select Providers with no plausible relevance.
- For "lab_results", set indicatorName only if the question names a specific, identifiable lab indicator (e.g. "ALT", "холестерин"); omit it to get all indicators.
- For "instrumental_findings", set BOTH structure and parameter only if the question names a specific anatomical structure and measured parameter; otherwise omit this provider entirely, since it returns nothing without both.
- For "documents" and "embeddings", set searchQuery to a short, focused phrase (a few words) capturing what to search for — not the entire question verbatim.
- Return an empty providers array if the question doesn't require any medical history lookup at all.`

// Plan runs the Planner (docs/architecture/03-knowledge-providers.md §6):
// one Structured call choosing which registered Providers to invoke, and
// with what parameters, for question. escalate, if non-nil, is tried once
// if provider hard-errors (see structuredWithEscalation) — e.g. falling
// back from Gemini to Claude on a rate limit, matching llm.yaml's
// planner_provider escalation config, if set. A nil logger falls back to
// slog.Default().
func Plan(ctx context.Context, provider, escalate StructuredProvider, question string, registry *Registry, logger *slog.Logger) ([]PlannerSelection, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: fmt.Sprintf(plannerPromptTemplate, describeProviders(registry))},
			{Role: llm.RoleUser, Content: question},
		},
		Schema:     plannerSchema(registry),
		SchemaName: PlannerSchemaName,
	}

	raw, err := structuredWithEscalation(ctx, provider, escalate, req, "plan", logger)
	if err != nil {
		return nil, fmt.Errorf("ask: plan: %w", err)
	}
	var result plannerResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("ask: plan: unmarshal result: %w", err)
	}

	// A selection naming a provider outside the Registry (a hallucinated or
	// stale name) is dropped rather than causing a lookup failure later —
	// the Planner only ever chooses from Registry.Names(), enforced here
	// defensively rather than trusted blindly.
	valid := result.Providers[:0]
	for _, s := range result.Providers {
		if _, ok := registry.Get(s.Provider); ok {
			valid = append(valid, s)
		}
	}
	return valid, nil
}

func describeProviders(registry *Registry) string {
	var b strings.Builder
	for _, m := range registry.Metadata() {
		fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
	}
	return b.String()
}

func plannerSchema(registry *Registry) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"providers": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"provider":      map[string]any{"type": "string", "enum": registry.Names()},
						"reason":        map[string]any{"type": "string"},
						"indicatorName": map[string]any{"type": "string"},
						"structure":     map[string]any{"type": "string"},
						"parameter":     map[string]any{"type": "string"},
						"searchQuery":   map[string]any{"type": "string"},
					},
					"required": []string{"provider"},
				},
			},
		},
		"required": []string{"providers"},
	}
}
