package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// MedicationNotFoundError is returned by CompleteMedication when the user
// has no currently active Medication at all, or none of them confidently
// matches the given text — mirrors DiagnosisNotFoundError (see that type's
// doc comment for why this carries the user's current candidates rather
// than just failing silently).
type MedicationNotFoundError struct {
	CurrentDrugNames []string
}

func (e *MedicationNotFoundError) Error() string {
	if len(e.CurrentDrugNames) == 0 {
		return "pipeline: complete medication: no active medications"
	}
	return fmt.Sprintf("pipeline: complete medication: no confident match among current medications: %s",
		strings.Join(e.CurrentDrugNames, "; "))
}

// CompleteMedication implements docs/mcp/10-medications.md's
// medical.complete_medication: finds the single currently active Medication
// text most clearly refers to (via decline.Match, the same small Structured
// LLM call over a short candidate list DeclinePlannedAction/ResolveDiagnosis
// use — Miranda passes text exactly as the user said it, e.g. "я закончил
// курс антибиотиков", never a specific medicationId) and marks it completed
// on the user's own say-so. The candidate set is profile.ResolveActiveMedications's
// output, not a raw ListByUser(Status: "active") listing — a drug can have
// an old superseded "active" row from an earlier document that a later one
// already discontinued, and only the resolver knows which row is actually
// current. Returns *MedicationNotFoundError (never a plain error) when
// nothing active exists or nothing matched confidently.
func (p *Pipeline) CompleteMedication(ctx context.Context, userID, text string) (normalization.Medication, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: text must not be empty")
	}

	all, err := p.medications.ListByUser(ctx, userID, storage.MedicationFilter{})
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: list: %w", err)
	}
	current := profile.ResolveActiveMedications(all)
	if len(current) == 0 {
		return normalization.Medication{}, &MedicationNotFoundError{}
	}

	names := make([]string, len(current))
	candidates := make([]decline.Candidate, len(current))
	for i, m := range current {
		names[i] = m.DrugName
		candidates[i] = decline.Candidate{ID: m.ID, Description: m.DrugName, Type: m.Status}
	}

	matchedID, err := decline.Match(ctx, p.extractionProvider,
		"The user is saying they have finished taking one of their current medications.", text, candidates)
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: match: %w", err)
	}

	for _, m := range current {
		if m.ID == matchedID && matchedID != "" {
			now := time.Now().UTC()
			if err := p.medications.MarkEndedManually(ctx, m.ID, userID, now); err != nil {
				return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: %w", err)
			}
			m.Status = "completed"
			m.EndedAt = &now
			m.ConfirmedEndedAt = &now
			return m, nil
		}
	}
	// Either the model omitted matchId (no confident match), or — despite
	// the schema's enum constraint — returned an id outside the candidate
	// set; both are treated identically as "nothing matched" rather than
	// trusting an id that was never actually offered (mirrors
	// ResolveDiagnosis's own posture).
	return normalization.Medication{}, &MedicationNotFoundError{CurrentDrugNames: names}
}
