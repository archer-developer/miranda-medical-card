// Package decline implements a small, reusable "which of the user's few
// current <thing>s does this free-text message refer to" matcher. Built for
// medical.decline_planned_action's matching step (docs/adr/004-planned-actions.md
// §3: given "отмени прививку от бешенства" and the user's current pending
// PlannedActions, decide which single one — if any — they mean), and reused
// as-is by medical.resolve_diagnosis (given "да, ОРВИ прошла" and the
// user's current non-resolved Diagnoses, decide which one). One small
// Structured LLM call over an explicit, short candidate list, not a
// general-purpose search — the candidate set is always small, so this is
// closer to a disambiguation prompt than internal/search.
package decline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	llm "github.com/archer-developer/miranda-llm"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "candidate_text_match"

// promptTemplate instructs the model to pick at most one candidate by
// meaning, not exact wording — free text and a Candidate.Description often
// use different words for the same thing ("укол от бешенства" vs
// "прививка от бешенства", "ОРВИ прошла" vs "Острая респираторная вирусная
// инфекция"). %s is filled in by Match with a one-line description of what
// kind of thing is being matched and why (see Match's kind parameter) —
// kept out of the candidate list itself so the model isn't tempted to
// invent a match just because something merely resembles a candidate.
const promptTemplate = `You are matching a user's short free-text message against a list of their own current candidate items.

%s

Given the user's text and a list of candidates (id + description), return the id of the single candidate the text most clearly refers to. The text may paraphrase the description — match by meaning, not exact wording.

Omit matchId entirely if you are not confident any candidate is what the user means — never guess when it's ambiguous or none fit.`

// Candidate is one current item (a pending PlannedAction, a non-resolved
// Diagnosis, ...) offered to the model.
type Candidate struct {
	ID          string
	Description string
	Type        string
}

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema —
// matchId is constrained to candidateIDs so the model can only ever return
// an id it was actually offered, never invent one.
func Schema(candidateIDs []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"matchId": map[string]any{
				"type":        "string",
				"enum":        candidateIDs,
				"description": "The id of the matching candidate. Omit entirely if none confidently match.",
			},
		},
	}
}

// Result is the Go-side mirror of Schema.
type Result struct {
	MatchID string `json:"matchId,omitempty"`
}

// StructuredProvider is the subset of llm.StructuredProvider Match needs.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// Match returns the id of the single candidate text refers to, or "" if
// candidates is empty or the model found no confident match — never an
// error for "no match," only for an actual call/parse failure (mirroring
// internal/events.Extract's posture: an inconclusive result is a normal
// outcome here, not a failure). kind is a one-line description of what's
// being matched and why, substituted into the prompt so the model doesn't
// have to guess the task from the candidate list alone — e.g. "The user is
// saying a planned medical action is no longer needed and should be
// cancelled." or "The user is saying one of their current diagnoses has
// now resolved."
func Match(ctx context.Context, provider StructuredProvider, kind, text string, candidates []Candidate) (string, error) {
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
			{Role: llm.RoleSystem, Content: fmt.Sprintf(promptTemplate, kind)},
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
	return result.MatchID, nil
}
