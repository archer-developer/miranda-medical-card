// Package profile implements ProfileBuilder (docs/domain/05-medical-profile.md
// §6, docs/domain/01-overview.md §7) — reads a user's current Medication/
// Diagnosis/Procedure/Allergy/LabResult/VitalSign entities across every
// document and aggregates them into one MedicalProfile "snapshot". No LLM.
//
// The Medication/Diagnosis Resolvers described in docs/domain/01-overview.md
// §7 live here too (resolveActiveMedications/resolveActiveDiagnoses):
// grouping entities by canonicalized name/code at rebuild time, rather than
// reading a persisted cross-document link, is exactly the "compute
// continuity on the fly" approach docs/architecture/02-processing-pipeline.md
// §6 requires (see that section's "Почему нет постоянных междокументных
// связей").
package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// Profile mirrors docs/domain/05-medical-profile.md §2.
type Profile struct {
	UserID            string
	ActiveDiagnoses   []DiagnosisSummary
	ChronicConditions []DiagnosisSummary
	ActiveMedications []MedicationSummary
	Allergies         []AllergySummary
	Vaccinations      []ProcedureSummary
	LatestLabResults  []LabResultSummary
	LatestVitalSigns  []VitalSignSummary
	RebuiltAt         time.Time
}

type DiagnosisSummary struct {
	Name        string
	Code        string
	CodeSystem  string
	DiagnosedAt *time.Time
}

type MedicationSummary struct {
	DrugName   string
	DoseAmount float64
	DoseUnit   string
	Frequency  string
	StartedAt  *time.Time
}

type AllergySummary struct {
	Substance string
	Reaction  string
	Severity  string
}

type ProcedureSummary struct {
	Name        string
	PerformedAt *time.Time
}

type LabResultSummary struct {
	IndicatorName    string
	Value            float64
	QualitativeValue string
	Unit             string
	TakenAt          *time.Time
}

type VitalSignSummary struct {
	Type       string
	Systolic   float64
	Diastolic  float64
	Value      float64
	Unit       string
	MeasuredAt *time.Time
}

// MedicationRepository is the narrow slice of storage.MedicationRepository
// Builder needs — reuses storage.MedicationFilter directly (rather than a
// package-local equivalent) so storage.NewMedicationRepository(s) satisfies
// this interface with no adapter.
type MedicationRepository interface {
	ListByUser(ctx context.Context, userID string, filter storage.MedicationFilter) ([]normalization.Medication, error)
}

type DiagnosisRepository interface {
	ListByUser(ctx context.Context, userID string) ([]normalization.Diagnosis, error)
}

type ProcedureRepository interface {
	ListVaccinations(ctx context.Context, userID string) ([]normalization.Procedure, error)
}

type AllergyRepository interface {
	ListByUser(ctx context.Context, userID string) ([]normalization.Allergy, error)
}

type LabResultRepository interface {
	LatestByIndicator(ctx context.Context, userID string) (map[string]normalization.LabResult, error)
}

type VitalSignRepository interface {
	LatestByType(ctx context.Context, userID string) (map[string]normalization.VitalSign, error)
}

// Builder builds a Profile by reading every repository for one user. now is
// injected (rather than calling time.Now() internally) so tests get a
// deterministic RebuiltAt.
type Builder struct {
	medications MedicationRepository
	diagnoses   DiagnosisRepository
	procedures  ProcedureRepository
	allergies   AllergyRepository
	labResults  LabResultRepository
	vitalSigns  VitalSignRepository
	now         func() time.Time
}

// NewBuilder builds a Builder over the given repositories.
func NewBuilder(
	medications MedicationRepository,
	diagnoses DiagnosisRepository,
	procedures ProcedureRepository,
	allergies AllergyRepository,
	labResults LabResultRepository,
	vitalSigns VitalSignRepository,
) *Builder {
	return &Builder{
		medications: medications, diagnoses: diagnoses, procedures: procedures,
		allergies: allergies, labResults: labResults, vitalSigns: vitalSigns,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Build reads every relevant entity for userID and returns the aggregated
// Profile. Per docs/domain/05-medical-profile.md §4, a user with no READY
// documents yet gets an all-empty Profile, not an error — every repository
// call below naturally returns an empty slice/map in that case, so no
// special-casing is needed here.
func (b *Builder) Build(ctx context.Context, userID string) (Profile, error) {
	meds, err := b.medications.ListByUser(ctx, userID, storage.MedicationFilter{})
	if err != nil {
		return Profile{}, fmt.Errorf("profile: list medications: %w", err)
	}
	diagnoses, err := b.diagnoses.ListByUser(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: list diagnoses: %w", err)
	}
	vaccinations, err := b.procedures.ListVaccinations(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: list vaccinations: %w", err)
	}
	allergies, err := b.allergies.ListByUser(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: list allergies: %w", err)
	}
	latestLabs, err := b.labResults.LatestByIndicator(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: latest lab results: %w", err)
	}
	latestVitals, err := b.vitalSigns.LatestByType(ctx, userID)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: latest vital signs: %w", err)
	}

	activeDiagnoses, chronic := resolveActiveDiagnoses(diagnoses)
	activeMedications := resolveActiveMedications(meds)

	return Profile{
		UserID:            userID,
		ActiveDiagnoses:   activeDiagnoses,
		ChronicConditions: chronic,
		ActiveMedications: activeMedications,
		Allergies:         dedupAllergies(allergies),
		Vaccinations:      toProcedureSummaries(vaccinations),
		LatestLabResults:  toLabResultSummaries(latestLabs),
		LatestVitalSigns:  toVitalSignSummaries(latestVitals),
		RebuiltAt:         b.now(),
	}, nil
}

// groupKey canonicalizes a name for cross-document grouping — case/whitespace
// only, the same modest scope as loinc.go's lookup and for the same reason
// (see internal/normalization's canonicalize doc comment: real name
// canonicalization, e.g. matching brand and generic drug names, is a still-
// open problem, not attempted here).
func groupKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// resolveActiveMedications is the Medication Resolver: groups by
// canonicalized drug name, keeps the entity with the latest date per group
// (StartedAt, falling back to EndedAt when a discontinued/completed
// Medication has no StartedAt), and includes it only if that latest
// snapshot's Status is "active" — an earlier document saying "active" for a
// drug a later document says was discontinued must not still show up here.
func resolveActiveMedications(meds []normalization.Medication) []MedicationSummary {
	type candidate struct {
		med     normalization.Medication
		date    time.Time
		hasDate bool
	}
	groups := make(map[string]candidate)
	for _, m := range meds {
		key := groupKey(m.DrugName)
		date, hasDate := medicationDate(m)
		current, exists := groups[key]
		if !exists || (hasDate && (!current.hasDate || date.After(current.date))) {
			groups[key] = candidate{med: m, date: date, hasDate: hasDate}
		}
	}

	var result []MedicationSummary
	for _, c := range groups {
		if c.med.Status != "active" {
			continue
		}
		result = append(result, MedicationSummary{
			DrugName: c.med.DrugName, DoseAmount: c.med.DoseAmount, DoseUnit: c.med.DoseUnit,
			Frequency: c.med.Frequency, StartedAt: c.med.StartedAt,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DrugName < result[j].DrugName })
	return result
}

func medicationDate(m normalization.Medication) (time.Time, bool) {
	if m.StartedAt != nil {
		return *m.StartedAt, true
	}
	if m.EndedAt != nil {
		return *m.EndedAt, true
	}
	return time.Time{}, false
}

// resolveActiveDiagnoses is the Diagnosis Resolver: groups by code when
// present (more reliable than free-text name matching across documents),
// otherwise by canonicalized name, keeps the latest by DiagnosedAt, and
// splits the result into activeDiagnoses (status active or chronic, per
// docs/domain/05-medical-profile.md §2) and its chronic-only subset.
func resolveActiveDiagnoses(diagnoses []normalization.Diagnosis) (active, chronic []DiagnosisSummary) {
	type candidate struct {
		dx      normalization.Diagnosis
		date    time.Time
		hasDate bool
	}
	groups := make(map[string]candidate)
	for _, d := range diagnoses {
		key := d.Code
		if key == "" {
			key = groupKey(d.Name)
		} else {
			key = d.CodeSystem + ":" + key
		}
		date, hasDate := time.Time{}, false
		if d.DiagnosedAt != nil {
			date, hasDate = *d.DiagnosedAt, true
		}
		current, exists := groups[key]
		if !exists || (hasDate && (!current.hasDate || date.After(current.date))) {
			groups[key] = candidate{dx: d, date: date, hasDate: hasDate}
		}
	}

	var activeList, chronicList []DiagnosisSummary
	for _, c := range groups {
		if c.dx.Status != "active" && c.dx.Status != "chronic" {
			continue
		}
		summary := DiagnosisSummary{Name: c.dx.Name, Code: c.dx.Code, CodeSystem: c.dx.CodeSystem, DiagnosedAt: c.dx.DiagnosedAt}
		activeList = append(activeList, summary)
		if c.dx.Status == "chronic" {
			chronicList = append(chronicList, summary)
		}
	}
	sort.Slice(activeList, func(i, j int) bool { return activeList[i].Name < activeList[j].Name })
	sort.Slice(chronicList, func(i, j int) bool { return chronicList[i].Name < chronicList[j].Name })
	return activeList, chronicList
}

// dedupAllergies collapses repeated mentions of the same substance across
// documents into one entry — Allergy has no cross-document link either, so
// without this every document repeating "Penicillin" would duplicate the
// profile entry. Keeps whichever mention has the most detail (a non-empty
// Reaction), since later mentions aren't inherently more authoritative here
// (see docs/domain/07-diagnosis-and-allergy.md §3: no status, no
// "supersedes" concept).
func dedupAllergies(allergies []normalization.Allergy) []AllergySummary {
	seen := make(map[string]AllergySummary)
	var order []string
	for _, a := range allergies {
		key := groupKey(a.Substance)
		existing, ok := seen[key]
		if !ok {
			seen[key] = AllergySummary{Substance: a.Substance, Reaction: a.Reaction, Severity: a.Severity}
			order = append(order, key)
			continue
		}
		if existing.Reaction == "" && a.Reaction != "" {
			seen[key] = AllergySummary{Substance: a.Substance, Reaction: a.Reaction, Severity: a.Severity}
		}
	}
	result := make([]AllergySummary, 0, len(order))
	for _, key := range order {
		result = append(result, seen[key])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Substance < result[j].Substance })
	return result
}

func toProcedureSummaries(procedures []normalization.Procedure) []ProcedureSummary {
	result := make([]ProcedureSummary, len(procedures))
	for i, p := range procedures {
		result[i] = ProcedureSummary{Name: p.Name, PerformedAt: p.PerformedAt}
	}
	return result
}

func toLabResultSummaries(latest map[string]normalization.LabResult) []LabResultSummary {
	result := make([]LabResultSummary, 0, len(latest))
	for _, l := range latest {
		result = append(result, LabResultSummary{IndicatorName: l.IndicatorName, Value: l.Value, QualitativeValue: l.QualitativeValue, Unit: l.Unit, TakenAt: l.TakenAt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IndicatorName < result[j].IndicatorName })
	return result
}

// Store wraps storage.ProfileRepository to work with typed Profile values —
// see storage.ProfileRepository's doc comment for why the storage layer
// itself only sees json.RawMessage (avoiding an import cycle back to this
// package).
type Store struct {
	repo storage.ProfileRepository
}

// NewStore builds a Store over repo.
func NewStore(repo storage.ProfileRepository) *Store {
	return &Store{repo: repo}
}

// Get returns userID's stored Profile, or found=false if none has been
// built yet.
func (s *Store) Get(ctx context.Context, userID string) (Profile, bool, error) {
	data, rebuiltAt, found, err := s.repo.Get(ctx, userID)
	if err != nil {
		return Profile{}, false, fmt.Errorf("profile: get: %w", err)
	}
	if !found {
		return Profile{}, false, nil
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, false, fmt.Errorf("profile: unmarshal stored profile: %w", err)
	}
	p.RebuiltAt = rebuiltAt
	return p, true, nil
}

// Replace atomically overwrites userID's stored Profile.
func (s *Store) Replace(ctx context.Context, p Profile) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("profile: marshal: %w", err)
	}
	if err := s.repo.Replace(ctx, p.UserID, data, p.RebuiltAt); err != nil {
		return fmt.Errorf("profile: replace: %w", err)
	}
	return nil
}

func toVitalSignSummaries(latest map[string]normalization.VitalSign) []VitalSignSummary {
	result := make([]VitalSignSummary, 0, len(latest))
	for _, v := range latest {
		result = append(result, VitalSignSummary{Type: v.Type, Systolic: v.Systolic, Diastolic: v.Diastolic, Value: v.Value, Unit: v.Unit, MeasuredAt: v.MeasuredAt})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}
