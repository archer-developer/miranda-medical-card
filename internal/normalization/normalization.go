// Package normalization implements the Normalization stage of the
// Processing Pipeline (see docs/architecture/02-processing-pipeline.md §6):
// turning one document's extraction.Result into the domain entities
// described in docs/domain/ (Diagnosis, Medication, LabResult,
// InstrumentalFinding, Procedure, Allergy, VitalSign).
//
// Deliberately document-scoped only: this package does not attempt
// cross-document entity resolution (e.g. deciding whether a Medication here
// is "the same course" as one from an earlier document) — see
// docs/architecture/02-processing-pipeline.md §6 "Почему нет постоянных
// междокументных связей" for why that's read-time logic (Medication
// Resolver / Diagnosis Resolver, see docs/domain/01-overview.md §7), not
// Normalization's job. What this package does do: assign generated ids,
// parse/validate dates, carry every extraction field through to its
// corresponding domain field 1:1 (no name canonicalization yet — see
// canonicalize's doc comment for the specific, still-open gap), and —
// the one deliberate exception to "document-scoped only" — dimensional
// unit normalization for LabResult/InstrumentalFinding (see units.go for
// why picking a canonical unit is mechanical enough not to have the same
// problems as entity linking).
package normalization

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/planning"
)

const dateLayout = "2006-01-02"

// Diagnosis mirrors docs/domain/07-diagnosis-and-allergy.md §2.
type Diagnosis struct {
	ID          string
	UserID      string
	DocumentID  string
	Name        string
	Code        string
	CodeSystem  string
	DiagnosedAt *time.Time
	Status      string
	Notes       string
	// ExpectedResolutionFrom/To are the [from, to] estimate of when an
	// "active" diagnosis should resolve, computed from
	// extraction.Diagnosis's relative ExpectedResolutionAmount*/Unit via
	// ComputeDueDateRange — see docs/domain/07-diagnosis-and-allergy.md §2.
	// Always nil for chronic/suspected/resolved, and for active diagnoses
	// with no reasonable estimate.
	ExpectedResolutionFrom *time.Time
	ExpectedResolutionTo   *time.Time
	// ActualResolutionAt is when this diagnosis was actually confirmed
	// resolved — set only via a direct user confirmation (see
	// storage.DiagnosisRepository.MarkResolved, medical.resolve_diagnosis),
	// never by Extraction itself, and deliberately independent of
	// ExpectedResolutionFrom/To: resolving a diagnosis does not erase what
	// was estimated beforehand, it just records what actually happened
	// alongside it (see docs/domain/07-diagnosis-and-allergy.md).
	ActualResolutionAt *time.Time
	// StatusReasoning is the model's own one-sentence explanation of why it
	// picked Status (and, if set, ExpectedResolutionFrom/To) — kept
	// deliberately separate from Notes (document-derived clinical context)
	// so it can be reviewed/queried directly without conflating the two.
	// Not shown to the user via medical.ask — see
	// docs/domain/07-diagnosis-and-allergy.md.
	StatusReasoning string
}

// Overdue reports whether d is still active and its expected-resolution
// range has already closed as of asOf — mirrors PlannedAction.Overdue below
// (see that method's doc comment for why this is deliberately computed
// on read, in one place, rather than stored/mutated: there's no background
// job in this codebase that could flip a stored Status over time, and
// Diagnosis.Status otherwise only ever comes from Extraction). A chronic,
// suspected, or resolved diagnosis, or an active one with no
// ExpectedResolutionTo at all (no estimate was ever produced), is never
// overdue — overdue does not change Status itself, just flags it for
// display (see internal/profile's DiagnosisSummary.Overdue and
// internal/ask/providers.go's DiagnosisProvider).
func (d Diagnosis) Overdue(asOf time.Time) bool {
	return d.Status == "active" && d.ExpectedResolutionTo != nil && d.ExpectedResolutionTo.Before(asOf)
}

// MedicationStatus is Medication.Status's value type — see
// docs/domain/06-medication.md §2. A named type (rather than the plain
// string PlannedAction.Status/Diagnosis.Status use) so a stray literal from
// the wrong domain (e.g. Diagnosis's "resolved") can't silently compile as a
// Medication status. MedicationStatusPrescribed is the default — set by
// Normalization whenever Extraction leaves Status unset (see below) — and is
// also the one status ReplaceForDocument treats as "still untouched": once a
// Medication has moved past it (to any other value, whether Extraction said
// so or a user confirmed it via medical.start_medication/complete_medication),
// a reprocess of its source document leaves that row alone rather than
// silently resetting it (see storage.MedicationRepository.ReplaceForDocument).
type MedicationStatus string

const (
	MedicationStatusPrescribed   MedicationStatus = "prescribed"
	MedicationStatusActive       MedicationStatus = "active"
	MedicationStatusDiscontinued MedicationStatus = "discontinued"
	MedicationStatusCompleted    MedicationStatus = "completed"
)

// Medication mirrors docs/domain/06-medication.md §2.
type Medication struct {
	ID           string
	UserID       string
	DocumentID   string
	DrugName     string
	DoseAmount   float64
	DoseUnit     string
	Frequency    string
	Route        string
	// StartedAt is when intake actually began — left nil while Status is
	// MedicationStatusPrescribed (a prescription alone doesn't mean the
	// patient has started taking it), populated either by Extraction (only
	// when the text explicitly confirms intake already started, see
	// extraction.Schema's medications.startedAt) or by
	// medical.start_medication's confirmation. Deliberately not a separate
	// "date prescribed" field: nothing in this domain currently needs that
	// date once intake is confirmed, so it isn't tracked.
	StartedAt    *time.Time
	EndedAt      *time.Time
	Status       MedicationStatus
	Reason       string
	PrescribedBy string
}

// LabResult mirrors docs/domain/09-lab-result-and-vital-sign.md §2.
// Value/Unit are exactly as extracted, never touched by this package.
// NormalizedValue/NormalizedUnit are Normalization's own derived,
// rebuildable addition (see units.go's CanonicalUnitResolver) — empty when
// the unit isn't recognized as convertible to this indicator's established
// canonical unit, never a guessed value. Code/CodeSystem come either from
// Extraction transcribing a printed code, or — far more often in
// practice — this package's own small LOINC alias lookup (see loinc.go,
// including its accuracy caveat) when nothing was printed. QualitativeValue
// mirrors InstrumentalFinding's field of the same name — a result that
// isn't numeric at all (e.g. blood type, a positive/negative test) — see
// that struct's doc comment; exactly one of Value/QualitativeValue is
// normally populated, but both may be empty.
type LabResult struct {
	ID               string
	UserID           string
	DocumentID       string
	IndicatorName    string
	Code             string
	CodeSystem       string
	Value            float64
	QualitativeValue string
	Unit             string
	NormalizedValue  float64
	NormalizedUnit   string
	ReferenceLow     float64
	ReferenceHigh    float64
	TakenAt          *time.Time
}

// InstrumentalFinding mirrors docs/domain/13-instrumental-finding.md §3.
// ProcedureID is deliberately left empty here — linking a finding to its
// parent Procedure record requires the Procedure to already have a real,
// assigned ID, which happens at the Repository/Application layer after
// normalization, not within this package (see docs/domain/13-instrumental-finding.md
// §5 — ProcedureID is itself optional for exactly this reason).
type InstrumentalFinding struct {
	ID               string
	UserID           string
	DocumentID       string
	ProcedureID      string
	Structure        string
	Parameter        string
	Value            float64
	Unit             string
	NormalizedValue  float64
	NormalizedUnit   string
	QualitativeValue string
	ReferenceLow     float64
	ReferenceHigh    float64
	MeasuredAt       *time.Time
}

// Procedure mirrors docs/domain/08-procedure.md §2.
type Procedure struct {
	ID          string
	UserID      string
	DocumentID  string
	Type        string
	Name        string
	PerformedAt *time.Time
	PerformedBy string
	Notes       string
}

// Allergy mirrors docs/domain/07-diagnosis-and-allergy.md §3. Extraction's
// schema has no severity/reaction split issue worth noting here beyond what
// the domain doc already says.
type Allergy struct {
	ID         string
	UserID     string
	DocumentID string
	Substance  string
	Reaction   string
	Severity   string
}

// VitalSign mirrors docs/domain/09-lab-result-and-vital-sign.md §3.
type VitalSign struct {
	ID         string
	UserID     string
	DocumentID string
	Type       string
	Systolic   float64
	Diastolic  float64
	Value      float64
	Unit       string
	MeasuredAt *time.Time
}

// PlannedAction mirrors docs/domain/14-planned-action.md — a future medical
// action (repeat test, vaccination, follow-up visit) recommended in a
// document or reported directly by the user, with an approximate due-date
// range rather than a point date (see docs/adr/004-planned-actions.md).
//
// Unlike every other entity in this package, PlannedAction's source isn't
// always a document — SourceType/SourceID ("document"|"self_reported")
// mirrors storage.MedicationIntake's own SourceType/SourceID, the existing
// precedent in this codebase for "a fact may come from a document or from a
// dialogue with Miranda." SourceID is the documentID or the
// SelfReportedEvent id, depending on SourceType.
type PlannedAction struct {
	ID                 string
	UserID             string
	SourceType         string // "document" | "self_reported"
	SourceID           string
	Type               string // see planning.Types
	Description        string
	ReferenceText      string
	MatchIndicatorName string // lab_test only — canonicalized like LabResult.IndicatorName
	MatchProcedureName string // every other type — canonicalized like Procedure.Name
	DueDateFrom        *time.Time
	DueDateTo          *time.Time
	Status             string // see PlannedActionStatus* consts
	MatchedDocumentID  string
	MatchedEntityID    string
	MatchedAt          *time.Time
}

// PlannedAction.Status values.
const (
	PlannedActionStatusPending   = "pending"
	PlannedActionStatusCompleted = "completed"
	PlannedActionStatusDeclined  = "declined"
)

// PlannedAction.SourceType values.
const (
	PlannedActionSourceDocument     = "document"
	PlannedActionSourceSelfReported = "self_reported"
)

// Overdue reports whether a is still pending and its due-date range has
// already closed as of asOf. The single place this is computed — see
// docs/adr/004-planned-actions.md — so the MCP tool, medical-dev, and the
// Knowledge Provider can't drift on the definition. A completed or declined
// action, or one with no DueDateTo at all (no timeframe was ever stated),
// is never overdue.
func (a PlannedAction) Overdue(asOf time.Time) bool {
	return a.Status == PlannedActionStatusPending && a.DueDateTo != nil && a.DueDateTo.Before(asOf)
}

// Result is every domain entity Normalize produced from one document.
type Result struct {
	Diagnoses            []Diagnosis
	Medications          []Medication
	LabResults           []LabResult
	InstrumentalFindings []InstrumentalFinding
	Procedures           []Procedure
	Allergies            []Allergy
	VitalSigns           []VitalSign
	PlannedActions       []PlannedAction
}

// Normalize maps extracted (Extraction.raw, see
// docs/domain/03-files-and-documents.md §4) into domain entities for one
// document. Returns every issue found (e.g. an unparseable date) alongside
// the result rather than failing outright — a single bad date on one
// medication shouldn't discard the other 8 correctly-extracted ones. Errors
// are annotated with enough context (entity type + index + field) to find
// the offending Extraction entry.
//
// canonicalUnits supplies dimensional unit normalization for LabResult/
// InstrumentalFinding (see units.go) — pass nil to skip it entirely
// (NormalizedValue/NormalizedUnit are left zero on every entity), e.g. for
// callers that only care about the raw structural mapping. indicatorAliases
// supplies LabResult.IndicatorName's alias dictionary (see
// indicator_aliases.go) — pass nil to skip it too (names are only trimmed,
// never rewritten).
func Normalize(ctx context.Context, userID, documentID string, extracted extraction.Result, canonicalUnits CanonicalUnitResolver, indicatorAliases IndicatorAliasResolver) (Result, []error) {
	var errs []error
	addErr := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	var result Result

	// anchor is the reference point for every relative timeframe this
	// document's extraction states (a diagnosis's expectedResolution*, a
	// plannedAction's due*) — the document's own date when known, falling
	// back to the current processing time otherwise (see the plannedActions
	// loop below and docs/adr/004-planned-actions.md). Computed once, up
	// front, since both loops need it.
	anchor := time.Now().UTC()
	if documentDate, err := parseOptionalDate(extracted.DocumentDate); err != nil {
		addErr("documentDate %q: %w", extracted.DocumentDate, err)
	} else if documentDate != nil {
		anchor = *documentDate
	}

	for i, d := range extracted.Diagnoses {
		diagnosedAt, err := parseOptionalDate(d.DiagnosedAt)
		if err != nil {
			addErr("diagnoses[%d] %q: diagnosedAt: %w", i, d.Name, err)
		}
		// A diagnosis is often stated under a document's own date (e.g. a
		// consultation's conclusion) without repeating that date next to
		// each individual diagnosis, so Extraction correctly leaves
		// diagnosedAt unset in that case — fall back to the document-level
		// anchor here rather than leaving the diagnosis undated. Mirrors
		// timeline.Build's resolveDate, which needs the same fallback for
		// TimelineEvent.date.
		if diagnosedAt == nil {
			diagnosedAt = &anchor
		}
		resolutionFrom, resolutionTo, err := ComputeDueDateRange(*diagnosedAt, d.ExpectedResolutionAmountMin, d.ExpectedResolutionAmountMax, d.ExpectedResolutionUnit)
		if err != nil {
			addErr("diagnoses[%d] %q: expectedResolution: %w", i, d.Name, err)
		}
		result.Diagnoses = append(result.Diagnoses, Diagnosis{
			ID:                     fmt.Sprintf("dx_%s_%d", documentID, i),
			UserID:                 userID,
			DocumentID:             documentID,
			Name:                   d.Name,
			Code:                   d.Code,
			CodeSystem:             d.CodeSystem,
			DiagnosedAt:            diagnosedAt,
			Status:                 d.Status,
			Notes:                  d.Notes,
			ExpectedResolutionFrom: resolutionFrom,
			ExpectedResolutionTo:   resolutionTo,
			StatusReasoning:        d.StatusReasoning,
		})
	}

	for i, m := range extracted.Medications {
		startedAt, err := parseOptionalDate(m.StartedAt)
		if err != nil {
			addErr("medications[%d] %q: startedAt: %w", i, m.Name, err)
		}
		endedAt, err := parseOptionalDate(m.EndedAt)
		if err != nil {
			addErr("medications[%d] %q: endedAt: %w", i, m.Name, err)
		}
		status := MedicationStatus(m.Status)
		if status == "" {
			// Extraction leaves Status unset whenever the text doesn't say
			// enough to justify any of the more specific values — a plain
			// prescription with no confirmation intake has begun is exactly
			// this domain's default state, not an extraction gap to warn
			// about (see MedicationStatusPrescribed's doc comment).
			status = MedicationStatusPrescribed
		}
		result.Medications = append(result.Medications, Medication{
			ID:           fmt.Sprintf("med_%s_%d", documentID, i),
			UserID:       userID,
			DocumentID:   documentID,
			DrugName:     canonicalize(m.Name),
			DoseAmount:   m.DoseAmount,
			DoseUnit:     m.DoseUnit,
			Frequency:    m.Frequency,
			Route:        m.Route,
			StartedAt:    startedAt,
			EndedAt:      endedAt,
			Status:       status,
			Reason:       m.Reason,
			PrescribedBy: m.PrescribedBy,
		})
	}

	for i, l := range extracted.LabResults {
		takenAt, err := parseOptionalDate(l.TakenAt)
		if err != nil {
			addErr("labResults[%d] %q: takenAt: %w", i, l.Name, err)
		}
		indicatorName, aliasErr := canonicalizeIndicatorName(ctx, indicatorAliases, l.Name)
		if aliasErr != nil {
			addErr("labResults[%d] %q: canonicalize indicator name: %w", i, l.Name, aliasErr)
			indicatorName = strings.TrimSpace(l.Name)
		}
		var normValue float64
		var normUnit string
		// Only a quantitative result (Unit set) goes through unit
		// normalization — a qualitative-only result (e.g. blood type) has
		// nothing to convert, same reasoning as InstrumentalFindings below.
		if canonicalUnits != nil && l.Unit != "" {
			var unitErr error
			normValue, normUnit, unitErr = normalizeUnit(ctx, canonicalUnits, userID, indicatorName, l.Value, l.Unit)
			if unitErr != nil {
				addErr("labResults[%d] %q: normalize unit: %w", i, l.Name, unitErr)
			}
		}
		code, codeSystem := l.Code, l.CodeSystem
		if code == "" {
			if loinc, ok := lookupLOINC(l.Name); ok {
				code, codeSystem = loinc, "loinc"
			}
		}
		result.LabResults = append(result.LabResults, LabResult{
			ID:               fmt.Sprintf("lab_%s_%d", documentID, i),
			UserID:           userID,
			DocumentID:       documentID,
			IndicatorName:    indicatorName,
			Code:             code,
			CodeSystem:       codeSystem,
			Value:            l.Value,
			QualitativeValue: l.QualitativeValue,
			Unit:             l.Unit,
			NormalizedValue:  normValue,
			NormalizedUnit:   normUnit,
			ReferenceLow:     l.ReferenceLow,
			ReferenceHigh:    l.ReferenceHigh,
			TakenAt:          takenAt,
		})
	}

	for i, f := range extracted.InstrumentalFindings {
		measuredAt, err := parseOptionalDate(f.MeasuredAt)
		if err != nil {
			addErr("instrumentalFindings[%d] %s/%s: measuredAt: %w", i, f.Structure, f.Parameter, err)
		}
		structure := canonicalize(f.Structure)
		var normValue float64
		var normUnit string
		// Only quantitative findings (Unit set) go through unit
		// normalization — a qualitative-only finding (e.g. "эхогенность:
		// обычная") has nothing to convert. The lookup key combines
		// structure and parameter (e.g. "Печень/правая доля КВР") since,
		// unlike LabResult's flat indicator namespace, the same parameter
		// name can recur across different structures with unrelated units.
		if canonicalUnits != nil && f.Unit != "" {
			var unitErr error
			normValue, normUnit, unitErr = normalizeUnit(ctx, canonicalUnits, userID, structure+"/"+f.Parameter, f.Value, f.Unit)
			if unitErr != nil {
				addErr("instrumentalFindings[%d] %s/%s: normalize unit: %w", i, structure, f.Parameter, unitErr)
			}
		}
		result.InstrumentalFindings = append(result.InstrumentalFindings, InstrumentalFinding{
			ID:               fmt.Sprintf("finding_%s_%d", documentID, i),
			UserID:           userID,
			DocumentID:       documentID,
			Structure:        structure,
			Parameter:        f.Parameter,
			Value:            f.Value,
			Unit:             f.Unit,
			NormalizedValue:  normValue,
			NormalizedUnit:   normUnit,
			QualitativeValue: f.QualitativeValue,
			ReferenceLow:     f.ReferenceLow,
			ReferenceHigh:    f.ReferenceHigh,
			MeasuredAt:       measuredAt,
		})
	}

	for i, p := range extracted.Procedures {
		performedAt, err := parseOptionalDate(p.PerformedAt)
		if err != nil {
			addErr("procedures[%d] %q: performedAt: %w", i, p.Name, err)
		}
		result.Procedures = append(result.Procedures, Procedure{
			ID:          fmt.Sprintf("proc_%s_%d", documentID, i),
			UserID:      userID,
			DocumentID:  documentID,
			Type:        p.Type,
			Name:        p.Name,
			PerformedAt: performedAt,
			PerformedBy: p.PerformedBy,
			Notes:       p.Notes,
		})
	}

	for i, a := range extracted.Allergies {
		result.Allergies = append(result.Allergies, Allergy{
			ID:         fmt.Sprintf("allergy_%s_%d", documentID, i),
			UserID:     userID,
			DocumentID: documentID,
			Substance:  canonicalize(a.Substance),
			Reaction:   a.Reaction,
			Severity:   a.Severity,
		})
	}

	for i, v := range extracted.VitalSigns {
		measuredAt, err := parseOptionalDate(v.MeasuredAt)
		if err != nil {
			addErr("vitalSigns[%d] %s: measuredAt: %w", i, v.Type, err)
		}
		result.VitalSigns = append(result.VitalSigns, VitalSign{
			ID:         fmt.Sprintf("vital_%s_%d", documentID, i),
			UserID:     userID,
			DocumentID: documentID,
			Type:       v.Type,
			Systolic:   v.Systolic,
			Diastolic:  v.Diastolic,
			Value:      v.Value,
			Unit:       v.Unit,
			MeasuredAt: measuredAt,
		})
	}

	// PlannedActions anchor their relative due-date offsets (see
	// ComputeDueDateRange) on the same document-level anchor computed above
	// (docs/adr/004-planned-actions.md).
	for i, pa := range extracted.PlannedActions {
		built, err := BuildPlannedAction(ctx, fmt.Sprintf("plan_%s_%d", documentID, i), userID,
			PlannedActionSourceDocument, documentID, anchor, pa, indicatorAliases)
		if err != nil {
			addErr("plannedActions[%d] %q: %w", i, pa.Description, err)
		}
		result.PlannedActions = append(result.PlannedActions, built)
	}

	return result, errs
}

// BuildPlannedAction turns one planning.PlannedAction into a
// normalization.PlannedAction, anchored at anchorDate — the document's own
// date for a document-derived recommendation, or the moment of the
// conversation for a self-reported one (see
// docs/adr/004-planned-actions.md). Exported and used directly by
// internal/pipeline.LogEvent as well as by Normalize's own loop above,
// since a self-reported note is never run through Normalize at all (see
// internal/events's package doc comment — it has its own, lighter
// pipeline) but still needs the identical date-math/canonicalization this
// function does for document-derived actions, so the two sources land on
// matching keys.
func BuildPlannedAction(ctx context.Context, id, userID, sourceType, sourceID string, anchorDate time.Time, p planning.PlannedAction, indicatorAliases IndicatorAliasResolver) (PlannedAction, error) {
	from, to, err := ComputeDueDateRange(anchorDate, p.DueAmountMin, p.DueAmountMax, p.DueUnit)

	var indicatorName, procedureName string
	if p.Type == "lab_test" {
		var aliasErr error
		indicatorName, aliasErr = canonicalizeIndicatorName(ctx, indicatorAliases, p.RelatedIndicatorName)
		if aliasErr != nil && err == nil {
			err = aliasErr
		}
	} else {
		procedureName = canonicalize(p.RelatedProcedureName)
	}

	return PlannedAction{
		ID:                 id,
		UserID:             userID,
		SourceType:         sourceType,
		SourceID:           sourceID,
		Type:               p.Type,
		Description:        p.Description,
		ReferenceText:      p.ReferenceText,
		MatchIndicatorName: indicatorName,
		MatchProcedureName: procedureName,
		DueDateFrom:        from,
		DueDateTo:          to,
		Status:             PlannedActionStatusPending,
	}, err
}

// ComputeDueDateRange turns a rough relative timeframe (minAmount/maxAmount
// + unit — as extracted by planning.PlannedAction's due*, or
// extraction.Diagnosis's expectedResolution*) into an absolute [from, to]
// date range anchored at base. See docs/adr/004-planned-actions.md's
// "Диапазон дат, а не точечная дата" for why this arithmetic is
// deterministic Go rather than something the LLM is asked to compute
// itself — the same reasoning applies to a diagnosis's expected-resolution
// estimate, which is why it reuses this function instead of a second copy.
//
// maxAmount <= 0 means no timeframe was stated at all (a legitimate case —
// e.g. "сдать кровь на глюкозу" with no due date, or a diagnosis extraction
// gave no expected-resolution estimate at all) — returns (nil, nil, nil),
// not an error. minAmount defaults to maxAmount when omitted (a
// precise "in 6 months" rather than an approximate "через полгода"
// range). minAmount > maxAmount, or an unrecognized unit, is an error —
// non-fatal to the caller, same posture as every other per-entity error
// Normalize collects.
func ComputeDueDateRange(base time.Time, minAmount, maxAmount float64, unit string) (from, to *time.Time, err error) {
	if maxAmount <= 0 {
		return nil, nil, nil
	}
	if minAmount <= 0 {
		minAmount = maxAmount
	}
	if minAmount > maxAmount {
		return nil, nil, fmt.Errorf("due amount min %v exceeds max %v", minAmount, maxAmount)
	}

	offset := func(amount float64) (time.Time, error) {
		switch unit {
		case "day":
			return base.AddDate(0, 0, int(math.Round(amount))), nil
		case "week":
			return base.AddDate(0, 0, int(math.Round(amount*7))), nil
		case "month":
			return base.AddDate(0, int(math.Round(amount)), 0), nil
		case "year":
			return base.AddDate(int(math.Round(amount)), 0, 0), nil
		default:
			return time.Time{}, fmt.Errorf("unknown due unit %q", unit)
		}
	}

	fromT, err := offset(minAmount)
	if err != nil {
		return nil, nil, err
	}
	toT, err := offset(maxAmount)
	if err != nil {
		return nil, nil, err
	}
	return &fromT, &toT, nil
}

// parseOptionalDate parses an ISO 8601 (YYYY-MM-DD) date string, per
// Extraction's own prompt contract (see extraction.Prompt's date-format
// rule). Empty input is not an error — most date fields are optional.
func parseOptionalDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return nil, fmt.Errorf("parse %q as %s: %w", s, dateLayout, err)
	}
	return &t, nil
}

// canonicalize is currently a no-op beyond whitespace trimming — the real
// canonicalization step (mapping "Урсокапс (урсаклин, урсосан)" to a
// single canonical drug identity, or matching "Печень"/"печень" regardless
// of case) is still an open design question, explicitly called out as
// unsolved in docs/architecture/02-processing-pipeline.md §6. Kept as its
// own named function (rather than inlined) so the eventual real
// implementation has one obvious place to land, and so every call site
// that will need it is already marked.
//
// LabResult.IndicatorName no longer goes through this function — it has its
// own alias-aware canonicalizeIndicatorName (see indicator_aliases.go),
// which is this same gap solved for exactly one entity type via a small
// hand-curated dictionary (same pattern as loinc.go). Medication.DrugName,
// InstrumentalFinding.Structure, and Allergy.Substance are still the
// open part of this gap.
func canonicalize(s string) string {
	return strings.TrimSpace(s)
}
