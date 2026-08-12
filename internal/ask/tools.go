package ask

import (
	"encoding/json"
	"fmt"
	"time"

	llm "github.com/archer-developer/miranda-llm"
)

// toolDateLayout is the date format the agent loop's tool schemas ask the
// model for (and decodeKnowledgeRequest parses) — matches medical-dev/CLI
// conventions elsewhere in this codebase (e.g. providers.go's formatDate).
const toolDateLayout = "2006-01-02"

// toolDefsForRegistry turns every registered ask.Provider into an
// llm.ToolDef the agent loop can offer the model — one tool per Provider,
// in registry order. Unlike the old Planner's single shared JSON schema
// (planner.go, removed) where every field was optional on every provider,
// each tool here exposes only the KnowledgeRequest fields that specific
// Provider actually reads (see toolParameters), which lets the schema
// itself enforce contracts a prompt could previously only ask nicely for —
// e.g. instrumental_findings requiring both structure and parameter.
func toolDefsForRegistry(r *Registry) []llm.ToolDef {
	meta := r.Metadata()
	defs := make([]llm.ToolDef, len(meta))
	for i, m := range meta {
		defs[i] = toolDefFor(m)
	}
	return defs
}

func toolDefFor(m ProviderMetadata) llm.ToolDef {
	return llm.ToolDef{
		Name:        m.Name,
		Description: m.Description,
		Parameters:  toolParameters(m.Name),
	}
}

// toolParameters returns providerName's JSON Schema tool parameters. The
// switch is keyed on the registry name each provider registers itself
// under (see providers.go/search_providers.go's Metadata() methods) —
// providers with no case here (medications, diagnoses, procedures,
// profile) fall through to the no-properties default, since none of them
// read anything beyond KnowledgeRequest.UserID (which is never
// LLM-controlled — see decodeKnowledgeRequest).
func toolParameters(providerName string) map[string]any {
	switch providerName {
	case "timeline", "self_reported_events":
		return objectSchema(map[string]any{
			"from":  dateProperty("Only include events on or after this date."),
			"to":    dateProperty("Only include events on or before this date."),
			"limit": limitProperty(),
		}, nil)
	case "lab_results":
		return objectSchema(map[string]any{
			"indicatorName": map[string]any{
				"type":        "string",
				"description": `A specific lab indicator (e.g. "ALT", "холестерин"). Omit to get every indicator's history.`,
			},
			"limit": limitProperty(),
		}, nil)
	case "instrumental_findings":
		return objectSchema(map[string]any{
			"structure": map[string]any{
				"type":        "string",
				"description": `The anatomical structure examined (e.g. "печень", "щитовидная железа").`,
			},
			"parameter": map[string]any{
				"type":        "string",
				"description": `The measured parameter (e.g. "объём", "эхогенность").`,
			},
		}, []string{"structure", "parameter"})
	case "documents", "embeddings":
		return objectSchema(map[string]any{
			"searchQuery": map[string]any{
				"type":        "string",
				"description": "A short, focused search phrase capturing what to look for — not the entire question verbatim.",
			},
			"limit": limitProperty(),
		}, []string{"searchQuery"})
	default:
		return objectSchema(map[string]any{}, nil)
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func dateProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description + " Format: YYYY-MM-DD."}
}

func limitProperty() map[string]any {
	return map[string]any{"type": "integer", "description": "Maximum number of results, most recent first. Omit for the default."}
}

// toolCallArgs is the union of every tool parameter across every provider
// (see toolParameters) — decoded generically rather than per-provider,
// since each tool's own schema already constrains which fields the model
// can actually populate for a given call; a field absent from that
// provider's schema simply never gets set by the model and is ignored here.
type toolCallArgs struct {
	From          string `json:"from"`
	To            string `json:"to"`
	Limit         int    `json:"limit"`
	IndicatorName string `json:"indicatorName"`
	Structure     string `json:"structure"`
	Parameter     string `json:"parameter"`
	SearchQuery   string `json:"searchQuery"`
}

// decodeKnowledgeRequest parses a tool call's raw JSON arguments into a
// KnowledgeRequest for providerName. UserID is deliberately left unset
// here — the caller (executeToolCall) injects it from the MCP-authenticated
// subjectID, never from the model-supplied JSON, so a tool call can never
// name a different user's data even if the model hallucinated a userId
// argument (no tool schema here exposes one).
func decodeKnowledgeRequest(providerName, argsJSON string) (KnowledgeRequest, error) {
	var args toolCallArgs
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return KnowledgeRequest{}, fmt.Errorf("ask: decode %s tool call arguments: %w", providerName, err)
		}
	}

	req := KnowledgeRequest{
		Query:         args.SearchQuery,
		IndicatorName: args.IndicatorName,
		Structure:     args.Structure,
		Parameter:     args.Parameter,
		Limit:         args.Limit,
	}
	if args.From != "" {
		t, err := time.Parse(toolDateLayout, args.From)
		if err != nil {
			return KnowledgeRequest{}, fmt.Errorf("ask: %s: parse from date %q: %w", providerName, args.From, err)
		}
		req.From = &t
	}
	if args.To != "" {
		t, err := time.Parse(toolDateLayout, args.To)
		if err != nil {
			return KnowledgeRequest{}, fmt.Errorf("ask: %s: parse to date %q: %w", providerName, args.To, err)
		}
		req.To = &t
	}
	return req, nil
}
