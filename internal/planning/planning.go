// Package planning defines the raw, LLM-facing shape of a future medical
// action recommended in a document or a self-reported note ("Повторный
// анализ крови через полгода", "сдать кровь на глюкозу", "прививка от
// бешенства в течение месяца") — see docs/domain/14-planned-action.md and
// docs/adr/004-planned-actions.md.
//
// This type and its Schema are shared between internal/extraction (Stage
// 2a's structured document extraction) and internal/events (self-reported
// notes' structured extraction) so the same JSON Schema fragment and Go
// struct aren't duplicated across those two otherwise-independent
// extraction pipelines. internal/normalization is the only place that turns
// this raw, still-relative shape into an absolute-dated, canonicalized
// normalization.PlannedAction — see docs/adr/004-planned-actions.md's
// "Диапазон дат, а не точечная дата" for why the LLM is deliberately never
// asked to compute a calendar date itself.
package planning

// Types is the enum PlannedAction.Type must be one of. It reuses
// internal/extraction.Procedure's own type enum (surgery/examination/
// hospitalization/vaccination/consultation/other) plus "lab_test", since a
// planned action completed by a LabResult (rather than a Procedure) needs
// its own bucket — see internal/planmatch's matching key, which switches on
// this exact value to decide whether to compare against LabResult or
// Procedure entities.
var Types = []string{"lab_test", "surgery", "examination", "hospitalization", "vaccination", "consultation", "other"}

// DueUnits is the enum PlannedAction.DueUnit must be one of.
var DueUnits = []string{"day", "week", "month", "year"}

// PlannedAction is the raw shape an LLM extracts a future recommendation
// into — deliberately never an absolute date, only a rough relative
// timeframe (DueAmountMin/Max + DueUnit), so the actual date arithmetic
// stays deterministic Go code in internal/normalization rather than
// non-deterministic LLM output.
type PlannedAction struct {
	// Type classifies what kind of action this is — see Types. "lab_test"
	// is the one addition to Procedure's own enum (see Types' doc comment).
	Type string `json:"type"`
	// Description is a short, human-readable description of the action,
	// e.g. "Повторный анализ глюкозы крови".
	Description string `json:"description"`
	// ReferenceText is the recommendation's original phrasing, verbatim,
	// kept for display/traceability — not used for matching.
	ReferenceText string `json:"referenceText,omitempty"`
	// RelatedIndicatorName is the lab indicator/test name as written, only
	// for Type == "lab_test" — e.g. "глюкоза". Used by normalization to
	// build a matching key against a later LabResult.IndicatorName via the
	// same canonicalization/alias resolution LabResult itself already goes
	// through.
	RelatedIndicatorName string `json:"relatedIndicatorName,omitempty"`
	// RelatedProcedureName is the procedure/vaccine/visit name as written,
	// for every Type other than "lab_test" — e.g. "прививка от бешенства".
	// Used by normalization to build a matching key against a later
	// Procedure.Name.
	RelatedProcedureName string `json:"relatedProcedureName,omitempty"`
	// DueAmountMin is the lower bound of the relative timeframe. Optional —
	// when omitted, normalization treats it as equal to DueAmountMax (a
	// plain "in 6 months" rather than an approximate "через полгода").
	DueAmountMin float64 `json:"dueAmountMin,omitempty"`
	// DueAmountMax is the upper bound (or the single value, if
	// DueAmountMin is omitted) of the relative timeframe. Required whenever
	// a timeframe is stated at all; omit the whole PlannedAction instead of
	// setting this to 0 when the source text states no timeframe (see
	// docs/adr/004-planned-actions.md — a PlannedAction with no due date is
	// still valid, it just never expires).
	DueAmountMax float64 `json:"dueAmountMax,omitempty"`
	// DueUnit is the unit DueAmountMin/Max are expressed in — see DueUnits.
	// Required whenever DueAmountMax is set.
	DueUnit string `json:"dueUnit,omitempty"`
}

// SchemaObject returns the JSON Schema for a single PlannedAction object,
// suitable for embedding either as the "items" of an array property (see
// internal/extraction.Schema's "plannedActions") or directly as a single
// object property (see internal/events.Schema's "plannedAction").
func SchemaObject() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type":                 map[string]any{"type": "string", "enum": Types},
			"description":          map[string]any{"type": "string"},
			"referenceText":        map[string]any{"type": "string", "description": "The recommendation's original phrasing, verbatim."},
			"relatedIndicatorName": map[string]any{"type": "string", "description": "Lab indicator/test name as written — only when type is lab_test."},
			"relatedProcedureName": map[string]any{"type": "string", "description": "Procedure/vaccine/visit name as written — for every type other than lab_test."},
			"dueAmountMin":         map[string]any{"type": "number", "description": "Lower bound of the timeframe, if the phrase is a range (e.g. 'через полгода' → min 5). Omit for a precise timeframe."},
			"dueAmountMax":         map[string]any{"type": "number", "description": "Upper bound, or the single value if dueAmountMin is omitted."},
			"dueUnit":              map[string]any{"type": "string", "enum": DueUnits},
		},
		"required": []string{"type", "description"},
	}
}
