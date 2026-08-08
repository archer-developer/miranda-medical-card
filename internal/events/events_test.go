package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/events"
)

func TestExtract_SymptomWithMedicationIntake(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"Приступ головной боли","medicationIntake":{"drugName":"ибупрофен","doseAmount":400,"doseUnit":"mg","reason":"головная боль"}}`),
	})

	result, err := events.Extract(context.Background(), provider, "Приступ головной боли, принял 400 мг ибупрофена")
	require.NoError(t, err)
	require.Equal(t, "symptom", result.Category)
	require.NotNil(t, result.MedicationIntake)
	require.Equal(t, "ибупрофен", result.MedicationIntake.DrugName)
	require.Equal(t, 400.0, result.MedicationIntake.DoseAmount)
}

func TestExtract_NoMedicationOmitsIntake(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"observation","description":"Давление 145/95"}`),
	})

	result, err := events.Extract(context.Background(), provider, "Давление сегодня 145/95")
	require.NoError(t, err)
	require.Nil(t, result.MedicationIntake)
}

func TestExtract_ProviderErrorPropagates(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{Err: errors.New("boom")})

	_, err := events.Extract(context.Background(), provider, "some text")
	require.Error(t, err)
}
