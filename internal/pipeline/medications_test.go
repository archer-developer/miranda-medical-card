package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func mustDate(s string) *time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &d
}

func TestCompleteMedication_HappyPath(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Аспирин", Status: normalization.MedicationStatusDiscontinued,
	}))
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_1", UserID: "user1", DocumentID: "doc1", DrugName: "Амоксициллин", Status: normalization.MedicationStatusActive,
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"matchId":"med_doc1_1"}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	completed, err := p.CompleteMedication(ctx, "user1", "я закончил принимать антибиотик")
	require.NoError(t, err)
	require.Equal(t, "med_doc1_1", completed.ID)
	require.Equal(t, normalization.MedicationStatusCompleted, completed.Status)
	require.NotNil(t, completed.EndedAt)

	all, err := medRepo.ListByUser(ctx, "user1", storage.MedicationFilter{})
	require.NoError(t, err)
	for _, m := range all {
		if m.ID == "med_doc1_1" {
			require.Equal(t, normalization.MedicationStatusCompleted, m.Status)
			require.NotNil(t, m.EndedAt)
		} else {
			require.Equal(t, normalization.MedicationStatusDiscontinued, m.Status, "the other medication must be untouched")
		}
	}
}

func TestCompleteMedication_NoActiveMedicationsAtAll(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.CompleteMedication(ctx, "user1", "я закончил курс")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentDrugNames)
}

func TestCompleteMedication_SupersededActiveRowIsNeverACandidate(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	// An older document said it was active; a newer one already discontinued
	// it — the Medication Resolver must exclude this drug entirely, not
	// offer the stale "active" row as a candidate.
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Розувастатин",
		Status: normalization.MedicationStatusActive, StartedAt: mustDate("2025-05-14"),
	}))
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc2_0", UserID: "user1", DocumentID: "doc2", DrugName: "розувастатин",
		Status: normalization.MedicationStatusDiscontinued, EndedAt: mustDate("2026-01-01"),
	}))
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.CompleteMedication(ctx, "user1", "закончил розувастатин")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentDrugNames, "an already-discontinued drug must never be offered as a candidate")
}

func TestCompleteMedication_NoConfidentMatchListsCurrentDrugNames(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Периндоприл", Status: normalization.MedicationStatusActive,
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`), // model found no confident match
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.CompleteMedication(ctx, "user1", "что-то непонятное закончил")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, []string{"Периндоприл"}, notFound.CurrentDrugNames)

	all, err := medRepo.ListByUser(ctx, "user1", storage.MedicationFilter{})
	require.NoError(t, err)
	require.Equal(t, normalization.MedicationStatusActive, all[0].Status, "no match must leave the medication untouched")
}

func TestCompleteMedication_EmptyTextRejected(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.CompleteMedication(ctx, "user1", "   ")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.NotErrorAs(t, err, &notFound, "empty text must be its own plain error, not MedicationNotFoundError")
}

func TestStartMedication_HappyPath(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Аспирин", Status: normalization.MedicationStatusActive,
	}))
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_1", UserID: "user1", DocumentID: "doc1", DrugName: "Амоксициллин", Status: normalization.MedicationStatusPrescribed,
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"matchId":"med_doc1_1"}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	started, err := p.StartMedication(ctx, "user1", "я начал принимать амоксициллин")
	require.NoError(t, err)
	require.Equal(t, "med_doc1_1", started.ID)
	require.Equal(t, normalization.MedicationStatusActive, started.Status)
	require.NotNil(t, started.StartedAt)

	all, err := medRepo.ListByUser(ctx, "user1", storage.MedicationFilter{})
	require.NoError(t, err)
	for _, m := range all {
		if m.ID == "med_doc1_1" {
			require.Equal(t, normalization.MedicationStatusActive, m.Status)
			require.NotNil(t, m.StartedAt)
		} else {
			require.Equal(t, normalization.MedicationStatusActive, m.Status, "the other medication must be untouched")
		}
	}
}

func TestStartMedication_NoPrescribedMedicationsAtAll(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.StartMedication(ctx, "user1", "я начал курс")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentDrugNames)
}

func TestStartMedication_AlreadyActiveMedicationIsNeverACandidate(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Розувастатин", Status: normalization.MedicationStatusActive,
	}))
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.StartMedication(ctx, "user1", "начал розувастатин")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentDrugNames, "an already-active drug must never be offered as a candidate to start")
}

func TestStartMedication_NoConfidentMatchListsCurrentDrugNames(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	medRepo := storage.NewMedicationRepository(s)
	require.NoError(t, medRepo.Add(ctx, normalization.Medication{
		ID: "med_doc1_0", UserID: "user1", DocumentID: "doc1", DrugName: "Периндоприл", Status: normalization.MedicationStatusPrescribed,
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`), // model found no confident match
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.StartMedication(ctx, "user1", "что-то непонятное начал")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, []string{"Периндоприл"}, notFound.CurrentDrugNames)

	all, err := medRepo.ListByUser(ctx, "user1", storage.MedicationFilter{})
	require.NoError(t, err)
	require.Equal(t, normalization.MedicationStatusPrescribed, all[0].Status, "no match must leave the medication untouched")
}

func TestStartMedication_EmptyTextRejected(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.StartMedication(ctx, "user1", "   ")
	require.Error(t, err)
	var notFound *pipeline.MedicationNotFoundError
	require.NotErrorAs(t, err, &notFound, "empty text must be its own plain error, not MedicationNotFoundError")
}
