package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// DiagnosisNotFoundError is returned by ResolveDiagnosis when the user has
// no non-resolved Diagnoses at all, or none of them confidently matches the
// given text — mirrors PlannedActionNotFoundError (see that type's doc
// comment for why this carries the user's current candidates rather than
// just failing silently).
type DiagnosisNotFoundError struct {
	CurrentNames []string
}

func (e *DiagnosisNotFoundError) Error() string {
	if len(e.CurrentNames) == 0 {
		return "pipeline: resolve diagnosis: no non-resolved diagnoses"
	}
	return fmt.Sprintf("pipeline: resolve diagnosis: no confident match among current diagnoses: %s",
		strings.Join(e.CurrentNames, "; "))
}

// ResolveDiagnosis implements docs/mcp/09-diagnoses.md's
// medical.resolve_diagnosis: finds the single non-resolved Diagnosis text
// most clearly refers to (via decline.Match, the same small Structured LLM
// call over a short candidate list DeclinePlannedAction uses — Miranda
// passes text exactly as the user said it, e.g. "да, ОРВИ прошла", never a
// specific diagnosisId) and marks it resolved. Returns
// *DiagnosisNotFoundError (never a plain error) when nothing non-resolved
// exists or nothing matched confidently.
func (p *Pipeline) ResolveDiagnosis(ctx context.Context, userID, text string) (normalization.Diagnosis, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: text must not be empty")
	}

	all, err := p.diagnoses.ListByUser(ctx, userID)
	if err != nil {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: list: %w", err)
	}
	var current []normalization.Diagnosis
	for _, d := range all {
		if d.Status != "resolved" {
			current = append(current, d)
		}
	}
	if len(current) == 0 {
		return normalization.Diagnosis{}, &DiagnosisNotFoundError{}
	}

	names := make([]string, len(current))
	candidates := make([]decline.Candidate, len(current))
	for i, d := range current {
		names[i] = d.Name
		candidates[i] = decline.Candidate{ID: d.ID, Description: d.Name, Context: d.Status}
	}

	matchedID, err := decline.Match(ctx, p.extractionProvider,
		"The user is saying one of their current diagnoses has now resolved.", text, candidates)
	if err != nil {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: match: %w", err)
	}

	for _, d := range current {
		if d.ID == matchedID && matchedID != "" {
			now := time.Now().UTC()
			reasoning := fmt.Sprintf("Пользователь подтвердил разрешение диагноза в диалоге: %q.", text)
			if err := p.diagnoses.MarkResolved(ctx, d.ID, userID, now, reasoning); err != nil {
				return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: %w", err)
			}
			d.Status = "resolved"
			d.ActualResolutionAt = &now
			d.StatusReasoning = reasoning
			return d, nil
		}
	}
	// Either the model omitted matchId (no confident match), or — despite
	// the schema's enum constraint — returned an id outside the candidate
	// set; both are treated identically as "nothing matched" rather than
	// trusting an id that was never actually offered (mirrors
	// DeclinePlannedAction's own posture).
	return normalization.Diagnosis{}, &DiagnosisNotFoundError{CurrentNames: names}
}
