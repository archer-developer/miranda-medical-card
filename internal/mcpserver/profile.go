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
		Description: "Возвращает агрегированное текущее состояние здоровья: действующие диагнозы, хронические заболевания, текущие лекарства, аллергии, прививки, последние анализы и показатели. Не выполняет анализ — только данные. См. docs/mcp/05-profile.md.",
	}, profileHandler(pl, gate, logger))
}

type ProfileInput struct {
	UserID    string `json:"userId" jsonschema:"Идентификатор пользователя."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Чей профиль получить, если не свой."`
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

type LabResultSummaryOutput struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	Date  string  `json:"date,omitempty"`
}

type VitalSignSummaryOutput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Date  string `json:"date,omitempty"`
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
		text := fmt.Sprintf("%d active diagnoses, %d active medications, %d allergies.", len(out.ActiveDiagnoses), len(out.ActiveMedications), len(out.Allergies))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
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
		out.LatestLabResults[i] = LabResultSummaryOutput{Name: l.IndicatorName, Value: l.Value, Unit: l.Unit, Date: formatOptionalDate(l.TakenAt)}
	}
	for i, v := range p.LatestVitalSigns {
		value := fmt.Sprintf("%g %s", v.Value, v.Unit)
		if v.Type == "blood_pressure" {
			value = fmt.Sprintf("%g/%g", v.Systolic, v.Diastolic)
		}
		out.LatestVitalSigns[i] = VitalSignSummaryOutput{Name: v.Type, Value: value, Date: formatOptionalDate(v.MeasuredAt)}
	}
	return out
}
