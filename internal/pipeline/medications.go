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

// MedicationNotFoundError is returned by StartMedication/CompleteMedication
// when the user has no Medication in the status being searched (prescribed
// for StartMedication, active for CompleteMedication) at all, or none of
// them confidently matches the given text — mirrors DiagnosisNotFoundError
// (see that type's doc comment for why this carries the user's current
// candidates rather than just failing silently).
type MedicationNotFoundError struct {
	CurrentDrugNames []string
}

func (e *MedicationNotFoundError) Error() string {
	if len(e.CurrentDrugNames) == 0 {
		return "pipeline: no matching medications"
	}
	return fmt.Sprintf("pipeline: no confident match among current medications: %s",
		strings.Join(e.CurrentDrugNames, "; "))
}

// findMedicationMatch finds the single Medication in status wantStatus that
// text most clearly refers to (via decline.Match, the same small Structured
// LLM call over a short candidate list DeclinePlannedAction/ResolveDiagnosis
// use — Miranda passes text exactly as the user said it, never a specific
// medicationId), shared by StartMedication and CompleteMedication. Candidates
// come from profile.ResolveLatestMedications, not a raw
// ListByUser(Status: wantStatus) listing — a drug can have an old superseded
// row in wantStatus from an earlier document that a later one has already
// moved past, and only the resolver knows which row is actually current.
// Returns *MedicationNotFoundError (never a plain error) when nothing in
// wantStatus exists or nothing matched confidently.
func (p *Pipeline) findMedicationMatch(ctx context.Context, userID, text, kind string, wantStatus normalization.MedicationStatus) (normalization.Medication, error) {
	all, err := p.medications.ListByUser(ctx, userID, storage.MedicationFilter{})
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("list: %w", err)
	}
	latest := profile.ResolveLatestMedications(all)
	var current []normalization.Medication
	for _, m := range latest {
		if m.Status == wantStatus {
			current = append(current, m)
		}
	}
	if len(current) == 0 {
		return normalization.Medication{}, &MedicationNotFoundError{}
	}

	names := make([]string, len(current))
	candidates := make([]decline.Candidate, len(current))
	for i, m := range current {
		names[i] = m.DrugName
		candidates[i] = decline.Candidate{ID: m.ID, Description: m.DrugName, Type: string(m.Status)}
	}

	matchedID, err := decline.Match(ctx, p.extractionProvider, kind, text, candidates)
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("match: %w", err)
	}
	for _, m := range current {
		if m.ID == matchedID && matchedID != "" {
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

// StartMedication implements docs/mcp/10-medications.md's
// medical.start_medication: finds the single prescribed-but-not-yet-started
// Medication text refers to and confirms intake actually began now — the
// manual counterpart to Extraction directly setting status=active when a
// document itself already confirms intake started (see
// docs/domain/06-medication.md §3). StartedAt is set to the moment of this
// call, not any date the source document recorded, since the whole point of
// this tool is that the two can differ.
func (p *Pipeline) StartMedication(ctx context.Context, userID, text string) (normalization.Medication, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.Medication{}, fmt.Errorf("pipeline: start medication: text must not be empty")
	}

	m, err := p.findMedicationMatch(ctx, userID, text,
		"The user is saying they have actually started taking one of their prescribed medications.",
		normalization.MedicationStatusPrescribed)
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: start medication: %w", err)
	}

	now := time.Now().UTC()
	if err := p.medications.MarkStartedManually(ctx, m.ID, userID, now); err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: start medication: %w", err)
	}
	m.Status = normalization.MedicationStatusActive
	m.StartedAt = &now
	return m, nil
}

// CompleteMedication implements docs/mcp/10-medications.md's
// medical.complete_medication: finds the single currently active Medication
// text most clearly refers to and marks it completed on the user's own
// say-so.
func (p *Pipeline) CompleteMedication(ctx context.Context, userID, text string) (normalization.Medication, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: text must not be empty")
	}

	m, err := p.findMedicationMatch(ctx, userID, text,
		"The user is saying they have finished taking one of their current medications.",
		normalization.MedicationStatusActive)
	if err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: %w", err)
	}

	now := time.Now().UTC()
	if err := p.medications.MarkEndedManually(ctx, m.ID, userID, now); err != nil {
		return normalization.Medication{}, fmt.Errorf("pipeline: complete medication: %w", err)
	}
	m.Status = normalization.MedicationStatusCompleted
	m.EndedAt = &now
	return m, nil
}
