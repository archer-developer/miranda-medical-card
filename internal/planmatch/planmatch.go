// Package planmatch implements the auto-completion half of Planned Actions
// (docs/adr/004-planned-actions.md): given the entities Normalization just
// produced for one document, decide which still-pending PlannedActions are
// now satisfied. A pure Domain Service, no I/O, no LLM — same shape as
// internal/timeline — because both sides of the comparison
// (PlannedAction.MatchIndicatorName/MatchProcedureName and
// LabResult.IndicatorName/Procedure.Name) are already canonicalized
// identically by internal/normalization, so this is a plain equality scan,
// never fuzzy matching.
package planmatch

import "github.com/archer-developer/miranda-medical-card/internal/normalization"

// Completion is one pending PlannedAction that normalized's entities
// satisfy.
type Completion struct {
	PlannedActionID string
	MatchedEntityID string
}

// Match returns a Completion for every entry in pending whose matching key
// (Type + MatchIndicatorName, for "lab_test"; Type + MatchProcedureName,
// otherwise) is found among normalized's freshly-normalized LabResults/
// Procedures. A pending action with no match key at all (the source text
// stated no specific test/procedure name — see
// docs/adr/004-planned-actions.md's "сдать кровь на глюкозу" vs. a plain
// "сдать анализы" example) never matches — there's nothing to compare
// against, and matching it against literally any lab result would produce
// false completions.
//
// At most one Completion per pending action (the first matching entity
// found); an entity may satisfy more than one pending action (e.g. two
// separate recommendations for the same repeat test).
func Match(normalized normalization.Result, pending []normalization.PlannedAction) []Completion {
	var completions []Completion
	for _, p := range pending {
		if p.Type == "lab_test" {
			if p.MatchIndicatorName == "" {
				continue
			}
			for _, l := range normalized.LabResults {
				if l.IndicatorName == p.MatchIndicatorName {
					completions = append(completions, Completion{PlannedActionID: p.ID, MatchedEntityID: l.ID})
					break
				}
			}
			continue
		}
		if p.MatchProcedureName == "" {
			continue
		}
		for _, proc := range normalized.Procedures {
			if proc.Type == p.Type && proc.Name == p.MatchProcedureName {
				completions = append(completions, Completion{PlannedActionID: p.ID, MatchedEntityID: proc.ID})
				break
			}
		}
	}
	return completions
}
