package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// PlannedActionNotFoundError is returned by DeclinePlannedAction/
// CompletePlannedAction when there are no pending PlannedActions for the
// user at all, or none of them confidently matches the given text (see
// docs/adr/004-planned-actions.md §3). PendingDescriptions carries the
// user's current pending descriptions (empty in the first case) so the
// caller (internal/mcpserver) can surface them in PLANNED_ACTION_NOT_FOUND's
// message, letting Miranda ask the user to clarify instead of silently
// no-op'ing.
type PlannedActionNotFoundError struct {
	PendingDescriptions []string
}

func (e *PlannedActionNotFoundError) Error() string {
	if len(e.PendingDescriptions) == 0 {
		return "pipeline: no pending planned actions"
	}
	return fmt.Sprintf("pipeline: no confident match among pending actions: %s",
		strings.Join(e.PendingDescriptions, "; "))
}

// findPendingMatch finds the single pending PlannedAction text most clearly
// refers to among userID's current pending actions (via decline.Match, one
// small Structured LLM call over the short candidate list — Miranda passes
// text exactly as the user said it, never a specific plannedActionId, same
// principle as medical.log_event). kind is substituted into decline.Match's
// prompt to say what's being confirmed (cancellation vs completion) so the
// model isn't matching blind. Returns *PlannedActionNotFoundError (never a
// plain error) when nothing pending exists or nothing matched confidently —
// shared by DeclinePlannedAction and CompletePlannedAction.
func (p *Pipeline) findPendingMatch(ctx context.Context, userID, text, kind string) (normalization.PlannedAction, error) {
	pending, err := p.plannedActions.ListPending(ctx, userID)
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("list pending: %w", err)
	}
	if len(pending) == 0 {
		return normalization.PlannedAction{}, &PlannedActionNotFoundError{}
	}
	descriptions := make([]string, len(pending))
	candidates := make([]decline.Candidate, len(pending))
	for i, a := range pending {
		descriptions[i] = a.Description
		candidates[i] = decline.Candidate{ID: a.ID, Description: a.Description, Type: a.Type}
	}

	matchedID, err := decline.Match(ctx, p.extractionProvider, kind, text, candidates)
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("match: %w", err)
	}
	for _, a := range pending {
		if a.ID == matchedID && matchedID != "" {
			return a, nil
		}
	}
	// Either the model omitted matchId (no confident match), or — despite
	// the schema's enum constraint — returned an id outside the candidate
	// set; both are treated identically as "nothing matched" rather than
	// trusting an id that was never actually offered.
	return normalization.PlannedAction{}, &PlannedActionNotFoundError{PendingDescriptions: descriptions}
}

// DeclinePlannedAction implements docs/mcp/08-planned-actions.md's
// medical.decline_planned_action: finds the pending PlannedAction text
// refers to and marks it declined.
func (p *Pipeline) DeclinePlannedAction(ctx context.Context, userID, text string) (normalization.PlannedAction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: text must not be empty")
	}

	a, err := p.findPendingMatch(ctx, userID, text,
		"The user is saying a planned medical action is no longer needed and should be cancelled.")
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: %w", err)
	}
	if err := p.plannedActions.MarkDeclined(ctx, a.ID, userID); err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: decline planned action: %w", err)
	}
	a.Status = normalization.PlannedActionStatusDeclined
	return a, nil
}

// CompletePlannedAction implements docs/mcp/08-planned-actions.md's
// medical.complete_planned_action: finds the pending PlannedAction text
// refers to and marks it completed on the user's own say-so — the manual
// counterpart to the automatic document-matching completion described in
// docs/domain/14-planned-action.md §4, for the case that can't close
// itself (a self-reported fact never produces a LabResult/Procedure to
// match against, see docs/adr/004-planned-actions.md §2). Unlike automatic
// completion, MatchedDocumentID/MatchedEntityID stay empty — only
// MatchedAt is set, to the moment of confirmation.
func (p *Pipeline) CompletePlannedAction(ctx context.Context, userID, text string) (normalization.PlannedAction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: complete planned action: text must not be empty")
	}

	a, err := p.findPendingMatch(ctx, userID, text,
		"The user is saying they have completed a planned medical action themselves.")
	if err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: complete planned action: %w", err)
	}
	now := time.Now()
	if err := p.plannedActions.MarkCompletedManually(ctx, a.ID, userID, now); err != nil {
		return normalization.PlannedAction{}, fmt.Errorf("pipeline: complete planned action: %w", err)
	}
	a.Status = normalization.PlannedActionStatusCompleted
	a.MatchedAt = &now
	return a, nil
}
