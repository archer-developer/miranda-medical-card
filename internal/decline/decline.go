// Package decline implements medical.decline_planned_action's matching step
// (docs/adr/004-planned-actions.md §3): given the text a user said to
// cancel a planned action ("отмени прививку от бешенства") and their
// current pending PlannedActions, decide which single one — if any — they
// mean. One small Structured LLM call over an explicit, short candidate
// list (a user's pending actions), not a general-purpose search — the
// candidate set is always small, so this is closer to a disambiguation
// prompt than internal/search.
package decline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	llm "github.com/archer-developer/miranda-llm"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "planned_action_decline_match"

// Prompt instructs the model to pick at most one candidate by meaning, not
// exact wording — text and a PlannedAction.Description often use different
// words for the same thing ("укол от бешенства" vs "прививка от
// бешенства").
const Prompt = `You are matching a user's short request to cancel a planned medical action against a list of their own currently pending planned actions.

Given the user's text and a list of candidates (id + description), return the id of the single candidate the text most clearly refers to. The text may paraphrase the description — match by meaning, not exact wording.

Omit plannedActionId entirely if you are not confident any candidate is what the user means — never guess when it's ambiguous or none fit.`

// Candidate is one pending PlannedAction offered to the model.
type Candidate struct {
	ID          string
	Description string
	Type        string
}

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema —
// plannedActionId is constrained to candidateIDs so the model can only ever
// return an id it was actually offered, never invent one.
func Schema(candidateIDs []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plannedActionId": map[string]any{
				"type":        "string",
				"enum":        candidateIDs,
				"description": "The id of the matching candidate. Omit entirely if none confidently match.",
			},
		},
	}
}

// Result is the Go-side mirror of Schema.
type Result struct {
	PlannedActionID string `json:"plannedActionId,omitempty"`
}

// StructuredProvider is the subset of llm.StructuredProvider Match needs.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// Match returns the id of the single candidate text refers to, or "" if
// candidates is empty or the model found no confident match — never an
// error for "no match," only for an actual call/parse failure (mirroring
// internal/events.Extract's posture: an inconclusive result is a normal
// outcome here, not a failure).
func Match(ctx context.Context, provider StructuredProvider, text string, candidates []Candidate) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}

	ids := make([]string, len(candidates))
	var listing strings.Builder
	for i, c := range candidates {
		ids[i] = c.ID
		fmt.Fprintf(&listing, "- id=%s type=%s: %s\n", c.ID, c.Type, c.Description)
	}

	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: Prompt},
			{Role: llm.RoleUser, Content: fmt.Sprintf("User's text: %q\n\nCandidates:\n%s", text, listing.String())},
		},
		Schema:     Schema(ids),
		SchemaName: SchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return "", fmt.Errorf("decline: structured: %w", err)
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decline: structured: unmarshal result: %w", err)
	}
	return result.PlannedActionID, nil
}
