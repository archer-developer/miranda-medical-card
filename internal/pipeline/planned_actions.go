package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// PlannedActionNotFoundError is returned by DeclinePlannedAction when there
// are no pending PlannedActions for the user at all, or none of them
// confidently matches the given text (see docs/adr/004-planned-actions.md
// §3). PendingDescriptions carries the user's current pending descriptions
// (empty in the first case) so the caller (internal/mcpserver) can surface
// them in PLANNED_ACTION_NOT_FOUND's message, letting Miranda ask the user
// to clarify instead of silently no-op'ing.
type PlannedActionNotFoundError struct {
	PendingDescriptions []string
}

func (e *PlannedActionNotFoundError) Error() string {
	if len(e.PendingDescriptions) == 0 {
		return "pipeline: decline planned action: no pending planned actions"
	}
	return fmt.Sprintf("pipeline: decline planned action: no confident match among pending actions: %s",
		strings.Join(e.PendingDescriptions, "; "))
}

// DeclinePlannedAction implements docs/mcp/08-planned-actions.md's
// medical.decline_planned_action: finds the single pending PlannedAction
// text most clearly refers to (via decline.Match, one small Structured LLM
// call over the user's current pending candidates — Miranda passes text
// exactly as the user said it, never a specific plannedActionId, same
// principle as medical.log_event) and marks it declined. Returns
// *PlannedActionNotFoundError (never a plain error) when nothing pending
// exists or nothing matched confidently.
func (p *Pipeline) DeclinePlannedAction(ctx context.Context, userID, text string) (normalization.PlannedAction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: text must not be empty")
	}

	pending, err := p.plannedActions.ListPending(ctx, userID)
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: list pending: %w", err)
	}
	descriptions := make([]string, len(pending))
	candidates := make([]decline.Candidate, len(pending))
	for i, a := range pending {
		descriptions[i] = a.Description
		candidates[i] = decline.Candidate{ID: a.ID, Description: a.Description, Type: a.Type}
	}
	if len(pending) == 0 {
		return normalization.PlannedAction{}, &PlannedActionNotFoundError{}
	}

	matchedID, err := decline.Match(ctx, p.provider,
		"The user is saying a planned medical action is no longer needed and should be cancelled.", text, candidates)
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: match: %w", err)
	}

	for _, a := range pending {
		if a.ID == matchedID && matchedID != "" {
			if err := p.plannedActions.MarkDeclined(ctx, a.ID, userID); err != nil {
				return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: %w", err)
			}
			a.Status = normalization.PlannedActionStatusDeclined
			return a, nil
		}
	}
	// Either the model omitted plannedActionId (no confident match), or —
	// despite the schema's enum constraint — returned an id outside the
	// candidate set; both are treated identically as "nothing matched"
	// rather than trusting an id that was never actually offered.
	return normalization.PlannedAction{}, &PlannedActionNotFoundError{PendingDescriptions: descriptions}
}
