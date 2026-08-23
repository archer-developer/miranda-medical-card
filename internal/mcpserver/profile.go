package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/linkstore"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
)

// profileFileLinkTTL bounds how long a format=file_uri link stays
// resolvable — docs/adr/007-short-lived-file-links-for-profile-export.md
// picks 5 minutes as "five orders of magnitude below the practical
// brute-force window for a 128-bit id", with large headroom over the
// actual use case (handing the link to another tool within the same
// agent-loop turn, normally seconds).
const profileFileLinkTTL = 5 * time.Minute

func registerProfileTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, links *linkstore.Store, publicBaseURL string, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.profile",
		Description: "Returns the aggregated current health state: active diagnoses, chronic conditions, current medications, allergies, vaccinations, past surgeries, latest lab results and vital signs, and dietary restrictions/recommendations. Does not perform analysis — data only. format=\"json\" (default) returns the full profile inline (activeDiagnoses, chronicConditions, activeMedications, allergies, vaccinations, surgeries, latestLabResults, latestVitalSigns, nutritionGuidance). format=\"file_uri\" instead returns {fileUri, expiresAt} — a short-lived (5 minute) link to that same JSON — use this when the goal is handing the data to another tool (e.g. code-execution-sandbox's upload_file, for PDF generation) rather than reading it yourself: retyping a large profile by hand is unreliable, a URI passed through server-to-server is not. fields=[...] restricts either shape to just the named top-level sections (e.g. [\"nutritionGuidance\", \"vaccinations\"]) instead of every section — use it when only one or two sections are actually needed, to avoid pulling (and paying context budget for) the rest of the profile.",
	}, profileHandler(pl, gate, links, publicBaseURL, logger))
}

type ProfileInput struct {
	UserID    string `json:"userId" jsonschema:"User identifier."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Whose profile to fetch, if not the caller's own. Must be that household member's own user_id — the same identifier used for userId elsewhere (e.g. \"anna\"), never a display name like \"Аня\". Omit to default to the caller's own data."`
	// Format selects the response shape: "json" (default) returns the full
	// profile inline, as today. "file_uri" instead stages the same JSON
	// behind a short-lived link and returns just that URI — see
	// docs/adr/007-short-lived-file-links-for-profile-export.md.
	Format string `json:"format,omitempty" jsonschema:"Response shape: \"json\" (default, full profile inline) or \"file_uri\" (a short-lived link to the same JSON — use when handing this straight to another tool, e.g. PDF generation, instead of reading it yourself)."`
	// Fields optionally narrows the response to a subset of ProfileOutput's
	// top-level sections (profileFields is the single source of truth for
	// the valid names) instead of the full profile — added for callers that
	// only need e.g. nutritionGuidance + vaccinations and shouldn't have to
	// pull (and pay context budget for) the rest. Applies to both format
	// shapes: for "json" it shrinks the inline payload, for "file_uri" it
	// shrinks the staged JSON the link resolves to. Nil/empty means every
	// section, unchanged from before this field existed.
	Fields []string `json:"fields,omitempty" jsonschema:"Restrict the response to only these top-level profile sections instead of the full profile. One or more of: activeDiagnoses, chronicConditions, activeMedications, allergies, vaccinations, surgeries, latestLabResults, latestVitalSigns, nutritionGuidance. Omit for the full profile (default)."`
}

// profileFieldDescriptor pairs one ProfileOutput top-level JSON key with an
// accessor for it — the single source of truth ProfileInput.Fields is
// validated and filtered against, so the valid-values list can never drift
// from what toProfileOutput actually populates. Order matches ProfileOutput's
// own field order.
type profileFieldDescriptor struct {
	name  string
	value func(ProfileOutput) any
}

var profileFields = []profileFieldDescriptor{
	{"activeDiagnoses", func(o ProfileOutput) any { return o.ActiveDiagnoses }},
	{"chronicConditions", func(o ProfileOutput) any { return o.ChronicConditions }},
	{"activeMedications", func(o ProfileOutput) any { return o.ActiveMedications }},
	{"allergies", func(o ProfileOutput) any { return o.Allergies }},
	{"vaccinations", func(o ProfileOutput) any { return o.Vaccinations }},
	{"surgeries", func(o ProfileOutput) any { return o.Surgeries }},
	{"latestLabResults", func(o ProfileOutput) any { return o.LatestLabResults }},
	{"latestVitalSigns", func(o ProfileOutput) any { return o.LatestVitalSigns }},
	{"nutritionGuidance", func(o ProfileOutput) any { return o.NutritionGuidance }},
}

func profileFieldNames() []string {
	names := make([]string, len(profileFields))
	for i, f := range profileFields {
		names[i] = f.name
	}
	return names
}

// validateProfileFields rejects any name ProfileInput.Fields carries that
// toProfileOutput doesn't actually produce — the same "reject anything
// Default()/here couldn't produce" spirit as internal/config's validate(),
// applied to a hand-typed tool argument instead of a hand-edited YAML file.
func validateProfileFields(fields []string) error {
	valid := make(map[string]bool, len(profileFields))
	for _, f := range profileFields {
		valid[f.name] = true
	}
	for _, f := range fields {
		if !valid[f] {
			return mcpError(codeInvalidFields, "unknown field %q — must be one of: %s", f, strings.Join(profileFieldNames(), ", "))
		}
	}
	return nil
}

// filterProfileOutput returns out unchanged (as ProfileOutput) when fields
// is empty — the full-profile default, preserving the exact response shape
// every caller before this field existed already relies on. A non-empty
// fields instead returns a map containing only the requested sections, so a
// section that wasn't asked for is genuinely absent from the JSON rather
// than present-but-empty — the two are different things (no allergies vs.
// allergies not requested) and callers should never have to guess which.
func filterProfileOutput(out ProfileOutput, fields []string) any {
	if len(fields) == 0 {
		return out
	}
	requested := make(map[string]bool, len(fields))
	for _, f := range fields {
		requested[f] = true
	}
	filtered := make(map[string]any, len(fields))
	for _, f := range profileFields {
		if requested[f.name] {
			filtered[f.name] = f.value(out)
		}
	}
	return filtered
}

type DiagnosisSummaryOutput struct {
	Name        string `json:"name"`
	Code        string `json:"code,omitempty"`
	DiagnosedAt string `json:"diagnosedAt,omitempty"`
	// Overdue mirrors normalization.Diagnosis.Overdue — true when this
	// diagnosis is still "active" but its expected-resolution estimate has
	// already passed. Always false for a chronic condition or an active
	// diagnosis with no estimate.
	Overdue bool `json:"overdue"`
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

// NutritionNoteOutput mirrors profile.NutritionNote — see
// docs/adr/006-nutrition-guidance.md §1 for why Reason is included
// alongside Text.
type NutritionNoteOutput struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// NutritionGuidanceOutput mirrors profile.NutritionGuidance
// (docs/adr/006-nutrition-guidance.md). Omitted entirely (both slices nil)
// when the user has no active diagnoses/allergies/recent symptoms to base
// guidance on — see that ADR's §4.
type NutritionGuidanceOutput struct {
	Restrictions    []NutritionNoteOutput `json:"restrictions,omitempty"`
	Recommendations []NutritionNoteOutput `json:"recommendations,omitempty"`
}

// ProfileOutput mirrors docs/mcp/05-profile.md §5.
type ProfileOutput struct {
	ActiveDiagnoses   []DiagnosisSummaryOutput  `json:"activeDiagnoses"`
	ChronicConditions []DiagnosisSummaryOutput  `json:"chronicConditions"`
	ActiveMedications []MedicationSummaryOutput `json:"activeMedications"`
	Allergies         []AllergySummaryOutput    `json:"allergies"`
	Vaccinations      []ProcedureSummaryOutput  `json:"vaccinations"`
	Surgeries         []ProcedureSummaryOutput  `json:"surgeries"`
	LatestLabResults  []LabResultSummaryOutput  `json:"latestLabResults"`
	LatestVitalSigns  []VitalSignSummaryOutput  `json:"latestVitalSigns"`
	NutritionGuidance NutritionGuidanceOutput   `json:"nutritionGuidance"`
}

// ProfileFileOutput is medical.profile's format=file_uri response shape —
// docs/adr/007-short-lived-file-links-for-profile-export.md §"Что нужно
// сделать" item 5. ExpiresAt is RFC3339 so the model has an explicit,
// parseable deadline rather than having to infer urgency from prose.
type ProfileFileOutput struct {
	FileURI   string `json:"fileUri"`
	ExpiresAt string `json:"expiresAt"`
}

// profileHandler's Out type parameter is `any`, not ProfileOutput, because
// the tool has more than one possible response shape depending on
// ProfileInput.Format (see docs/adr/007 §5, "решить на этапе реализации...
// не переусложнять специально под ADR") and now also ProfileInput.Fields
// (filterProfileOutput returns a map[string]any instead of ProfileOutput
// once Fields narrows the response) — mcp.AddTool's generic Out can't
// express either directly. This still preserves the
// Content-mirrors-StructuredContent invariant server_test.go checks: the
// SDK's toolForErr marshals whatever concrete value the handler returns
// into both fields identically, regardless of the static Out type (see
// modelcontextprotocol/go-sdk's server.go toolForErr) — it only loses the
// auto-generated *output* JSON Schema entry for medical.profile, which the
// tool Description above compensates for in prose.
func profileHandler(pl *pipeline.Pipeline, gate *userGate, links *linkstore.Store, publicBaseURL string, logger *slog.Logger) mcp.ToolHandlerFor[ProfileInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ProfileInput) (*mcp.CallToolResult, any, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, nil, err
		}
		format := in.Format
		if format == "" {
			format = "json"
		}
		if format != "json" && format != "file_uri" {
			return nil, nil, mcpError(codeInvalidFormat, "format must be \"json\" or \"file_uri\", got %q", in.Format)
		}
		if err := validateProfileFields(in.Fields); err != nil {
			return nil, nil, err
		}

		built, err := pl.GetProfile(ctx, subjectID)
		if err != nil {
			logger.Error("profile failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
			return nil, nil, mcpError(codeStorageError, "%v", err)
		}

		result := filterProfileOutput(toProfileOutput(built), in.Fields)

		if format == "file_uri" {
			jsonBytes, err := json.Marshal(result)
			if err != nil {
				return nil, nil, fmt.Errorf("mcpserver: marshal profile for file_uri: %w", err)
			}
			linkID, expiresAt, err := links.Mint(ctx, jsonBytes, "application/json", "profile.json", profileFileLinkTTL)
			if err != nil {
				logger.Error("profile: mint file_uri link failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
				return nil, nil, mcpError(codeStorageError, "%v", err)
			}
			logger.Info("profile", "userId", in.UserID, "subjectId", subjectID, "format", format, "fields", in.Fields, "expiresAt", expiresAt)
			fileOut := ProfileFileOutput{FileURI: linkURI(publicBaseURL, linkID), ExpiresAt: expiresAt.Format(time.RFC3339)}
			return nil, fileOut, nil
		}

		logger.Info("profile", "userId", in.UserID, "subjectId", subjectID, "format", format, "fields", in.Fields)
		// Deliberately no hand-built Content here (unlike other handlers in
		// this package that write a short human summary) — see
		// docs/adr/002-structured-profile-response.md: a caller reading
		// medical.profile to build something like a PDF needs every
		// section (allergies, labs, vitals, ...), and a lossy one-line
		// count of just 3 of the 7 sections was silently starving that
		// caller of the rest. Leaving Content nil makes the SDK fall back
		// to serializing the full, schema-validated `result` as the Content
		// text (see modelcontextprotocol/go-sdk's toolForErr) — the same
		// value already returned as StructuredContent, so the two can
		// never drift apart.
		return nil, result, nil
	}
}

func toProfileOutput(p profile.Profile) ProfileOutput {
	out := ProfileOutput{
		ActiveDiagnoses:   make([]DiagnosisSummaryOutput, len(p.ActiveDiagnoses)),
		ChronicConditions: make([]DiagnosisSummaryOutput, len(p.ChronicConditions)),
		ActiveMedications: make([]MedicationSummaryOutput, len(p.ActiveMedications)),
		Allergies:         make([]AllergySummaryOutput, len(p.Allergies)),
		Vaccinations:      make([]ProcedureSummaryOutput, len(p.Vaccinations)),
		Surgeries:         make([]ProcedureSummaryOutput, len(p.Surgeries)),
		LatestLabResults:  make([]LabResultSummaryOutput, len(p.LatestLabResults)),
		LatestVitalSigns:  make([]VitalSignSummaryOutput, len(p.LatestVitalSigns)),
	}
	for i, d := range p.ActiveDiagnoses {
		out.ActiveDiagnoses[i] = DiagnosisSummaryOutput{Name: d.Name, Code: d.Code, DiagnosedAt: formatOptionalDate(d.DiagnosedAt), Overdue: d.Overdue}
	}
	for i, d := range p.ChronicConditions {
		out.ChronicConditions[i] = DiagnosisSummaryOutput{Name: d.Name, Code: d.Code, DiagnosedAt: formatOptionalDate(d.DiagnosedAt), Overdue: d.Overdue}
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
	for i, pr := range p.Surgeries {
		out.Surgeries[i] = ProcedureSummaryOutput{Name: pr.Name, PerformedAt: formatOptionalDate(pr.PerformedAt)}
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
	out.NutritionGuidance = toNutritionGuidanceOutput(p.NutritionGuidance)
	return out
}

func toNutritionGuidanceOutput(g profile.NutritionGuidance) NutritionGuidanceOutput {
	return NutritionGuidanceOutput{
		Restrictions:    toNutritionNoteOutputs(g.Restrictions),
		Recommendations: toNutritionNoteOutputs(g.Recommendations),
	}
}

func toNutritionNoteOutputs(notes []profile.NutritionNote) []NutritionNoteOutput {
	if len(notes) == 0 {
		return nil
	}
	out := make([]NutritionNoteOutput, len(notes))
	for i, n := range notes {
		out[i] = NutritionNoteOutput{Text: n.Text, Reason: n.Reason}
	}
	return out
}
