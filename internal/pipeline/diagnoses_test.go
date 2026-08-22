package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestResolveDiagnosis_HappyPath(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	dxRepo := storage.NewDiagnosisRepository(s)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Хронический тонзиллит", Status: "chronic",
	}))
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_doc1_1", UserID: "user1", DocumentID: "doc1", Name: "ОРВИ", Status: "active",
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"matchId":"dx_doc1_1"}`),
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	resolved, err := p.ResolveDiagnosis(ctx, "user1", "да, ОРВИ прошла")
	require.NoError(t, err)
	require.Equal(t, "dx_doc1_1", resolved.ID)
	require.Equal(t, "resolved", resolved.Status)
	require.NotNil(t, resolved.ActualResolutionAt)
	require.NotEmpty(t, resolved.StatusReasoning)

	all, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	for _, d := range all {
		if d.ID == "dx_doc1_1" {
			require.Equal(t, "resolved", d.Status)
		} else {
			require.Equal(t, "chronic", d.Status, "the other diagnosis must be untouched")
		}
	}
}

func TestResolveDiagnosis_NoNonResolvedDiagnosesAtAll(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.ResolveDiagnosis(ctx, "user1", "да, всё прошло")
	require.Error(t, err)
	var notFound *pipeline.DiagnosisNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentNames)
}

func TestResolveDiagnosis_AlreadyResolvedDiagnosesAreNeverCandidates(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	dxRepo := storage.NewDiagnosisRepository(s)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Серная пробка", Status: "resolved",
	}))
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.ResolveDiagnosis(ctx, "user1", "да, всё прошло")
	require.Error(t, err)
	var notFound *pipeline.DiagnosisNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Empty(t, notFound.CurrentNames, "an already-resolved diagnosis must never be offered as a candidate")
}

func TestResolveDiagnosis_NoConfidentMatchListsCurrentNames(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	dxRepo := storage.NewDiagnosisRepository(s)
	require.NoError(t, dxRepo.Add(ctx, normalization.Diagnosis{
		ID: "dx_doc1_0", UserID: "user1", DocumentID: "doc1", Name: "Артериальная гипертензия", Status: "chronic",
	}))

	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{}`), // model found no confident match
	})
	p := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.ResolveDiagnosis(ctx, "user1", "что-то непонятное прошло")
	require.Error(t, err)
	var notFound *pipeline.DiagnosisNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, []string{"Артериальная гипертензия"}, notFound.CurrentNames)

	all, err := dxRepo.ListByUser(ctx, "user1")
	require.NoError(t, err)
	require.Equal(t, "chronic", all[0].Status, "no match must leave the diagnosis untouched")
}

func TestResolveDiagnosis_EmptyTextRejected(t *testing.T) {
	ctx := context.Background()
	s, fs := newTestBackend(t)
	p := pipeline.New(llmtest.New("fake"), nil, llmtest.New("fake"), nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	_, err := p.ResolveDiagnosis(ctx, "user1", "   ")
	require.Error(t, err)
	var notFound *pipeline.DiagnosisNotFoundError
	require.NotErrorAs(t, err, &notFound, "empty text must be its own plain error, not DiagnosisNotFoundError")
}
