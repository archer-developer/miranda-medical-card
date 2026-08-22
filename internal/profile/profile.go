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
	"errors"
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
	// Overdue mirrors normalization.Diagnosis.Overdue(now) as of this
	// Profile's RebuiltAt — always false for a chronic condition (see
	// ChronicConditions) or an active diagnosis with no expected-resolution
	// estimate.
	Overdue bool
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

// LabResultSummary's DocumentTitle is the source MedicalDocument's Title
// (e.g. "Общий анализ крови", "Общий анализ мочи — Инвитро") — without it,
// two same-named indicators from different panel types (protein in a blood
// panel vs. a urinalysis, say) render identically in a Profile chunk with
// no way to tell them apart. Empty when the source document has none (an
// older document predating docs/domain/03-files-and-documents.md's
// studyTitle field, or the fixed documentType label alone) — never
// fabricated.
type LabResultSummary struct {
	IndicatorName    string
	Value            float64
	QualitativeValue string
	Unit             string
	TakenAt          *time.Time
	DocumentTitle    string
}

// VitalSignSummary.DocumentTitle mirrors LabResultSummary.DocumentTitle —
// same reasoning, same source.
type VitalSignSummary struct {
	Type          string
	Systolic      float64
	Diastolic     float64
	Value         float64
	Unit          string
	MeasuredAt    *time.Time
	DocumentTitle string
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

// DocumentRepository is the narrow slice of storage.DocumentRepository
// Builder needs — just enough to resolve a LabResult/VitalSign's
// DocumentID into its source document's Title for
// LabResultSummary.DocumentTitle/VitalSignSummary.DocumentTitle.
type DocumentRepository interface {
	Get(ctx context.Context, id, userID string) (storage.MedicalDocument, error)
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
	documents   DocumentRepository
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
	documents DocumentRepository,
) *Builder {
	return &Builder{
		medications: medications, diagnoses: diagnoses, procedures: procedures,
		allergies: allergies, labResults: labResults, vitalSigns: vitalSigns,
		documents: documents,
		now:       func() time.Time { return time.Now().UTC() },
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

	activeDiagnoses, chronic := resolveActiveDiagnoses(diagnoses, b.now())
	activeMedications := resolveActiveMedications(meds)

	labSummaries, err := b.toLabResultSummaries(ctx, userID, latestLabs)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: resolve lab result source documents: %w", err)
	}
	vitalSummaries, err := b.toVitalSignSummaries(ctx, userID, latestVitals)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: resolve vital sign source documents: %w", err)
	}

	return Profile{
		UserID:            userID,
		ActiveDiagnoses:   activeDiagnoses,
		ChronicConditions: chronic,
		ActiveMedications: activeMedications,
		Allergies:         dedupAllergies(allergies),
		Vaccinations:      toProcedureSummaries(vaccinations),
		LatestLabResults:  labSummaries,
		LatestVitalSigns:  vitalSummaries,
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

// resolveActiveMedications is the Medication Resolver's Profile-facing view:
// ResolveActiveMedications's full entities, mapped down to the summary shape
// medical.profile actually returns.
func resolveActiveMedications(meds []normalization.Medication) []MedicationSummary {
	active := ResolveActiveMedications(meds)
	result := make([]MedicationSummary, len(active))
	for i, m := range active {
		result[i] = MedicationSummary{
			DrugName: m.DrugName, DoseAmount: m.DoseAmount, DoseUnit: m.DoseUnit,
			Frequency: m.Frequency, StartedAt: m.StartedAt,
		}
	}
	return result
}

// ResolveLatestMedications is the Medication Resolver's core grouping
// (docs/domain/01-overview.md §7): groups by canonicalized drug name and
// keeps the entity with the latest date per group (StartedAt, falling back
// to EndedAt when a discontinued/completed Medication has no StartedAt),
// regardless of that entity's Status — one row per drug, whichever
// document's account of it is most recent. Exported (full entities, not
// MedicationSummary) so internal/pipeline's CompleteMedication/StartMedication
// can each filter this same "what's the current word on this drug" set down
// to their own status of interest (active, prescribed), rather than
// duplicating this grouping logic against a raw storage.MedicationFilter{Status:
// ...} listing (which alone can't tell a superseded row from the real
// latest one — a drug can have an old "active" row a later document already
// discontinued, or an old "prescribed" row a later document already
// confirmed started).
func ResolveLatestMedications(meds []normalization.Medication) []normalization.Medication {
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

	result := make([]normalization.Medication, 0, len(groups))
	for _, c := range groups {
		result = append(result, c.med)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DrugName < result[j].DrugName })
	return result
}

// ResolveActiveMedications is ResolveLatestMedications narrowed to drugs
// whose latest snapshot's Status is "active" — an earlier document saying
// "active" for a drug a later document says was discontinued must not still
// show up here.
func ResolveActiveMedications(meds []normalization.Medication) []normalization.Medication {
	latest := ResolveLatestMedications(meds)
	result := make([]normalization.Medication, 0, len(latest))
	for _, m := range latest {
		if m.Status == normalization.MedicationStatusActive {
			result = append(result, m)
		}
	}
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
// docs/domain/05-medical-profile.md §2) and its chronic-only subset. now is
// the Profile's own RebuiltAt, threaded through to each summary's Overdue
// (normalization.Diagnosis.Overdue) rather than each summary computing its
// own "current time" — a Profile's Overdue flags must all be consistent
// with the single instant it claims to be a snapshot of.
func resolveActiveDiagnoses(diagnoses []normalization.Diagnosis, now time.Time) (active, chronic []DiagnosisSummary) {
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
		summary := DiagnosisSummary{
			Name: c.dx.Name, Code: c.dx.Code, CodeSystem: c.dx.CodeSystem, DiagnosedAt: c.dx.DiagnosedAt,
			Overdue: c.dx.Overdue(now),
		}
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

func (b *Builder) toLabResultSummaries(ctx context.Context, userID string, latest map[string]normalization.LabResult) ([]LabResultSummary, error) {
	titles := make(map[string]string)
	result := make([]LabResultSummary, 0, len(latest))
	for _, l := range latest {
		title, err := b.documentTitle(ctx, userID, l.DocumentID, titles)
		if err != nil {
			return nil, err
		}
		result = append(result, LabResultSummary{
			IndicatorName: l.IndicatorName, Value: l.Value, QualitativeValue: l.QualitativeValue,
			Unit: l.Unit, TakenAt: l.TakenAt, DocumentTitle: title,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IndicatorName < result[j].IndicatorName })
	return result, nil
}

// documentTitle resolves documentID to its source MedicalDocument's Title,
// caching within one Build call — LatestByIndicator/LatestByType can return
// several entries pointing at the same document (e.g. every indicator on
// one blood panel), and each shouldn't re-fetch it. A document that's
// vanished (or documentID being empty, e.g. in an old test fixture) yields
// "" rather than an error — a missing title is a rendering gap, not reason
// to fail the whole Profile rebuild.
func (b *Builder) documentTitle(ctx context.Context, userID, documentID string, cache map[string]string) (string, error) {
	if documentID == "" {
		return "", nil
	}
	if title, ok := cache[documentID]; ok {
		return title, nil
	}
	doc, err := b.documents.Get(ctx, documentID, userID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			cache[documentID] = ""
			return "", nil
		}
		return "", fmt.Errorf("profile: get document %s: %w", documentID, err)
	}
	cache[documentID] = doc.Title
	return doc.Title, nil
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

func (b *Builder) toVitalSignSummaries(ctx context.Context, userID string, latest map[string]normalization.VitalSign) ([]VitalSignSummary, error) {
	titles := make(map[string]string)
	result := make([]VitalSignSummary, 0, len(latest))
	for _, v := range latest {
		title, err := b.documentTitle(ctx, userID, v.DocumentID, titles)
		if err != nil {
			return nil, err
		}
		result = append(result, VitalSignSummary{
			Type: v.Type, Systolic: v.Systolic, Diastolic: v.Diastolic,
			Value: v.Value, Unit: v.Unit, MeasuredAt: v.MeasuredAt, DocumentTitle: title,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result, nil
}
