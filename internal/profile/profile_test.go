package profile_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustDate(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestBuilder_EmptyForUserWithNoData(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)

	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, p.ActiveDiagnoses)
	require.Empty(t, p.ActiveMedications)
	require.Empty(t, p.Allergies)
	require.False(t, p.RebuiltAt.IsZero())
}

func TestBuilder_MedicationResolver_LaterDocumentWins(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	meds := storage.NewMedicationRepository(s)
	require.NoError(t, meds.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1",
		DrugName: "Розувастатин", Status: "active", StartedAt: mustDate("2025-05-14"),
	}))
	require.NoError(t, meds.Add(ctx, normalization.Medication{
		ID: "med_2", UserID: "user1", DocumentID: "doc2",
		DrugName: "розувастатин", Status: "discontinued", EndedAt: mustDate("2026-01-01"),
	}))

	b := profile.NewBuilder(
		meds, storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Empty(t, p.ActiveMedications, "the later document discontinued it — case/whitespace-insensitive grouping must treat both rows as the same drug")
}

func TestBuilder_MedicationResolver_StillActiveIncluded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	meds := storage.NewMedicationRepository(s)
	require.NoError(t, meds.Add(ctx, normalization.Medication{
		ID: "med_1", UserID: "user1", DocumentID: "doc1",
		DrugName: "Розувастатин", DoseAmount: 10, DoseUnit: "mg", Status: "active", StartedAt: mustDate("2025-05-14"),
	}))

	b := profile.NewBuilder(
		meds, storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.ActiveMedications, 1)
	require.Equal(t, "Розувастатин", p.ActiveMedications[0].DrugName)
	require.Equal(t, 10.0, p.ActiveMedications[0].DoseAmount)
}

func TestBuilder_DiagnosisResolver_GroupsByCodeAndSplitsChronic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	dx := storage.NewDiagnosisRepository(s)
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_1", UserID: "user1", DocumentID: "doc1",
		Name: "Артериальная гипертензия", Code: "I10", CodeSystem: "icd10", Status: "suspected", DiagnosedAt: mustDate("2024-01-01"),
	}))
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_2", UserID: "user1", DocumentID: "doc2",
		Name: "Гипертоническая болезнь", Code: "I10", CodeSystem: "icd10", Status: "chronic", DiagnosedAt: mustDate("2025-06-01"),
	}))
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_3", UserID: "user1", DocumentID: "doc3",
		Name: "Резолвед", Status: "resolved", DiagnosedAt: mustDate("2026-01-01"),
	}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), dx, storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.ActiveDiagnoses, 1, "same ICD-10 code across two documents must resolve to one entry, and resolved status must be excluded")
	require.Equal(t, "Гипертоническая болезнь", p.ActiveDiagnoses[0].Name, "the later (2025-06-01, chronic) mention wins over the earlier (2024-01-01, suspected) one")
	require.Len(t, p.ChronicConditions, 1)
}

func TestBuilder_DiagnosisResolver_OverdueFlagsPastExpectedResolution(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	dx := storage.NewDiagnosisRepository(s)
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_1", UserID: "user1", DocumentID: "doc1",
		Name: "ОРВИ (просрочен)", Status: "active", ExpectedResolutionTo: mustDate("2020-01-01"),
	}))
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_2", UserID: "user1", DocumentID: "doc2",
		Name: "ОРВИ (в сроке)", Status: "active", ExpectedResolutionTo: mustDate("2099-01-01"),
	}))
	require.NoError(t, dx.Add(ctx, normalization.Diagnosis{
		ID: "dx_3", UserID: "user1", DocumentID: "doc3",
		Name: "Без оценки срока", Status: "active",
	}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), dx, storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.ActiveDiagnoses, 3)

	byName := map[string]profile.DiagnosisSummary{}
	for _, d := range p.ActiveDiagnoses {
		byName[d.Name] = d
	}
	require.True(t, byName["ОРВИ (просрочен)"].Overdue)
	require.False(t, byName["ОРВИ (в сроке)"].Overdue)
	require.False(t, byName["Без оценки срока"].Overdue)
}

func TestBuilder_AllergiesDeduped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	allergies := storage.NewAllergyRepository(s)
	require.NoError(t, allergies.Add(ctx, normalization.Allergy{ID: "a1", UserID: "user1", DocumentID: "doc1", Substance: "Пенициллин"}))
	require.NoError(t, allergies.Add(ctx, normalization.Allergy{ID: "a2", UserID: "user1", DocumentID: "doc2", Substance: "пенициллин", Reaction: "Rash"}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		allergies, storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.Allergies, 1)
	require.Equal(t, "Rash", p.Allergies[0].Reaction, "the more detailed mention should be kept")
}

func TestBuilder_VaccinationsAllIncludedNotDeduped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	procedures := storage.NewProcedureRepository(s)
	require.NoError(t, procedures.Add(ctx, normalization.Procedure{ID: "p1", UserID: "user1", DocumentID: "doc1", Type: "vaccination", Name: "Грипп 2025", PerformedAt: mustDate("2025-10-01")}))
	require.NoError(t, procedures.Add(ctx, normalization.Procedure{ID: "p2", UserID: "user1", DocumentID: "doc2", Type: "vaccination", Name: "Грипп 2026", PerformedAt: mustDate("2026-10-01")}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), storage.NewDiagnosisRepository(s), procedures,
		storage.NewAllergyRepository(s), storage.NewLabResultRepository(s), storage.NewVitalSignRepository(s), storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.Vaccinations, 2, "repeat vaccinations are all legitimate separate events, unlike medications/diagnoses/allergies")
}

func TestBuilder_LatestLabResultsAndVitalSigns(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	labs := storage.NewLabResultRepository(s)
	require.NoError(t, labs.Add(ctx, normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: "doc1", IndicatorName: "ALT", Value: 28, TakenAt: mustDate("2025-01-01")}))
	require.NoError(t, labs.Add(ctx, normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: "doc2", IndicatorName: "ALT", Value: 54.7, TakenAt: mustDate("2026-06-12")}))
	vitals := storage.NewVitalSignRepository(s)
	require.NoError(t, vitals.Add(ctx, normalization.VitalSign{ID: "v1", UserID: "user1", DocumentID: "doc1", Type: "weight", Value: 80, MeasuredAt: mustDate("2025-01-01")}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), labs, vitals, storage.NewDocumentRepository(s),
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.LatestLabResults, 1)
	require.Equal(t, 54.7, p.LatestLabResults[0].Value)
	require.Len(t, p.LatestVitalSigns, 1)
	require.Equal(t, 80.0, p.LatestVitalSigns[0].Value)
}

func TestBuilder_LatestLabResultsCarryDistinguishingDocumentTitle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	documents := storage.NewDocumentRepository(s)
	bloodDoc, err := documents.Add(ctx, storage.MedicalDocument{ID: "doc1", UserID: "user1", Title: "Общий анализ крови"})
	require.NoError(t, err)
	urineDoc, err := documents.Add(ctx, storage.MedicalDocument{ID: "doc2", UserID: "user1", Title: "Общий анализ мочи"})
	require.NoError(t, err)

	labs := storage.NewLabResultRepository(s)
	require.NoError(t, labs.Add(ctx, normalization.LabResult{ID: "l1", UserID: "user1", DocumentID: bloodDoc.ID, IndicatorName: "Белок", Value: 72, Unit: "г/л", TakenAt: mustDate("2026-05-08")}))
	require.NoError(t, labs.Add(ctx, normalization.LabResult{ID: "l2", UserID: "user1", DocumentID: urineDoc.ID, IndicatorName: "Белок в моче", Value: 0, Unit: "г/л", TakenAt: mustDate("2026-05-08")}))

	b := profile.NewBuilder(
		storage.NewMedicationRepository(s), storage.NewDiagnosisRepository(s), storage.NewProcedureRepository(s),
		storage.NewAllergyRepository(s), labs, storage.NewVitalSignRepository(s), documents,
	)
	p, err := b.Build(ctx, "user1")
	require.NoError(t, err)
	require.Len(t, p.LatestLabResults, 2)

	titles := make(map[string]string)
	for _, l := range p.LatestLabResults {
		titles[l.IndicatorName] = l.DocumentTitle
	}
	require.Equal(t, "Общий анализ крови", titles["Белок"], "a lab result's DocumentTitle must identify which panel it came from, so two same-shaped indicators from different document types aren't indistinguishable downstream")
	require.Equal(t, "Общий анализ мочи", titles["Белок в моче"])
}

func TestStore_ReplaceThenGetRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	store := profile.NewStore(storage.NewProfileRepository(s))

	p := profile.Profile{
		UserID:            "user1",
		ActiveMedications: []profile.MedicationSummary{{DrugName: "Розувастатин", DoseAmount: 10, DoseUnit: "mg"}},
		RebuiltAt:         time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.Replace(ctx, p))

	got, found, err := store.Get(ctx, "user1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "user1", got.UserID)
	require.Len(t, got.ActiveMedications, 1)
	require.Equal(t, "Розувастатин", got.ActiveMedications[0].DrugName)
	require.True(t, p.RebuiltAt.Equal(got.RebuiltAt))
}

func TestStore_GetNotFoundForUnbuiltProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	store := profile.NewStore(storage.NewProfileRepository(s))

	_, found, err := store.Get(ctx, "user1")
	require.NoError(t, err)
	require.False(t, found)
}
