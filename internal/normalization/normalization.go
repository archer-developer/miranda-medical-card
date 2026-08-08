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
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
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
}

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
	StartedAt    *time.Time
	EndedAt      *time.Time
	Status       string
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
// including its accuracy caveat) when nothing was printed.
type LabResult struct {
	ID              string
	UserID          string
	DocumentID      string
	IndicatorName   string
	Code            string
	CodeSystem      string
	Value           float64
	Unit            string
	NormalizedValue float64
	NormalizedUnit  string
	ReferenceLow    float64
	ReferenceHigh   float64
	TakenAt         *time.Time
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

// Result is every domain entity Normalize produced from one document.
type Result struct {
	Diagnoses            []Diagnosis
	Medications          []Medication
	LabResults           []LabResult
	InstrumentalFindings []InstrumentalFinding
	Procedures           []Procedure
	Allergies            []Allergy
	VitalSigns           []VitalSign
}

// Normalize maps extracted (Extraction.raw, see
// docs/domain/03-files-and-documents.md §4) into domain entities for one
// document. Returns every issue found (e.g. an unparseable date) alongside
// the result rather than failing outright — a single bad date on one
// medication shouldn't discard the other 8 correctly-extracted ones. Errors
// are annotated with enough context (entity type + index + field) to find
// the offending Extraction entry.
//
// resolver supplies dimensional unit normalization for LabResult/
// InstrumentalFinding (see units.go) — pass nil to skip it entirely
// (NormalizedValue/NormalizedUnit are left zero on every entity), e.g. for
// callers that only care about the raw structural mapping.
func Normalize(userID, documentID string, extracted extraction.Result, resolver CanonicalUnitResolver) (Result, []error) {
	var errs []error
	addErr := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	var result Result

	for i, d := range extracted.Diagnoses {
		diagnosedAt, err := parseOptionalDate(d.DiagnosedAt)
		if err != nil {
			addErr("diagnoses[%d] %q: diagnosedAt: %w", i, d.Name, err)
		}
		result.Diagnoses = append(result.Diagnoses, Diagnosis{
			ID:          fmt.Sprintf("dx_%s_%d", documentID, i),
			UserID:      userID,
			DocumentID:  documentID,
			Name:        d.Name,
			Code:        d.Code,
			CodeSystem:  d.CodeSystem,
			DiagnosedAt: diagnosedAt,
			Status:      d.Status,
			Notes:       d.Notes,
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
			Status:       m.Status,
			Reason:       m.Reason,
			PrescribedBy: m.PrescribedBy,
		})
	}

	for i, l := range extracted.LabResults {
		takenAt, err := parseOptionalDate(l.TakenAt)
		if err != nil {
			addErr("labResults[%d] %q: takenAt: %w", i, l.Name, err)
		}
		indicatorName := canonicalize(l.Name)
		var normValue float64
		var normUnit string
		if resolver != nil {
			normValue, normUnit, _ = normalizeUnit(resolver, userID, indicatorName, l.Value, l.Unit)
		}
		code, codeSystem := l.Code, l.CodeSystem
		if code == "" {
			if loinc, ok := lookupLOINC(l.Name); ok {
				code, codeSystem = loinc, "loinc"
			}
		}
		result.LabResults = append(result.LabResults, LabResult{
			ID:              fmt.Sprintf("lab_%s_%d", documentID, i),
			UserID:          userID,
			DocumentID:      documentID,
			IndicatorName:   indicatorName,
			Code:            code,
			CodeSystem:      codeSystem,
			Value:           l.Value,
			Unit:            l.Unit,
			NormalizedValue: normValue,
			NormalizedUnit:  normUnit,
			ReferenceLow:    l.ReferenceLow,
			ReferenceHigh:   l.ReferenceHigh,
			TakenAt:         takenAt,
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
		if resolver != nil && f.Unit != "" {
			normValue, normUnit, _ = normalizeUnit(resolver, userID, structure+"/"+f.Parameter, f.Value, f.Unit)
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

	return result, errs
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
func canonicalize(s string) string {
	return strings.TrimSpace(s)
}
