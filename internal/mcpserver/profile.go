package mcpserver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
)

func registerProfileTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.profile",
		Description: "Returns the aggregated current health state: active diagnoses, chronic conditions, current medications, allergies, vaccinations, latest lab results and vital signs. Does not perform analysis — data only.",
	}, profileHandler(pl, gate, logger))
}

type ProfileInput struct {
	UserID    string `json:"userId" jsonschema:"User identifier."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Whose profile to fetch, if not the caller's own. Must be that household member's own user_id — the same identifier used for userId elsewhere (e.g. \"anna\"), never a display name like \"Аня\". Omit to default to the caller's own data."`
}

type DiagnosisSummaryOutput struct {
	Name        string `json:"name"`
	Code        string `json:"code,omitempty"`
	DiagnosedAt string `json:"diagnosedAt,omitempty"`
}

type MedicationSummaryOutput struct {
	Name      string `json:"name"`
	Dose      string `json:"dose,omitempty"`
	Frequency string `json:"frequency,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

type AllergySummaryOutput struct {
	Substance string `json:"substance"`
	Reaction  string `json:"reaction,omitempty"`
}

type ProcedureSummaryOutput struct {
	Name        string `json:"name"`
	PerformedAt string `json:"performedAt,omitempty"`
}

// LabResultSummaryOutput.DocumentSource names the source document type/title
// (e.g. "Общий анализ крови" vs "Общий анализ мочи") a reading came from —
// without it, two differently-sourced indicators sharing a name (protein in
// a blood panel vs. a urinalysis) are indistinguishable to a caller, which
// has caused Miranda to mis-group profile data (see
// docs/adr/002-structured-profile-response.md).
type LabResultSummaryOutput struct {
	Name             string  `json:"name"`
	Value            float64 `json:"value,omitempty"`
	QualitativeValue string  `json:"qualitativeValue,omitempty"`
	Unit             string  `json:"unit,omitempty"`
	Date             string  `json:"date,omitempty"`
	DocumentSource   string  `json:"documentSource,omitempty"`
}

// VitalSignSummaryOutput.DocumentSource mirrors
// LabResultSummaryOutput.DocumentSource — same reasoning, same source.
type VitalSignSummaryOutput struct {
	Name           string `json:"name"`
	Value          string `json:"value"`
	Date           string `json:"date,omitempty"`
	DocumentSource string `json:"documentSource,omitempty"`
}

// ProfileOutput mirrors docs/mcp/05-profile.md §5.
type ProfileOutput struct {
	ActiveDiagnoses   []DiagnosisSummaryOutput  `json:"activeDiagnoses"`
	ChronicConditions []DiagnosisSummaryOutput  `json:"chronicConditions"`
	ActiveMedications []MedicationSummaryOutput `json:"activeMedications"`
	Allergies         []AllergySummaryOutput    `json:"allergies"`
	Vaccinations      []ProcedureSummaryOutput  `json:"vaccinations"`
	LatestLabResults  []LabResultSummaryOutput  `json:"latestLabResults"`
	LatestVitalSigns  []VitalSignSummaryOutput  `json:"latestVitalSigns"`
}

func profileHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[ProfileInput, ProfileOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ProfileInput) (*mcp.CallToolResult, ProfileOutput, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, ProfileOutput{}, err
		}

		built, err := pl.GetProfile(ctx, subjectID)
		if err != nil {
			logger.Error("profile failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
			return nil, ProfileOutput{}, mcpError(codeStorageError, "%v", err)
		}

		out := toProfileOutput(built)
		logger.Info("profile", "userId", in.UserID, "subjectId", subjectID)
		// Deliberately no hand-built Content here (unlike other handlers in
		// this package that write a short human summary) — see
		// docs/adr/002-structured-profile-response.md: a caller reading
		// medical.profile to build something like a PDF needs every
		// section (allergies, labs, vitals, ...), and a lossy one-line
		// count of just 3 of the 7 sections was silently starving that
		// caller of the rest. Leaving Content nil makes the SDK fall back
		// to serializing the full, schema-validated `out` as the Content
		// text (see modelcontextprotocol/go-sdk's toolForErr) — the same
		// value already returned as StructuredContent, so the two can
		// never drift apart.
		return nil, out, nil
	}
}

func toProfileOutput(p profile.Profile) ProfileOutput {
	out := ProfileOutput{
		ActiveDiagnoses:   make([]DiagnosisSummaryOutput, len(p.ActiveDiagnoses)),
		ChronicConditions: make([]DiagnosisSummaryOutput, len(p.ChronicConditions)),
		ActiveMedications: make([]MedicationSummaryOutput, len(p.ActiveMedications)),
		Allergies:         make([]AllergySummaryOutput, len(p.Allergies)),
		Vaccinations:      make([]ProcedureSummaryOutput, len(p.Vaccinations)),
		LatestLabResults:  make([]LabResultSummaryOutput, len(p.LatestLabResults)),
		LatestVitalSigns:  make([]VitalSignSummaryOutput, len(p.LatestVitalSigns)),
	}
	for i, d := range p.ActiveDiagnoses {
		out.ActiveDiagnoses[i] = DiagnosisSummaryOutput{Name: d.Name, Code: d.Code, DiagnosedAt: formatOptionalDate(d.DiagnosedAt)}
	}
	for i, d := range p.ChronicConditions {
		out.ChronicConditions[i] = DiagnosisSummaryOutput{Name: d.Name, Code: d.Code, DiagnosedAt: formatOptionalDate(d.DiagnosedAt)}
	}
	for i, m := range p.ActiveMedications {
		dose := ""
		if m.DoseAmount > 0 {
			dose = fmt.Sprintf("%g %s", m.DoseAmount, m.DoseUnit)
		}
		out.ActiveMedications[i] = MedicationSummaryOutput{Name: m.DrugName, Dose: dose, Frequency: m.Frequency, StartedAt: formatOptionalDate(m.StartedAt)}
	}
	for i, a := range p.Allergies {
		out.Allergies[i] = AllergySummaryOutput{Substance: a.Substance, Reaction: a.Reaction}
	}
	for i, pr := range p.Vaccinations {
		out.Vaccinations[i] = ProcedureSummaryOutput{Name: pr.Name, PerformedAt: formatOptionalDate(pr.PerformedAt)}
	}
	for i, l := range p.LatestLabResults {
		out.LatestLabResults[i] = LabResultSummaryOutput{
			Name: l.IndicatorName, Value: l.Value, QualitativeValue: l.QualitativeValue, Unit: l.Unit,
			Date: formatOptionalDate(l.TakenAt), DocumentSource: l.DocumentTitle,
		}
	}
	for i, v := range p.LatestVitalSigns {
		value := fmt.Sprintf("%g %s", v.Value, v.Unit)
		if v.Type == "blood_pressure" {
			value = fmt.Sprintf("%g/%g", v.Systolic, v.Diastolic)
		}
		out.LatestVitalSigns[i] = VitalSignSummaryOutput{Name: v.Type, Value: value, Date: formatOptionalDate(v.MeasuredAt), DocumentSource: v.DocumentTitle}
	}
	return out
}
